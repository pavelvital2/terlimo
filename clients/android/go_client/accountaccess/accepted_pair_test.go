package accountaccess

// Accepted server pair checks: contracts-253cd8a2 (tree 18f2454a) and backend-35ff3194
// provide the contract authority. The fixtures are local copies under testdata/, pinned
// by sha256, and decoded with the real strict client code. No server, no network.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

const (
	acceptedContractDir = "testdata/accepted-253cd8a2/"
	acceptedBackendDir  = "testdata/backend-35ff3194/"
	accepted0503391Dir  = "testdata/accepted-0503391/"
)

// acceptedFixtureSHA256 anchors the exact accepted-pair bytes.
var acceptedFixtureSHA256 = map[string]string{
	"testdata/auth_vectors.json":                            "9e414b4092dd0dedc0cc6ed62f0c0d4cc03aedc43527f2c8d21f7cb143fc0d45",
	acceptedContractDir + "access_sync_response.json":       "d1871920eb88c8eaf2d6b66bd2b6826a783b0c2a32b894f545d5c158b92671c7",
	acceptedContractDir + "catalog_response.json":           "5dc8e0da97c1dd38a5f6c1a0309ede2e2dad2b1e01d9f38ff1e126fb43b936ee",
	acceptedContractDir + "challenge_response.json":         "505e228461597ee326a7ea11242703ae539d5c8f4af0a88b5e4f5ac438ffbf1c",
	acceptedContractDir + "enrollment_response.json":        "c347e0bfdc2015815e98b2c4829ec70c900a27fd9ec655fb1f8cdd7144f925f3",
	acceptedContractDir + "error_access_sync_pending.json":  "8d6a2f6203d62018ff4f0da42713232c2967f488002ae0d367fad41110f5a543",
	acceptedContractDir + "error_idempotency_conflict.json": "3fddc4678e19cfcf1fdc512cfcc46988166d98c770f6641e3bd29c5ee8b9d164",
	acceptedContractDir + "error_revision_conflict.json":    "4fbf620f38c854d9c28b36fae8ad8cda11103cebf52b59a44cf4bc88d57ac0d0",
	acceptedContractDir + "error_service_unavailable.json":  "4fb377685516c8bc7255d8d7af9ec77dee2d49e9ff3706d2b0b010a057f25de1",
	acceptedContractDir + "me_response.json":                "cd7f85e2210fed65159a53509a64dcdc0c83744968f118c826f066737450c612",
	acceptedContractDir + "me_response_no_binding.json":     "b75814ced3ca0254cfe88e2d249f577273b2dcdfaab75b092264a89084d5ad8f",
	acceptedContractDir + "operation_response.json":         "e7c6ca77a1711ce4aa2380aaab295ed7dc57f9da49f262dc1d470c1946ece160",
	acceptedContractDir + "session_response.json":           "13831a92bb53e83c72ffd551b266af389e53070f9aa6f1c266d88a7c50ed867b",
	acceptedBackendDir + "catalog_and_operations.json":      "0f344493331c9332bfd8e98fdef778fec7abeefb7f3bd8b26b150f5385c4751b",
	acceptedBackendDir + "endpoint_mapping.json":            "18f40b96c97039440a945716b013c370f41e4d11820c3bab7f9862e7c98d3a90",
	acceptedBackendDir + "error_semantics.json":             "c298651382396f1defce6149d485b93ecf4eb83e06f6f6c497b251b5c78ae2bf",
	acceptedBackendDir + "me_vectors.json":                  "e66cdee90303da4ce4726f87dfdcae964f9759379a38740bbb704b3055f54d20",
	accepted0503391Dir + "mapping/COMPATIBILITY_NOTES.md":   "fefda2e0d2276d00e49a5125e03f453110d26889343f8ff55326c9b7ad10929c",
	accepted0503391Dir + "schemas/common.json":              "070736d495dd8d0ddc7aa3d4b75a95455a7a5cd1aac151ff8d578bf842696dfc",
	accepted0503391Dir + "schemas/onboarding.json":          "81ff8e130c8bdad922fa6464bbc4f495e3ffbaf306dc007810852008a0027814",
	accepted0503391Dir + "schemas/subscription.json":        "d8bcdae1bfda03b7cffb1b00ef4733d9ee9c5ea90d7c2dc9a43ac29f419e666d",
	accepted0503391Dir + "tests/test_perpetual_deadline.py": "6b9742b77899021ec5b792d3cd104407001e94f33ec64558ce6fea85a1ec672a",
}

