package servicechannel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/wlwire"
)

// TestAccountaccessSmokeThroughServiceChannel drives the unmodified accountaccess
// session/coordinator stack through the service Doer: challenge + PoP session, then
// bearer-authenticated /me, /gateways and access/sync. Only the transport changes;
// PoP transcripts, bearer headers and the Idempotency-Key stay exactly as the HTTPS
// path builds them.
func TestAccountaccessSmokeThroughServiceChannel(t *testing.T) {
	base := "../accountaccess/testdata/accepted-253cd8a2/"
	challenge := readFixture(t, base+"challenge_response.json")
	session := readFixture(t, base+"session_response.json")
	me := readFixture(t, base+"me_response.json")
	catalog := readFixture(t, base+"catalog_response.json")
	errorPending := readFixture(t, base+"error_access_sync_pending.json")
	operation := readFixture(t, base+"operation_response.json")

	now := time.Now().UTC().Truncate(time.Second)
	session = patchSessionTimes(t, session, now)

	jsonHeaders := map[string]string{"Content-Type": "application/json"}
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		switch frame.Path {
		case "/api/mobile/v1/auth/challenge":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeaders, challenge)}
		case "/api/mobile/v1/auth/session":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeaders, session)}
		case "/api/mobile/v1/me":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeaders, me)}
		case "/api/mobile/v1/gateways":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeaders, catalog)}
		case "/api/mobile/v1/access/sync":
			return fixtureResponse{body: responseJSON(t, id, 409, jsonHeaders, errorPending)}
		case "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeaders, operation)}
		default:
			return fixtureResponse{}
		}
	})
	doer := fixtureDoer(t, fixture)

	var (
		transcripts int
		transcript  []byte
	)
	signer := func(_ context.Context, message []byte) ([]byte, error) {
		transcripts++
		transcript = append([]byte(nil), message...)
		return bytes.Repeat([]byte{0xAB}, 70), nil
	}
	sessionClient, err := accountaccess.NewMobileSession(accountaccess.MobileConfig{
		BaseURL:     testOrigin,
		Environment: accountaccess.EnvironmentTest,
		SPKIDER:     []byte{1, 2, 3},
		HTTP:        doer,
		Signer:      signer,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := sessionClient.Ensure(ctx); err != nil {
		t.Fatalf("session through the service channel: %v", err)
	}
	if transcripts != 1 {
		t.Fatalf("PoP signer calls changed: %d", transcripts)
	}
	if !bytes.HasPrefix(transcript, []byte("WLBS-POP-1\x00")) {
		t.Fatalf("PoP transcript changed: %q", transcript[:min(len(transcript), 24)])
	}
	if bearer, err := sessionClient.Bearer(ctx); err != nil || bearer != "sess-2" {
		t.Fatalf("bearer changed: %q %v", bearer, err)
	}

	client := &accountaccess.Client{
		BaseURL: testOrigin + "/api/mobile/v1",
		HTTP:    doer,
		Tokens:  sessionClient,
	}
	meResponse, apiError, err := client.GetMe(ctx)
	if err != nil || apiError != nil {
		t.Fatalf("me through the service channel: %v %v", err, apiError)
	}
	if meResponse.AccountState != "ACTIVE_PAID" || meResponse.Revision != "7" {
		t.Fatalf("me decode changed: %+v", meResponse)
	}
	gatewaysResponse, apiError, err := client.GetGateways(ctx)
	if err != nil || apiError != nil {
		t.Fatalf("gateways through the service channel: %v %v", err, apiError)
	}
	if gatewaysResponse.Catalog == nil || len(gatewaysResponse.Catalog.Gateways) != 2 ||
		gatewaysResponse.Catalog.Revision != "7" {
		t.Fatalf("catalog decode changed: %+v", gatewaysResponse)
	}
	if _, apiError, err = client.SyncAccess(ctx, accountaccess.AccessSyncRequest{
		CatalogRevision: "7",
		BindingRevision: "3",
	}, "idem-smoke"); err != nil || apiError == nil || apiError.Code != "ACCESS_SYNC_PENDING" {
		t.Fatalf("access/sync classification changed: %v %v", err, apiError)
	}
	operationResponse, apiError, err := client.GetOperation(ctx, "01234567-89ab-cdef-0123-456789abcdef")
	if err != nil || apiError != nil || operationResponse.OperationID != "op-bind-1" {
		t.Fatalf("operation through the service channel: %v %v %+v", err, apiError, operationResponse)
	}

	received, _, ids, invalid := fixture.snapshot()
	if len(received) != 6 {
		t.Fatalf("expected 6 requests, got %d", len(received))
	}
	for index, item := range invalid {
		if item != nil {
			t.Fatalf("request %d failed the bounded contract: %v", index, item)
		}
	}
	byPath := map[string]int{}
	for index, frame := range received {
		if frame.RequestID != RequestID(ids[index]) {
			t.Fatalf("request_id does not echo the frame id at %d", index)
		}
		byPath[frame.Path] = index
	}
	for _, must := range []string{"/api/mobile/v1/auth/challenge", "/api/mobile/v1/auth/session",
		"/api/mobile/v1/me", "/api/mobile/v1/gateways", "/api/mobile/v1/access/sync",
		"/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef"} {
		if _, ok := byPath[must]; !ok {
			t.Fatalf("missing request %s", must)
		}
	}
	challengeFrame := received[byPath["/api/mobile/v1/auth/challenge"]]
	if _, hasAuth := challengeFrame.Headers["Authorization"]; hasAuth {
		t.Fatal("challenge carried a bearer")
	}
	if challengeFrame.Headers["Content-Type"] != "application/json" {
		t.Fatalf("challenge content type changed: %+v", challengeFrame.Headers)
	}
	sessionFrame := received[byPath["/api/mobile/v1/auth/session"]]
	if sessionFrame.Headers["Idempotency-Key"] == "" || sessionFrame.Headers["Content-Type"] != "application/json" {
		t.Fatalf("session headers changed: %+v", sessionFrame.Headers)
	}
	meFrame := received[byPath["/api/mobile/v1/me"]]
	if meFrame.Headers["Authorization"] != "Bearer sess-2" {
		t.Fatalf("me bearer changed: %+v", meFrame.Headers)
	}
	syncFrame := received[byPath["/api/mobile/v1/access/sync"]]
	if syncFrame.Headers["Authorization"] != "Bearer sess-2" || syncFrame.Headers["Idempotency-Key"] != "idem-smoke" {
		t.Fatalf("sync headers changed: %+v", syncFrame.Headers)
	}
	var syncBody map[string]any
	if err := json.Unmarshal(decodeRawURL(t, syncFrame.BodyB64), &syncBody); err != nil {
		t.Fatal(err)
	}
	if syncBody["catalog_revision"] != "7" || syncBody["binding_revision"] != "3" {
		t.Fatalf("sync body changed: %+v", syncBody)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// patchSessionTimes keeps the accepted session fixture live: the contract only
// requires a well-formed, not-yet-expired session for the smoke round trip.
func patchSessionTimes(t *testing.T, raw []byte, now time.Time) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["server_time"] = now.Format(time.RFC3339)
	session, ok := object["session"].(map[string]any)
	if !ok {
		t.Fatal("session fixture shape changed")
	}
	session["expires_at"] = now.Add(time.Hour).Format(time.RFC3339)
	patched, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return patched
}

func decodeRawURL(t *testing.T, encoded string) []byte {
	t.Helper()
	if encoded == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
