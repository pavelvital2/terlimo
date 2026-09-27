"""Contract vectors from terlimo-s1-contracts @ 336bd6d verify the ported PoP code.

Runs the accepted vectors through this backend's `terlimo_backend.pop` AND through the
contract's own `auth/pop_canonical.py` (loaded from the contract checkout) and compares
their outputs, so the port cannot silently diverge from the accepted bytes.
"""

from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path

import pytest

from terlimo_backend import pop

CONTRACT_DIR = Path(os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts"))
VECTORS_PATH = CONTRACT_DIR / "vectors" / "auth_vectors.json"
CONTRACT_POP_PATH = CONTRACT_DIR / "auth" / "pop_canonical.py"

if not VECTORS_PATH.exists() or not CONTRACT_POP_PATH.exists():
    pytest.skip(
        "accepted contract vectors not available (set TERLIMO_CONTRACT_DIR)",
        allow_module_level=True,
    )


def _load_contract_pop():
    spec = importlib.util.spec_from_file_location("contract_pop_canonical", CONTRACT_POP_PATH)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


contract_pop = _load_contract_pop()
VECTORS = json.loads(VECTORS_PATH.read_text(encoding="utf-8"))
BY_NAME = {vector["name"]: vector for vector in VECTORS["vectors"]}


def test_known_top_level_matches_contract():
    # Accepted vectors pin the contract subset; the explicit-connect flow adds documented
    # server-known fields (request_key/intent_id, doc STEP036 rev3; gateway_key, owner doc 19
    # control404 section 6). Any other difference still fails.
    contract_fields = set(VECTORS["known_top_level"])
    documented_additions = {"request_key", "intent_id", "gateway_key"}
    assert contract_fields <= set(pop.KNOWN_TOP_LEVEL)
    assert set(pop.KNOWN_TOP_LEVEL) - contract_fields == documented_additions


def test_canonical_bytes_digest_and_message_match_contract_for_all_vectors():
    for vector in VECTORS["vectors"]:
        payload = vector["payload"]
        assert pop.canonical_json(payload).hex() == vector["payload_canonical_bytes_hex"], vector["name"]
        assert pop.canonical_json(payload) == contract_pop.canonical_json(payload), vector["name"]
        assert pop.sha256_hex(pop.canonical_json(payload)) == vector["payload_hash"], vector["name"]
        message = pop.pop_message(
            vector["request_id"], vector["challenge_id"], vector["nonce_b64"], pop.canonical_json(payload)
        )
        assert message.hex() == vector["canonical_message_hex"], vector["name"]
        contract_message = contract_pop.pop_message(
            vector["request_id"],
            vector["challenge_id"],
            vector["nonce_b64"],
            contract_pop.canonical_json(payload),
        )
        assert message == contract_message, vector["name"]


def test_accepted_signatures_verify_with_this_port():
    public_key = pop.load_public_key(VECTORS["test_key"]["public_spki_b64"])
    for vector in VECTORS["vectors"]:
        if not vector["expected"].get("verify", False):
            continue
        raw, _ = pop.decode_signed_payload(vector["signed_payload_b64"])
        assert pop.sha256_hex(raw) == vector["payload_hash"], vector["name"]
        message = pop.pop_message(
            vector["request_id"], vector["challenge_id"], vector["nonce_b64"], raw
        )
        assert pop.verify(public_key, message, vector["signature_b64"]), vector["name"]


def test_wrong_key_and_tampered_payload_are_rejected():
    key_vector = BY_NAME["wrong_key"]
    public_key = pop.load_public_key(key_vector["expected_public_spki_b64"])
    raw, _ = pop.decode_signed_payload(key_vector["signed_payload_b64"])
    message = pop.pop_message(
        key_vector["request_id"], key_vector["challenge_id"], key_vector["nonce_b64"], raw
    )
    assert not pop.verify(public_key, message, key_vector["signature_b64"])

    tampered = BY_NAME["tampered_signed_payload"]
    with pytest.raises(pop.PopError):
        pop.decode_signed_payload(tampered["signed_payload_b64"])


def test_business_digest_matches_contract_and_retry_identity():
    changed = VECTORS["changed_business_data"]
    assert pop.business_digest(changed["before"]["payload"]) == changed["before"]["digest"]
    assert pop.business_digest(changed["after"]["payload"]) == changed["after"]["digest"]
    assert changed["before"]["digest"] != changed["after"]["digest"]

    retry = VECTORS["retry"]
    digests = [pop.business_digest(proof["payload"]) for proof in retry["proofs"]]
    assert digests == retry["business_digests"]
    assert digests[0] == digests[1]

    conflict = VECTORS["idempotency_conflict"]
    assert pop.business_digest(conflict["first_payload"]) != pop.business_digest(
        conflict["second_payload"]
    )


def test_unicode_nfc_vector():
    vector = VECTORS["unicode_nfc"]
    assert pop.nfc(vector["composed"]) == vector["composed"]
    assert pop.nfc(vector["decomposed"]) == vector["composed"]
    assert pop.canonical_json({"value": vector["decomposed"]}) == pop.canonical_json(
        {"value": vector["composed"]}
    )
    assert vector["canonical_equal"] is True


def test_environment_and_scope_helpers_raise_contract_codes():
    with pytest.raises(pop.WrongEnvironment):
        pop.check_environment({"env": "production"}, "test")
    with pytest.raises(pop.WrongScope):
        pop.check_scope({"scope": "binding"}, "session")
    with pytest.raises(pop.UnknownCriticalField):
        pop.check_critical_fields({"critical": ["x-future-field"]}, ["env", "scope"])


def test_installation_fingerprint_matches_enrollment_vector():
    enrollment = BY_NAME["enrollment_valid"]
    fingerprint = pop.installation_fingerprint(enrollment["request_material"]["public_key_spki_b64"])
    assert fingerprint == enrollment["payload"]["installation_id"]
