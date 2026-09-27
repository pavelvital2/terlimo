package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"wg-turn-client/internal/wlwire"
)

func waitServiceIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if serviceGlobalLimiter().inFlight() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("service admission slot not released: inFlight=%d", serviceGlobalLimiter().inFlight())
}

// waitDone proves the handler returned while the server listener is still open and the
// parent context is alive (peer-only cancellation isolation).
func mustServiceFrames(t *testing.T, id wlwire.ID, payload []byte) [][]byte {
	t.Helper()
	frames, err := wlwire.ServiceFrames(id, false, payload)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func (p *serviceDTLSPair) waitDone(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		t.Fatal("service handler did not finish while the server listener stayed open")
	}
}

// TestServicePairingUnixChain runs the real node service handler/codec against a live
// ServiceRelay Unix socket (WL_SERVICE_PAIRING_SOCKET) that forwards over per-node mTLS to
// the real backend service endpoint and its existing public API. Skipped unless the socket
// is provided by the pairing harness; the repository suite is unaffected.
func TestServicePairingUnixChain(t *testing.T) {
	socket := os.Getenv("WL_SERVICE_PAIRING_SOCKET")
	if socket == "" {
		t.Skip("WL_SERVICE_PAIRING_SOCKET is not set")
	}
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request := func(t *testing.T, marker byte, method, path string, body []byte, headers map[string]string) serviceResponse {
		t.Helper()
		pair := startServiceDTLS(t, ctx, identity)
		id := svcTestID(marker)
		serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, method, path, headers, body))
		_, raw := serviceReadResponse(t, pair.client)
		pair.close(t)
		waitServiceIdle(t)
		var resp serviceResponse
		if err := wlwire.StrictJSON(raw, &resp); err == nil && resp.V == 1 && resp.Status != 0 {
			return resp
		}
		var errFrame serviceErrorFrame
		if err := wlwire.StrictJSON(raw, &errFrame); err != nil {
			t.Fatalf("pairing reply not a union: %s", raw)
		}
		t.Fatalf("pairing returned transport error %s", errFrame.Error.Code)
		return serviceResponse{}
	}

	// 1) Allowed request through the full chain preserves the existing API 401 + body.
	resp := request(t, 1, "GET", "/api/mobile/v1/me", nil, nil)
	if resp.Status != 401 {
		t.Fatalf("expected API 401, got %d", resp.Status)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(resp.BodyB64)
	if err != nil {
		t.Fatal(err)
	}
	var apiBody map[string]any
	if json.Unmarshal(decoded, &apiBody) != nil || apiBody["code"] != "SESSION_INVALID" {
		t.Fatalf("API body not preserved: %s", decoded)
	}
	t.Logf("PAIRING_OK scenario=allowed status=401 code=%v", apiBody["code"])

	// 1b) Real successful 2xx operation: existing auth/challenge with a valid fixture payload.
	challengeBody, _ := json.Marshal(map[string]any{
		"installation_fingerprint": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"purpose":                  "enrollment",
		"environment":              "test",
	})
	challengeResp := request(t, 6, "POST", "/api/mobile/v1/auth/challenge", challengeBody, map[string]string{"Content-Type": "application/json"})
	if challengeResp.Status != 200 {
		t.Fatalf("challenge expected 200, got %d", challengeResp.Status)
	}
	challengeRaw, err := base64.RawURLEncoding.Strict().DecodeString(challengeResp.BodyB64)
	if err != nil {
		t.Fatal(err)
	}
	var challenge map[string]any
	if json.Unmarshal(challengeRaw, &challenge) != nil || challenge["status"] != "ok" {
		t.Fatalf("challenge body invalid: %s", challengeRaw)
	}
	challengeID, _ := challenge["challenge_id"].(string)
	if len(challengeID) != 32 {
		t.Fatalf("challenge_id missing/short: %q", challengeID)
	}
	t.Logf("PAIRING_OK scenario=challenge_2xx status=200 challenge_id_len=%d", len(challengeID))

	// 2) Denied path is rejected before any upstream call.
	pairDenied := startServiceDTLS(t, ctx, identity)
	idDenied := svcTestID(2)
	serviceWriteRequest(t, pairDenied.client, idDenied, serviceRequestBody(t, idDenied, "GET", "/internal/onboarding/evidence", nil, nil))
	_, deniedRaw := serviceReadResponse(t, pairDenied.client)
	pairDenied.close(t)
	waitServiceIdle(t)
	var denied serviceErrorFrame
	if err := wlwire.StrictJSON(deniedRaw, &denied); err != nil || denied.Error.Code != "SERVICE_PATH_DENIED" {
		t.Fatalf("denied path not rejected: %s", deniedRaw)
	}
	t.Logf("PAIRING_OK scenario=denied code=%s", denied.Error.Code)

	// 3) Legal large request frame (64KiB decoded, JSON above the old 64KiB reader limit).
	large := bytes.Repeat([]byte("q"), 64*1024)
	respLarge := request(t, 3, "POST", "/api/mobile/v1/access/sync", large, nil)
	if respLarge.Status != 401 {
		t.Fatalf("large legal frame not processed: %d", respLarge.Status)
	}
	t.Logf("PAIRING_OK scenario=large_request status=%d", respLarge.Status)

	// 4a) Cancellation mid-frame (no upstream entry) then recovery.
	pairCancel := startServiceDTLS(t, ctx, identity)
	frames, err := wlwire.ServiceFrames(svcTestID(4), false, bytes.Repeat([]byte("z"), 40*1024))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pairCancel.client.Write(frames[0]); err != nil {
		t.Fatal(err)
	}
	_ = pairCancel.client.Close()
	pairCancel.client = nil
	pairCancel.close(t)
	waitServiceIdle(t)
	respAfter := request(t, 5, "GET", "/api/mobile/v1/me", nil, nil)
	if respAfter.Status != 401 {
		t.Fatalf("chain not recovered after mid-frame cancel: %d", respAfter.Status)
	}
	t.Logf("PAIRING_OK scenario=cancel_release status=%d", respAfter.Status)

	// 4b) Active-upstream cancellation: disconnect only after the TEST fixture confirms the
	// request entered the upstream API. The server listener stays open and the parent ctx
	// stays alive while we await handler completion, so only the peer disconnect is isolated.
	barrierFile := os.Getenv("WL_PAIRING_BARRIER_FILE")
	if barrierFile != "" {
		pairActive := startServiceDTLS(t, ctx, identity)
		idActive := svcTestID(8)
		serviceWriteRequest(t, pairActive.client, idActive, serviceRequestBody(t, idActive, "POST", "/api/mobile/v1/access/sync", map[string]string{"Idempotency-Key": "pairing-barrier"}, nil))
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(barrierFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(barrierFile); err != nil {
			t.Fatal("upstream barrier was never entered")
		}
		_ = pairActive.client.Close()
		pairActive.client = nil
		pairActive.waitDone(t, 5*time.Second) // listener open + parent ctx alive
		waitServiceIdle(t)
		_ = pairActive.server.Close()
		respActive := request(t, 9, "GET", "/api/mobile/v1/me", nil, nil)
		if respActive.Status != 401 {
			t.Fatalf("chain not recovered after active-upstream cancel: %d", respActive.Status)
		}
		t.Logf("PAIRING_OK scenario=active_upstream_cancel status=%d", respActive.Status)
	}

	// 5) Several sequential request/response pairs over ONE DTLS session.
	pairSeq := startServiceDTLS(t, ctx, identity)
	for i := 0; i < 3; i++ {
		id := svcTestID(byte(20 + i))
		serviceWriteRequest(t, pairSeq.client, id, serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
		_, raw := serviceReadResponse(t, pairSeq.client)
		var seqResp serviceResponse
		if err := wlwire.StrictJSON(raw, &seqResp); err != nil || seqResp.Status != 401 {
			t.Fatalf("sequential request %d failed: %s", i, raw)
		}
	}
	pairSeq.close(t)
	waitServiceIdle(t)
	t.Logf("PAIRING_OK scenario=sequential_session requests=3")

	// 6) Watcher/cancel race: repeated open/write/close with no listener teardown between
	// the close and handler completion, then the chain must still work.
	for i := 0; i < 15; i++ {
		pair := startServiceDTLS(t, ctx, identity)
		id := svcTestID(byte(40 + i))
		if i%2 == 0 {
			for _, frame := range mustServiceFrames(t, id, serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil)) {
				_, _ = pair.client.Write(frame)
			}
		}
		_ = pair.client.Close()
		pair.client = nil
		pair.waitDone(t, 5*time.Second)
		waitServiceIdle(t)
		_ = pair.server.Close()
	}
	respRace := request(t, 90, "GET", "/api/mobile/v1/me", nil, nil)
	if respRace.Status != 401 {
		t.Fatalf("chain not recovered after watcher race: %d", respRace.Status)
	}
	t.Logf("PAIRING_OK scenario=watcher_race iterations=15 status=%d", respRace.Status)
}