func readAcceptedFixture(t *testing.T, path string) []byte {
	t.Helper()
	want, pinned := acceptedFixtureSHA256[path]
	if !pinned {
		t.Fatalf("%s is not a pinned accepted fixture", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("%s checksum mismatch: got %s want %s", path, got, want)
	}
	return raw
}

func TestAcceptedPairFixtureChecksums(t *testing.T) {
	for path := range acceptedFixtureSHA256 {
		readAcceptedFixture(t, path)
	}
}

func TestAcceptedCatalogFixtureStrictDecodeAndLeaseSeq(t *testing.T) {
	raw := readAcceptedFixture(t, acceptedContractDir+"catalog_response.json")
	catalog, err := DecodeCatalogStrict(raw)
	if err != nil {
		t.Fatalf("accepted catalog rejected: %v", err)
	}
	if catalog.Revision != "7" || len(catalog.Gateways) != 2 {
		t.Fatalf("accepted catalog shape changed: %+v", catalog)
	}
	if catalog.Gateways[0].Access.LeaseSeq != "1" || catalog.Gateways[1].Access.LeaseSeq != "2" {
		t.Fatalf("accepted lease_seq not decoded: %+v", catalog.Gateways)
	}
	if digest, err := catalog.DigestedCatalog(); err != nil || len(digest) != 64 {
		t.Fatalf("catalog digest: %q %v", digest, err)
	}
	// The accepted contract requires lease_seq: absence must be malformed.
	without := bytes.Replace(raw, []byte(`"lease_seq": "1",`), nil, 1)
	if _, err := DecodeCatalogStrict(without); err == nil {
		t.Fatal("accepted catalog without lease_seq must be malformed")
	}
}

func TestAcceptedMeFixturesStrictDecode(t *testing.T) {
	me, err := DecodeMeStrict(readAcceptedFixture(t, acceptedContractDir+"me_response.json"))
	if err != nil {
		t.Fatalf("accepted me rejected: %v", err)
	}
	if me.Revision != "7" || me.BindingRevision == nil || *me.BindingRevision != "3" ||
		me.GrantResolution.DataAccess != "subscription_data" || me.GrantResolution.EffectiveDeadline == nil {
		t.Fatalf("accepted me projection changed: %+v", me)
	}
	noBinding, err := DecodeMeStrict(readAcceptedFixture(t, acceptedContractDir+"me_response_no_binding.json"))
	if err != nil {
		t.Fatalf("accepted no-binding me rejected: %v", err)
	}
	if noBinding.BindingRevision != nil || noBinding.GrantResolution.DataAccess != "none" ||
		noBinding.GrantResolution.EffectiveDeadline != nil {
		t.Fatalf("accepted no-binding projection changed: %+v", noBinding)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if decision := DecideAdmission(noBinding, admissionCatalog(t), "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "DATA_ACCESS_NONE" {
		t.Fatalf("management-only me must not admit data: %+v", decision)
	}
}

func TestAcceptedAccessSyncAndOperationFixturesDecode(t *testing.T) {
	var syncResponse AccessSyncResponse
	if err := decodeStrict(readAcceptedFixture(t, acceptedContractDir+"access_sync_response.json"), &syncResponse); err != nil {
		t.Fatalf("accepted access/sync rejected: %v", err)
	}
	if syncResponse.OperationID != "op-sync-1" || !syncResponse.Terminal() || len(syncResponse.Grants) != 1 ||
		syncResponse.Grants[0].AppliedGeneration == nil || *syncResponse.Grants[0].AppliedGeneration != "2" {
		t.Fatalf("accepted access/sync shape changed: %+v", syncResponse)
	}
	var operation OperationResponse
	if err := decodeStrict(readAcceptedFixture(t, acceptedContractDir+"operation_response.json"), &operation); err != nil {
		t.Fatalf("accepted operation rejected: %v", err)
	}
	if operation.OperationID != "op-bind-1" || operation.OperationType != "device_binding" ||
		len(operation.PerNode) != 1 || operation.PerNode[0].Attempts != 1 {
		t.Fatalf("accepted operation shape changed: %+v", operation)
	}
}

func TestAcceptedErrorFixturesDecode(t *testing.T) {
	cases := []struct {
		file      string
		code      string
		retryable bool
	}{
		{"error_access_sync_pending.json", CodeAccessSyncPending, true},
		{"error_service_unavailable.json", CodeServiceUnavailable, true},
		{"error_revision_conflict.json", CodeRevisionConflict, false},
		{"error_idempotency_conflict.json", CodeIdempotencyConflict, false},
	}
	for _, testCase := range cases {
		envelope, err := DecodeErrorStrict(readAcceptedFixture(t, acceptedContractDir+testCase.file))
		if err != nil {
			t.Fatalf("%s rejected: %v", testCase.file, err)
		}
		if envelope.Code != testCase.code || envelope.Retryable != testCase.retryable {
			t.Fatalf("%s classified wrong: %+v", testCase.file, envelope)
		}
		if _, _, tokens := envelope.AdmissionTokens(); tokens {
			t.Fatalf("%s must not synthesize admission tokens without details", testCase.file)
		}
	}
}

func TestAcceptedAccessSyncRequestShape(t *testing.T) {
	body := AccessSyncRequest{CatalogRevision: "4", BindingRevision: "1"}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["catalog_revision"] != "4" || keys["binding_revision"] != "1" {
		t.Fatalf("access/sync request shape changed: %s", raw)
	}
	digest, err := body.Digest()
	if err != nil || digest != "d9390e00e064f3c9df86906a7ae32dd9630ab0684f5467fd2c222baf05fa721b" {
		t.Fatalf("access/sync canonical digest=%s err=%v", digest, err)
	}
}

type acceptedPairDoer struct {
	challenge []byte
	session   []byte
	bodies    []map[string]any
}

func (d *acceptedPairDoer) Do(request *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	d.bodies = append(d.bodies, body)
	var payload []byte
	switch request.URL.Path {
	case "/api/mobile/v1/auth/challenge":
		payload = d.challenge
	case "/api/mobile/v1/auth/session":
		payload = d.session
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader([]byte(`{}`)))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(payload))}, nil
}

