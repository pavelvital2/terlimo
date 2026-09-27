"""STEP03.4 server-side preparation: fixtures conform to the exact accepted C01 DTOs.

Validates the synthetic fixtures against the contract JSON Schemas (reused, not copied),
compares the endpoint mapping with the accepted machine-readable mapping and checks the
onboarding hour / last-good / native-coordination policy claims.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

CONTRACT_DIR = Path(os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts"))
FIXTURES = Path(__file__).resolve().parents[1] / "fixtures" / "step034"

if not (CONTRACT_DIR / "schemas" / "subscription.json").exists():
    pytest.skip("accepted contract schemas not available (set TERLIMO_CONTRACT_DIR)", allow_module_level=True)


def _load(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def _registry() -> Registry:
    resources = []
    for path in sorted((CONTRACT_DIR / "schemas").glob("*.json")):
        schema = _load(path)
        resources.append(
            (schema["$id"], Resource.from_contents(schema, default_specification=DRAFT202012))
        )
    return Registry().with_resources(resources)


REGISTRY = _registry()


def _validator(schema_file: str, def_name: str) -> Draft202012Validator:
    document = _load(CONTRACT_DIR / "schemas" / schema_file)
    # Keep the document base URI so relative refs (common.json, onboarding.json) resolve
    # through the shared registry, while validating the exact accepted $defs fragment.
    wrapper = {
        "$schema": document.get("$schema"),
        "$id": document["$id"],
        "$ref": f"#/$defs/{def_name}",
        "$defs": document["$defs"],
    }
    return Draft202012Validator(wrapper, registry=REGISTRY)


ME_VECTORS = _load(FIXTURES / "me_vectors.json")
CATALOG_AND_OPERATIONS = _load(FIXTURES / "catalog_and_operations.json")
ENDPOINT_MAPPING = _load(FIXTURES / "endpoint_mapping.json")
ERROR_SEMANTICS = _load(FIXTURES / "error_semantics.json")


def test_me_vectors_match_accepted_dto():
    validator = _validator("subscription.json", "MeResponse")
    for case in ME_VECTORS["cases"]:
        errors = sorted(validator.iter_errors(case["me"]), key=lambda error: error.path)
        assert not errors, (case["name"], [error.message for error in errors])
        expected = case["expected"]
        resolution = case["me"]["grant_resolution"]
        assert resolution["control_available"] is True
        if expected["access_valid"]:
            assert resolution["data_access"] in ("onboarding_hour", "subscription_data")
            assert resolution["effective_deadline"] is not None
        else:
            assert resolution["data_access"] in ("none", "restricted_checkout")
            assert resolution["effective_deadline"] is None


def test_onboarding_hour_constants_and_enrollment_boundary():
    validator = _validator("onboarding.json", "OnboardingHour")
    for case in ME_VECTORS["cases"]:
        onboarding = case["me"]["onboarding"]
        errors = sorted(validator.iter_errors(onboarding), key=lambda error: error.path)
        assert not errors, (case["name"], [error.message for error in errors])
        assert onboarding["duration_seconds"] == 3600
        assert onboarding["one_time"] is True
        assert onboarding["extends_on_refresh"] is False
        assert onboarding["extends_on_restart"] is False
        assert onboarding["creates_trial"] is False
        assert onboarding["started_by"] == "server_confirmed_first_connection"
        if onboarding["state"] == "not_started":
            assert onboarding["started_at"] is None and onboarding["not_after"] is None
        else:
            assert onboarding["started_at"] is not None and onboarding["not_after"] is not None

    enrollment = next(
        route for route in ENDPOINT_MAPPING["routes"] if route["path"] == "/installations"
    )
    assert enrollment["creates_entitlement"] is False
    assert enrollment["data_access"] == "none"
    assert enrollment["scope"] == "enrollment"


def test_catalog_and_operations_fixtures_match_accepted_dto():
    catalog_validator = _validator("catalog.json", "CatalogResponse")
    errors = sorted(
        catalog_validator.iter_errors(CATALOG_AND_OPERATIONS["catalog"]),
        key=lambda error: error.path,
    )
    assert not errors, [error.message for error in errors]

    sync_validator = _validator("operation.json", "AccessSyncResponse")
    for case in CATALOG_AND_OPERATIONS["access_sync_cases"]:
        errors = sorted(sync_validator.iter_errors(case["response"]), key=lambda error: error.path)
        assert not errors, (case["name"], [error.message for error in errors])
        state = case["response"]["access_application_state"]
        if case["expected"]["terminal"]:
            assert state == "applied"
        else:
            assert state in ("pending", "retryable_failure", "rejected")
            assert case["expected"]["native_action"]


def test_endpoint_mapping_matches_accepted_contract_mapping():
    contract = _load(CONTRACT_DIR / "mapping" / "op_scope_field_mapping.json")
    by_path = {op["path"]: op for op in contract["operations"]}
    for route in ENDPOINT_MAPPING["routes"]:
        op = by_path[route["path"]]
        assert route["method"] == op["method"], route["path"]
        assert route["auth"] == op["auth"], route["path"]
        assert route["op"] == op["op"], route["path"]
        assert route["scope"] == op["scope"], route["path"]
        assert route["proof_bearing"] == op["proof_bearing"], route["path"]
        if "session" in op:
            session = op["session"]
            assert route["required_scope"] == session["required_scope"], route["path"]
            assert route["account_states"] == session["account_states"], route["path"]
            assert route["ownership"] == session["ownership"], route["path"]
            assert route["data_access"] == session["data_access"], route["path"]


def test_error_codes_are_from_accepted_enum():
    enum = set(_load(CONTRACT_DIR / "schemas" / "errors.json")["$defs"]["ErrorCode"]["enum"])
    for case in ERROR_SEMANTICS["cases"]:
        if case["code"] is None:
            # Success (authoritative empty catalog) carries no error code by design.
            assert case["http"] == 200, case["scenario"]
            continue
        assert case["code"] in enum, case["scenario"]


def test_policy_flags_match_owner_decisions():
    policy = ERROR_SEMANTICS["policy"]
    assert policy["last_good_catalog_on_error"] is True
    assert policy["nonterminal_errors_do_not_invalidate_known_right"] is True
    assert policy["expired_right_never_resurrected"] is True
    assert policy["management_read_grants_no_data"] is True
    assert policy["same_subject_bearer_refresh_keeps_catalog_baseline"] is True
    assert policy["native_sole_retry_and_canonical_coordinator"] is True

    expired = next(case for case in ME_VECTORS["cases"] if case["name"] == "expired_keeps_last_good")
    assert expired["expected"]["access_valid"] is False
    assert expired["expected"]["new_data_forbidden"] is True
    assert expired["expected"]["catalog_last_good_allowed"] is True
