"""Durable checkout receipts: policy records issued for an existing S5 pending order.

This module is storage + transactional helper only. It never creates an invoice, never calls
the payment provider and never credits access; it only records the bounded checkout policy the
backend decided to issue for an already provider-created order.
"""
from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any

import asyncpg

from .auth_api import ApiError


@dataclass(frozen=True)
class CheckoutPolicy:
    """Explicit policy value. Production must pass a verified configuration; there is no
    implicit or invented default. Synthetic fixtures only in tests."""

    version: str
    allowed_origins: tuple[str, ...]
    allowed_redirects: tuple[str, ...]
    ttl: timedelta


def _refuse(code: str, http: int) -> ApiError:
    return ApiError(code, http=http)


def _bounded_strings(value: Any, *, limit: int) -> bool:
    if not isinstance(value, (tuple, list)) or not value or len(value) > limit:
        return False
    return all(isinstance(item, str) and 0 < len(item) <= 2048 for item in value)


def _policy_usable(policy: CheckoutPolicy | None) -> bool:
    """Structural validation of the injected policy value only. Real provider origin/redirect
    verification against a merchant policy is a separate (still missing) configuration step."""
    if policy is None or not isinstance(policy, CheckoutPolicy):
        return False
    if not isinstance(policy.version, str) or not 0 < len(policy.version) <= 64:
        return False
    if not isinstance(policy.ttl, timedelta) or policy.ttl <= timedelta(0):
        return False
    return _bounded_strings(policy.allowed_origins, limit=32) and _bounded_strings(
        policy.allowed_redirects, limit=32
    )


async def issue_checkout_receipt(
    connection: asyncpg.Connection,
    *,
    order_id: Any,
    account_id: Any,
    installation_id: Any,
    idempotency_key: str,
    policy: CheckoutPolicy | None,
) -> dict[str, Any]:
    """Return the durable receipt for this order (same-key replay) or create it once.

    Fail-closed order of checks: disabled/unusable policy -> CHECKOUT_POLICY_DENIED 403;
    missing/foreign/historical-null owner -> neutral PAYMENT_NOT_FOUND 404; not a pending
    provider-created order with a pay_url -> PAYMENT_STATE_INVALID 409. The order row is locked
    FOR UPDATE, so competing calls serialize on one receipt; a different key conflicts with
    IDEMPOTENCY_CONFLICT 409 and an expired receipt or changed policy version never reissues.
    """
    key = (idempotency_key or "").strip()
    if not key:
        raise _refuse("BAD_MESSAGE", 400)
    if not _policy_usable(policy):
        # Production default is disabled; malformed injected values fail closed too.
        raise _refuse("CHECKOUT_POLICY_DENIED", 403)
    async with connection.transaction():
        order = await connection.fetchrow(
            """
            SELECT id, status, installation_id, checkout_owner_account_id,
                   provider_create_state, provider_payment_url
            FROM payment_orders WHERE id = $1 FOR UPDATE
            """,
            order_id,
        )
        if order is None:
            raise _refuse("PAYMENT_NOT_FOUND", 404)
        if (
            account_id is None
            or installation_id is None
            or order["checkout_owner_account_id"] is None
            or order["checkout_owner_account_id"] != account_id
            or order["installation_id"] != installation_id
        ):
            # Foreign, historical-null or incomplete owner: neutral 404 before any write,
            # never a NOT NULL violation and never inferred ownership.
            raise _refuse("PAYMENT_NOT_FOUND", 404)
        # State gates apply to replay as well: a terminal or provider-unconfirmed order must
        # never re-issue checkout, even for an already existing receipt.
        if (
            order["status"] != "pending"
            or order["provider_create_state"] != "created"
            or not order["provider_payment_url"]
        ):
            raise _refuse("PAYMENT_STATE_INVALID", 409)
        existing = await connection.fetchrow(
            "SELECT * FROM checkout_sessions WHERE order_id = $1", order_id
        )
        now = datetime.now(UTC)
        if existing is not None:
            if existing["idempotency_key"] != key:
                raise _refuse("IDEMPOTENCY_CONFLICT", 409)
            if existing["policy_version"] != policy.version or existing["expires_at"] <= now:
                raise _refuse("CHECKOUT_POLICY_DENIED", 403)
            return dict(existing)
        created = await connection.fetchrow(
            """
            INSERT INTO checkout_sessions
                (order_id, account_id, installation_id, idempotency_key, policy_version,
                 allowed_origins, allowed_redirects, expires_at)
            VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8)
            RETURNING *
            """,
            order_id,
            account_id,
            installation_id,
            key,
            policy.version,
            json.dumps(list(policy.allowed_origins)),
            json.dumps(list(policy.allowed_redirects)),
            now + policy.ttl,
        )
        return dict(created)