func TestAcceptedSessionFixtureAndProofShape(t *testing.T) {
	doer := &acceptedPairDoer{
		challenge: readAcceptedFixture(t, acceptedContractDir+"challenge_response.json"),
		session:   readAcceptedFixture(t, acceptedContractDir+"session_response.json"),
	}
	spki := bytes.Repeat([]byte{7}, 64)
	session, err := NewMobileSession(MobileConfig{
		BaseURL: "https://fixture.invalid", Environment: EnvironmentTest, SPKIDER: spki, HTTP: doer,
		Signer: func(context.Context, []byte) ([]byte, error) { return bytes.Repeat([]byte{1}, 64), nil },
		Now:    func() time.Time { return time.Date(2026, 9, 19, 15, 20, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Ensure(context.Background()); err != nil {
		t.Fatalf("accepted session fixture rejected: %v", err)
	}
	if session.Generation() != "3" || session.Fingerprint() != InstallationFingerprint(spki) ||
		session.Subject().AccountRef != "acc-1" || !containsString(session.Scopes(), "access:sync") {
		t.Fatalf("accepted session projection wrong: gen=%q scopes=%v", session.Generation(), session.Scopes())
	}
	if len(doer.bodies) != 2 {
		t.Fatalf("challenge/session request count=%d", len(doer.bodies))
	}
	challengeRequest := doer.bodies[0]
	if len(challengeRequest) != 3 || challengeRequest["purpose"] != "session" || challengeRequest["environment"] != "test" ||
		challengeRequest["installation_fingerprint"] != InstallationFingerprint(spki) {
		t.Fatalf("challenge request shape changed: %+v", challengeRequest)
	}
	sessionRequest := doer.bodies[1]
	if len(sessionRequest) != 1 {
		t.Fatalf("session request shape changed: %+v", sessionRequest)
	}
	proof, _ := sessionRequest["proof"].(map[string]any)
	wantedProofKeys := []string{"algorithm", "challenge_id", "environment", "nonce_b64", "payload_hash", "request_id", "signature_b64", "signed_payload_b64"}
	if len(proof) != len(wantedProofKeys) {
		t.Fatalf("proof key count=%d: %+v", len(proof), proof)
	}
	for _, key := range wantedProofKeys {
		if _, present := proof[key]; !present {
			t.Fatalf("proof missing %q: %+v", key, proof)
		}
	}
	if proof["challenge_id"] != "fedcba9876543210fedcba9876543210" || proof["nonce_b64"] != "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM" {
		t.Fatalf("proof must bind the accepted challenge: %+v", proof)
	}
	signed, _ := proof["signed_payload_b64"].(string)
	payloadRaw, payload, err := DecodeProofPayload(signed)
	if err != nil {
		t.Fatalf("proof payload is not canonical: %v", err)
	}
	sum := sha256.Sum256(payloadRaw)
	if payload["scope"] != "session" || payload["op"] != "auth.session" || payload["env"] != "test" ||
		hex.EncodeToString(sum[:]) != proof["payload_hash"] {
		t.Fatalf("poP payload shape changed: %+v", payload)
	}
}

func TestAcceptedBackendCatalogAndOperationVectors(t *testing.T) {
	raw := readAcceptedFixture(t, acceptedBackendDir+"catalog_and_operations.json")
	var fixture struct {
		Catalog json.RawMessage `json:"catalog"`
		Cases   []struct {
			Name     string          `json:"name"`
			Response json.RawMessage `json:"response"`
			Expected struct {
				HTTP     int  `json:"http"`
				Terminal bool `json:"terminal"`
			} `json:"expected"`
		} `json:"access_sync_cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	catalog, err := DecodeCatalogStrict(fixture.Catalog)
	if err != nil {
		t.Fatalf("backend catalog rejected: %v", err)
	}
	if len(catalog.Gateways) != 1 || catalog.Gateways[0].Access.LeaseSeq != "2" ||
		catalog.Gateways[0].Transport.DTLSSPKISHA256 != "PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo" {
		t.Fatalf("backend catalog shape changed: %+v", catalog.Gateways)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("no access/sync vectors")
	}
	for _, testCase := range fixture.Cases {
		var response AccessSyncResponse
		if err := decodeStrict(testCase.Response, &response); err != nil {
			t.Fatalf("%s rejected: %v", testCase.Name, err)
		}
		if testCase.Expected.HTTP != http.StatusOK || response.Terminal() != testCase.Expected.Terminal {
			t.Fatalf("%s terminal=%v want %v", testCase.Name, response.Terminal(), testCase.Expected.Terminal)
		}
		if response.OperationID == "" || response.SchemaVersion != SchemaVersion {
			t.Fatalf("%s envelope incomplete: %+v", testCase.Name, response)
		}
	}
}

func TestAcceptedBackendMeVectors(t *testing.T) {
	raw := readAcceptedFixture(t, acceptedBackendDir+"me_vectors.json")
	var vectors struct {
		Cases []struct {
			Name     string `json:"name"`
			Expected struct {
				DataAccess  string `json:"data_access"`
				AccessValid bool   `json:"access_valid"`
			} `json:"expected"`
			Me json.RawMessage `json:"me"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) < 5 {
		t.Fatalf("me vectors: %d", len(vectors.Cases))
	}
	for _, testCase := range vectors.Cases {
		me, err := DecodeMeStrict(testCase.Me)
		if err != nil {
			t.Fatalf("%s rejected: %v", testCase.Name, err)
		}
		if me.GrantResolution.DataAccess != testCase.Expected.DataAccess {
			t.Fatalf("%s data_access=%q want %q", testCase.Name, me.GrantResolution.DataAccess, testCase.Expected.DataAccess)
		}
		if testCase.Expected.DataAccess == "none" && me.GrantResolution.EffectiveDeadline != nil {
			t.Fatalf("%s must not carry a deadline", testCase.Name)
		}
		if testCase.Expected.DataAccess == "subscription_data" && me.GrantResolution.EffectiveDeadline == nil {
			t.Fatalf("%s must carry a deadline", testCase.Name)
		}
	}
}

func TestAcceptedBackendErrorSemanticsAndRoutes(t *testing.T) {
	raw := readAcceptedFixture(t, acceptedBackendDir+"error_semantics.json")
	var semantics struct {
		Policy map[string]bool `json:"policy"`
		Cases  []struct {
			Scenario string `json:"scenario"`
			HTTP     int    `json:"http"`
			Code     string `json:"code"`
			Route    string `json:"route"`
			Retry    bool   `json:"retryable"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &semantics); err != nil {
		t.Fatal(err)
	}
	if !semantics.Policy["last_good_catalog_on_error"] || !semantics.Policy["catalog_nonterminal_keeps_last_good"] {
		t.Fatalf("accepted catalog error policy changed: %+v", semantics.Policy)
	}
	found := map[string]string{}
	for _, testCase := range semantics.Cases {
		if testCase.Route != "" {
			found[testCase.Scenario] = testCase.Route
		}
	}
	if found["catalog_pending_get"] != "GET /gateways" || found["catalog_application_failed"] != "GET /gateways" {
		t.Fatalf("catalog nonterminal routes changed: %+v", found)
	}
	for _, testCase := range semantics.Cases {
		switch testCase.Scenario {
		case "catalog_pending_get":
			if testCase.Code != CodeAccessSyncPending || !testCase.Retry {
				t.Fatalf("catalog pending semantics changed: %+v", testCase)
			}
		case "catalog_application_failed":
			if testCase.Code != CodeServiceUnavailable {
				t.Fatalf("catalog failure semantics changed: %+v", testCase)
			}
		}
	}

	raw = readAcceptedFixture(t, acceptedBackendDir+"endpoint_mapping.json")
	var mapping struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Op     string `json:"op"`
			Scope  string `json:"scope"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &mapping); err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{}
	for _, route := range mapping.Routes {
		routes[route.Method+" "+route.Path] = route.Op
	}
	for _, wanted := range []string{"POST /auth/challenge", "POST /installations", "POST /auth/session", "GET /me", "GET /gateways", "POST /access/sync"} {
		if _, present := routes[wanted]; !present {
			t.Fatalf("accepted endpoint mapping lost %s: %+v", wanted, routes)
		}
	}
}
