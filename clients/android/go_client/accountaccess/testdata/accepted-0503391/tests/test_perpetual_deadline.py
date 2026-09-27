#!/usr/bin/env python3
"""Indefinite subscription /me deadline consistency (S1-C01 correction).

Truthful NULL grant_resolution.effective_deadline is allowed ONLY for
data_access=subscription_data together with entitlement.perpetual_commercial=true and
entitlement.valid_until=null. Finite subscription and onboarding_hour keep a finite deadline.
Technical node grant lease / catalog / session validity are separate and stay finite.

The validator here uses the same RefResolver/registry as tests/test_contract.py. A naive
Draft202012Validator with a registry keyed only by $id silently ignores the nested relative
$ref constraints (false negatives), which is why an explicit "nested refs are applied" check is
included below.
"""

from __future__ import annotations

import json
import unittest
from pathlib import Path

import jsonschema
from jsonschema import RefResolver

ROOT = Path(__file__).resolve().parent.parent
SCHEMAS = ROOT / "schemas"


def _all_schemas() -> dict:
    store = {}
    for path in SCHEMAS.glob("*.json"):
        doc = json.loads(path.read_text())
        store[path.as_uri()] = doc
        store[path.name] = doc
        store[f"https://terlimo.local/schemas/{path.name}"] = doc
    return store


def resolver_for(schema: dict) -> jsonschema.RefResolver:
    return jsonschema.RefResolver(base_uri=SCHEMAS.as_uri() + "/", referrer=schema, store=_all_schemas())


def subscription_root() -> dict:
    return json.loads((SCHEMAS / "subscription.json").read_text())


def me_schema() -> dict:
    return subscription_root()["$defs"]["MeResponse"]


def me_body() -> dict:
    """Synthetic sanitized /me body (no secrets); finite subscription shape by default."""
    return {
        "request_id": "0" * 32,
        "server_time": "2026-09-22T15:40:00Z",
        "schema_version": "1.0",
        "status": "ok",
        "account_state": "ACTIVE_PAID",
        "account_ref": "00000000-0000-0000-0000-0000000000aa",
        "telegram_linked": True,
        "entitlement": {
            "type": "paid",
            "status": "active",
            "valid_from": "2026-09-22T15:33:00Z",
            "valid_until": "2026-09-22T16:40:00Z",
            "effective_device_limit": 2,
            "slots_used": 1,
            "revision": "1",
            "perpetual_commercial": False,
        },
        "binding_status": "active",
        "binding_revision": "1",
        "management_only": False,
        "onboarding": {
            "started_by": "server_confirmed_first_connection",
            "duration_seconds": 3600,
            "one_time": True,
            "extends_on_refresh": False,
            "extends_on_restart": False,
            "creates_trial": False,
            "requires_hardware_id": False,
            "unit": "installation_fingerprint",
            "post_telegram_identity": "account_history_correlation",
            "pre_telegram_reinstall": "may_be_indistinguishable_new_key_separate_unit",
            "state": "not_started",
            "started_at": None,
            "not_after": None,
        },
        "grant_resolution": {
            "control_available": True,
            "restricted_checkout_available": True,
            "data_access": "subscription_data",
            "effective_deadline": "2026-09-22T16:40:00Z",
        },
        "revision": "7",
    }


def indefinite_body() -> dict:
    b = me_body()
    b["entitlement"]["valid_until"] = None
    b["entitlement"]["perpetual_commercial"] = True
    b["grant_resolution"]["effective_deadline"] = None
    return b


def valid(body: dict) -> bool:
    root = subscription_root()
    try:
        jsonschema.validate(body, root["$defs"]["MeResponse"], resolver=resolver_for(root))
        return True
    except jsonschema.ValidationError:
        return False


class PerpetualDeadlineContract(unittest.TestCase):
    def test_positive_finite_subscription(self):
        self.assertTrue(valid(me_body()))

    def test_positive_indefinite_real_builder_fixture(self):
        # exact shape emitted by mobile_account._grant_resolution/_entitlement_snapshot when the
        # effective entitlement is paid/active with ends_at NULL.
        self.assertTrue(valid(indefinite_body()))

    def test_positive_onboarding_hour_with_finite_deadline(self):
        b = me_body()
        b["grant_resolution"]["data_access"] = "onboarding_hour"
        b["grant_resolution"]["effective_deadline"] = "2026-09-22T16:00:00Z"
        b["onboarding"].update(
            state="active",
            started_at="2026-09-22T15:00:00Z",
            not_after="2026-09-22T16:00:00Z",
        )
        self.assertTrue(valid(b))

    def test_positive_none_has_null_deadline(self):
        b = me_body()
        b["grant_resolution"]["data_access"] = "none"
        b["grant_resolution"]["effective_deadline"] = None
        self.assertTrue(valid(b))

    def test_negative_null_deadline_without_perpetual(self):
        b = me_body()
        b["grant_resolution"]["effective_deadline"] = None  # perpetual_commercial False
        self.assertFalse(valid(b))

    def test_negative_null_deadline_with_non_null_valid_until(self):
        b = me_body()
        b["entitlement"]["perpetual_commercial"] = True
        b["grant_resolution"]["effective_deadline"] = None  # valid_until still a string
        self.assertFalse(valid(b))

    def test_negative_onboarding_hour_null_deadline(self):
        b = me_body()
        b["grant_resolution"]["data_access"] = "onboarding_hour"
        b["grant_resolution"]["effective_deadline"] = None
        self.assertFalse(valid(b))

    def test_negative_finite_subscription_null_deadline(self):
        b = me_body()
        b["grant_resolution"]["effective_deadline"] = None
        b["entitlement"]["valid_until"] = None
        b["entitlement"]["perpetual_commercial"] = False
        self.assertFalse(valid(b))

    def test_validator_applies_nested_refs_not_silently_ignored(self):
        # A bogus data_access class must be rejected: proves the nested onboarding.json
        # GrantResolution $ref (and its allOf) is actually applied by this harness.
        b = me_body()
        b["grant_resolution"]["data_access"] = "bogus"
        b["grant_resolution"]["effective_deadline"] = 123
        root = subscription_root()
        try:
            jsonschema.validate(b, root["$defs"]["MeResponse"], resolver=resolver_for(root))
            self.fail("nested GrantResolution constraints were silently ignored")
        except jsonschema.ValidationError:
            pass


if __name__ == "__main__":
    unittest.main()
