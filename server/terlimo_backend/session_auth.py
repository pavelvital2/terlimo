"""Session-bearer authorization for mobile v1 routes.

One authorizer for every session route: bearer hash lookup, expiry/revocation/generation,
installation/environment binding, required scope and account state/ownership. A stored result
is never returned and no business action is taken before these checks pass. Foreign resources
are reported as a neutral 404 by the route layer.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any

import asyncpg

from .config import Settings


class AuthError(RuntimeError):
    def __init__(self, code: str, http: int) -> None:
        super().__init__(code)
        self.code = code
        self.http = http
        self.retryable = False


def sha256_hex_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def now_utc() -> datetime:
    return datetime.now(UTC)


@dataclass
class SessionContext:
    session_id: Any
    session_generation: int
    account_id: Any | None
    installation_id: Any
    installation_ref: str
    environment: str
    scopes: frozenset[str]
    account_state: str
    management_only: bool
    binding: asyncpg.Record | None
    binding_status: str
    entitlement: asyncpg.Record | None
    active_entitlement: asyncpg.Record | None
    onboarding_hour: asyncpg.Record | None
    slots_used: int
    evaluated_at: datetime
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def data_access_allowed(self) -> bool:
        return (
            not self.management_only
            and self.active_entitlement is not None
            and self.binding_status == "active"
            and not self.metadata.get("device_capacity_exceeded",False)
        )


def _account_state(
    account_id: Any | None,
    binding: asyncpg.Record | None,
    entitlement: asyncpg.Record | None,
    active_entitlement: asyncpg.Record | None,
    history_started: bool,
) -> str:
    if account_id is None:
        return "UNLINKED"
    if active_entitlement is not None:
        if binding is not None and binding["status"] == "active":
            return "ACTIVE_TRIAL" if active_entitlement["kind"] == "trial" else "ACTIVE_PAID"
        return "VERIFIED_NO_SLOT"
    if entitlement is not None and history_started:
        return "EXPIRED"
    # No right has ever started: a scheduled/future right is still no entitlement.
    return "VERIFIED_NO_ENTITLEMENT"


async def authenticate_session(
    connection: asyncpg.Connection,
    settings: Settings,
    raw_token: str,
    *,
    required_scope: str | None = None,
    now: datetime | None = None,
) -> SessionContext:
    # One effective timestamp per snapshot attempt so start/end boundary checks cannot
    # contradict each other inside a single response.
    now = now or now_utc()
    if not isinstance(raw_token, str) or not raw_token or len(raw_token) > 256:
        raise AuthError("SESSION_INVALID", 401)
    row = await connection.fetchrow(
        """
        SELECT session.id, session.account_id, session.installation_id, session.scopes,
               session.generation, session.expires_at, session.revoked_at,
               session.binding_id, session.binding_generation,
               installation.public_key_fingerprint, installation.environment,
               installation.state AS installation_state
        FROM sessions AS session
        JOIN installations AS installation ON installation.id = session.installation_id
        WHERE session.token_sha256 = $1
        """,
        sha256_hex_text(raw_token),
    )
    if row is None or row["revoked_at"] is not None:
        raise AuthError("SESSION_INVALID", 401)
    if row["expires_at"] <= now:
        raise AuthError("SESSION_EXPIRED", 401)
    if row["installation_state"] == "revoked":
        raise AuthError("DEVICE_REVOKED", 403)
    if row["environment"] != settings.environment:
        raise AuthError("SESSION_INVALID", 401)
    scopes = frozenset(row["scopes"] or [])
    if required_scope is not None and required_scope not in scopes:
        raise AuthError("ACCESS_DENIED", 403)

    binding = None
    entitlement = None
    active_entitlement = None
    onboarding_hour = None
    slots_used = 0
    history_started = False
    # The hour unit is the installation fingerprint and is resolved for linked and unlinked
    # installations alike; an hour with installation_id NULL stays undetermined.
    onboarding_hour = await connection.fetchrow(
        """
        SELECT * FROM entitlements
        WHERE kind = 'onboarding_hour' AND installation_id = $1
        ORDER BY created_at DESC
        LIMIT 1
        """,
        row["installation_id"],
    )
    if row["account_id"] is not None:
        # The session is bound to one exact binding identity+generation; a same-account
        # replacement or a generation bump must never inherit the old bearer's rights.
        if row["binding_id"] is None or row["binding_generation"] is None:
            raise AuthError("SESSION_INVALID", 401)
        binding = await connection.fetchrow(
            """
            SELECT id, status, generation
            FROM account_bindings
            WHERE id = $1 AND account_id = $2 AND installation_id = $3
            """,
            row["binding_id"],
            row["account_id"],
            row["installation_id"],
        )
        if binding is None or int(binding["generation"]) != int(row["binding_generation"]):
            raise AuthError("SESSION_INVALID", 401)
        # Commercial rights are selected separately from the onboarding hour. An effective
        # commercial right requires status=active, starts_at <= now < ends_at (or an allowed
        # indefinite end); a future start is not an active right.
        commercial = await connection.fetch(
            """
            SELECT *
            FROM entitlements
            WHERE account_id = $1 AND kind IN ('trial', 'paid', 'imported')
            ORDER BY created_at DESC
            """,
            row["account_id"],
        )
        effective = [
            item
            for item in commercial
            if item["status"] == "active"
            and (item["starts_at"] is None or item["starts_at"] <= now)
            and (item["ends_at"] is None or item["ends_at"] > now)
        ]
        history_started = any(
            item["status"] == "revoked"
            or item["starts_at"] is None
            or item["starts_at"] <= now
            for item in commercial
        )
        if effective:
            # Most recently created effective right wins; history stays untouched.
            active_entitlement = dict(effective[0])
            from .payment_products import paid_limit
            active_entitlement["device_limit"] = await paid_limit(connection,active_entitlement,now)
            from .payment_products import binding_paid_capacity
            capacity, _deadline = await binding_paid_capacity(connection,active_entitlement,binding["id"],now)
            active_entitlement["device_capacity_exceeded"] = not capacity
            entitlement = active_entitlement
        elif commercial:
            entitlement = commercial[0]
        # The onboarding hour unit is the installation fingerprint: only an hour explicitly
        # linked to this installation is visible. An hour with installation_id NULL is
        # undetermined and is never attributed by guessing.
        onboarding_hour = await connection.fetchrow(
            """
            SELECT * FROM entitlements
            WHERE kind = 'onboarding_hour' AND installation_id = $1
            ORDER BY created_at DESC
            LIMIT 1
            """,
            row["installation_id"],
        )
        slots_used = (
            await connection.fetchval(
                "SELECT count(*) FROM account_bindings WHERE account_id = $1 AND status = 'active'",
                row["account_id"],
            )
            or 0
        )
    elif row["binding_id"] is not None:
        raise AuthError("SESSION_INVALID", 401)

    return SessionContext(
        session_id=row["id"],
        session_generation=int(row["generation"]),
        account_id=row["account_id"],
        installation_id=row["installation_id"],
        installation_ref=row["public_key_fingerprint"],
        environment=row["environment"],
        scopes=scopes,
        account_state=_account_state(
            row["account_id"], binding, entitlement, active_entitlement, history_started
        ),
        management_only="management-only" in scopes,
        binding=binding,
        binding_status=(binding["status"] if binding is not None else "none"),
        entitlement=entitlement,
        active_entitlement=active_entitlement,
        onboarding_hour=onboarding_hour,
        slots_used=int(slots_used),
        evaluated_at=now,
        metadata={"device_capacity_exceeded":bool(active_entitlement and active_entitlement.get("device_capacity_exceeded",False))},
    )
