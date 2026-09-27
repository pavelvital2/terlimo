package onboarding

// Hermetic compatibility checks against the authoritative synthetic intent fixture
// delivered for accepted server 193511c: the exact signed onboarding.intent key sets
// with and without the optional gateway_key. The fixture is copied byte-identical under
// go_client/testdata/step036/ and the pinned sha256 is verified before every read. No
// server, no network.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

const step036IntentFixtureSHA = "25d74d343883cb8d289acd10980bb8351e67ea8503bb6087541cdd8556e0521c"

// step036IntentFixtureBytes reads the hermetic fixture and verifies its pinned sha256
// before any decoding: a mismatch is a hard abort, never a fit.
func step036IntentFixtureBytes(t *testing.T) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("step036 intent fixture: runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "testdata", "step036", "step036_intent_payload_gateway_key.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("step036 intent fixture: %v", err)
	}
	digest := sha256.Sum256(raw)
	got := hex.EncodeToString(digest[:])
	if got != step036IntentFixtureSHA {
		t.Fatalf("step036 intent fixture sha256 mismatch: got %s want %s; aborting", got, step036IntentFixtureSHA)
	}
	return raw
}

// step036IntentFixture loads the two signed payload shapes: with_gateway_key (a non-first
// selected gateway) and legacy_without_gateway_key. The top-level `synthetic` harness
// marker is ignored; only the payload objects are consumed.
func step036IntentFixture(t *testing.T) map[string]map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(step036IntentFixtureBytes(t), &document); err != nil {
		t.Fatalf("step036 intent fixture parse: %v", err)
	}
	if document["synthetic"] != true {
		t.Fatalf("step036 intent fixture harness marker missing: %v", document["synthetic"])
	}
	fixture := map[string]map[string]any{}
	for _, name := range []string{"with_gateway_key", "legacy_without_gateway_key"} {
		payload, ok := document[name].(map[string]any)
		if !ok || len(payload) == 0 {
			t.Fatalf("step036 intent fixture %s payload missing", name)
		}
		fixture[name] = payload
	}
	return fixture
}

func sortedFieldNames(payload map[string]any) []string {
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestIntentPayloadKeySetMatchesSyntheticServerFixture asserts the builder's key sets are
// EXACTLY the fixture payload key sets (9 keys with gateway_key, 8 without) and that the
// built values equal the fixture values for the fixture inputs.
func TestIntentPayloadKeySetMatchesSyntheticServerFixture(t *testing.T) {
	fixture := step036IntentFixture(t)
	with := fixture["with_gateway_key"]
	legacy := fixture["legacy_without_gateway_key"]

	bound := IntentPayload(with["env"].(string), with["installation_id"].(string), with["request_key"].(string),
		with["nonce"].(string), with["request_id"].(string), with["ts"].(string), with["gateway_key"].(string))
	if got, want := sortedFieldNames(bound), sortedFieldNames(with); !reflect.DeepEqual(got, want) {
		t.Fatalf("bound intent key set = %v, fixture with_gateway_key = %v", got, want)
	}
	if !reflect.DeepEqual(bound, with) {
		t.Fatalf("bound intent values differ from the fixture:\n got %v\nwant %v", bound, with)
	}

	legacyBuilt := IntentPayload(legacy["env"].(string), legacy["installation_id"].(string), legacy["request_key"].(string),
		legacy["nonce"].(string), legacy["request_id"].(string), legacy["ts"].(string), "")
	if got, want := sortedFieldNames(legacyBuilt), sortedFieldNames(legacy); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy intent key set = %v, fixture legacy_without_gateway_key = %v", got, want)
	}
	if !reflect.DeepEqual(legacyBuilt, legacy) {
		t.Fatalf("legacy intent values differ from the fixture:\n got %v\nwant %v", legacyBuilt, legacy)
	}
	if _, present := legacyBuilt["gateway_key"]; present {
		t.Fatal("legacy intent must not carry gateway_key")
	}

	// The signed start payload is a different frozen shape: it keeps its accepted legacy
	// field set and never inherits gateway_key.
	start := StartPayload(legacy["env"].(string), legacy["installation_id"].(string), legacy["request_key"].(string),
		testIntentID, legacy["nonce"].(string), legacy["request_id"].(string), legacy["ts"].(string))
	if _, present := start["gateway_key"]; present {
		t.Fatal("signed start payload must not carry gateway_key")
	}
	if len(start) != len(legacy)+1 || start["op"] != StartOp || start["intent_id"] != testIntentID {
		t.Fatalf("start payload field set changed: %v", sortedFieldNames(start))
	}
}
