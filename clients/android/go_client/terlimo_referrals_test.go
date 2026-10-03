package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"wg-turn-client/accountaccess"
)

const referralTestAccount = "12345678-1234-1234-1234-123456789abc"

func referralNativeFixtures(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../../../docs/TERLIMO_IMPLEMENTATION/referral_20261003/native-wire-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func referralNativeHarness(t *testing.T, linked bool, responder func(http.ResponseWriter, *http.Request, *managedMobile)) (*managedMobile, *syncBuffer) {
	t.Helper()
	fixture := newWiringFixture(t)
	base := fixture.handler()
	var mobile *managedMobile
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/mobile/v1/auth/session" {
			recorded := httptest.NewRecorder()
			base.ServeHTTP(recorded, r)
			var envelope map[string]any
			if err := json.Unmarshal(recorded.Body.Bytes(), &envelope); err != nil {
				t.Error(err)
				return
			}
			if linked {
				envelope["session"].(map[string]any)["account_ref"] = referralTestAccount
			}
			_ = json.NewEncoder(w).Encode(envelope)
			return
		}
		if r.URL.Path == "/api/mobile/v1/auth/challenge" {
			base.ServeHTTP(w, r)
			return
		}
		responder(w, r, mobile)
	}))
	t.Cleanup(server.Close)
	output := &syncBuffer{}
	bridge := newManagedBridge(output, "referral-attempt", func() {})
	start := managedStart{MobileBaseURL: server.URL, MobileEnvironment: "test"}
	var err error
	mobile, err = newManagedMobile(start, fixture.spkiDER, func(_ context.Context, raw []byte) ([]byte, error) {
		digest := sha256.Sum256(raw)
		return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	}, bridge, &managedController{bridge: bridge, start: start})
	if err != nil {
		t.Fatal(err)
	}
	return mobile, output
}

func referralLastResult(t *testing.T, output *syncBuffer) bridgeMessage {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var event bridgeMessage
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestReferralBridgeCorrelationPreemptionAndLegacyRegistration(t *testing.T) {
	var output bytes.Buffer
	bridge := newManagedBridge(&output, "attempt", func() {})
	preempts := 0
	bridge.preemptSeed = func() { preempts++ }
	frames := []bridgeMessage{
		{"v": 1, "attempt_id": "old-attempt", "type": referralActionCandidateSet},
		{"v": 1, "attempt_id": "attempt", "type": referralActionCandidateSet, "client_request_id": "set-1", "idempotency_key": "original-key-0001", "code": "Ab12", "seed_update_epoch": "7"},
		{"v": 1, "attempt_id": "attempt", "type": referralActionCandidateClear, "client_request_id": "clear-1", "idempotency_key": "original-key-0002"},
		{"v": 1, "attempt_id": "attempt", "type": "request_telegram_registration"},
	}
	var input bytes.Buffer
	for _, frame := range frames {
		_ = json.NewEncoder(&input).Encode(frame)
	}
	bridge.read(context.Background(), bufio.NewScanner(&input))
	if preempts != 3 || bridge.seedUpdateEpoch != 7 {
		t.Fatalf("preemption/epoch: %d/%d", preempts, bridge.seedUpdateEpoch)
	}
	request := <-bridge.referrals
	if request.RequestID != "set-1" || request.IdempotencyKey != "original-key-0001" || request.Code != "Ab12" {
		t.Fatalf("mutation intent changed: %+v", request)
	}
	if op := <-bridge.registration; op != "request_telegram_registration" {
		t.Fatalf("legacy registration changed: %q", op)
	}
	var busy bridgeMessage
	if json.Unmarshal(bytes.TrimSpace(output.Bytes()), &busy) != nil || busy.string("code") != "BUSY" || busy.string("client_request_id") != "clear-1" || busy.string("idempotency_key") != "original-key-0002" {
		t.Fatalf("lost busy correlation: %s", output.String())
	}
}

func TestReferralBridgeRejectsMalformedCandidateAndRegistration(t *testing.T) {
	for _, frame := range []bridgeMessage{
		{"type": referralActionCandidateSet, "client_request_id": "set", "idempotency_key": "original-key-0001", "code": "https://t.me/a"},
		{"type": referralActionCandidateSet, "client_request_id": "set", "idempotency_key": "original-key-0001", "code": "Код"},
		{"type": referralActionCandidateSet, "client_request_id": "set", "idempotency_key": "original-key-0001", "code": strings.Repeat("a", 33)},
		{"type": referralActionRegistration, "client_request_id": "registration", "referral_candidate_id": referralTestAccount},
	} {
		var output bytes.Buffer
		bridge := newManagedBridge(&output, "attempt", func() {})
		bridge.queueReferral(frame)
		if len(bridge.referrals) != 0 || !strings.Contains(output.String(), `"code":"INVALID_REQUEST"`) {
			t.Fatalf("malformed input entered queue: %s", output.String())
		}
	}
}

func TestReferralCandidateOriginalKeyReplayAndClear(t *testing.T) {
	fixtures := referralNativeFixtures(t)
	var requests []capturedBridgeRequest
	mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, r *http.Request, _ *managedMobile) {
		raw, _ := io.ReadAll(r.Body)
		requests = append(requests, capturedBridgeRequest{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key"), Auth: r.Header.Get("Authorization"), Body: string(raw)})
		if r.Method == http.MethodDelete {
			_, _ = w.Write(fixtures["referralCleared"])
		} else {
			_, _ = w.Write(fixtures["referralPending"])
		}
	})
	request := managedReferralRequest{Action: referralActionCandidateSet, RequestID: "set-1", IdempotencyKey: "original-key-0001", Code: "Ab12"}
	for i := 0; i < 2; i++ {
		mobile.handleReferralAction(context.Background(), request)
		event := referralLastResult(t, output)
		if event.string("state") != "ok" || event.string("idempotency_key") != request.IdempotencyKey || event.string("client_request_id") != request.RequestID {
			t.Fatalf("missing replay correlation: %+v", event)
		}
		got, _ := json.Marshal(event["payload"])
		decoded, err := accountaccess.DecodeReferralCandidateStrict(got)
		wanted, fixtureErr := accountaccess.DecodeReferralCandidateStrict(fixtures["referralPending"])
		if err != nil || fixtureErr != nil || decoded != wanted {
			t.Fatalf("candidate payload changed: %s", got)
		}
	}
	mobile.handleReferralAction(context.Background(), managedReferralRequest{Action: referralActionCandidateClear, RequestID: "clear-1", IdempotencyKey: "original-key-0002"})
	if event := referralLastResult(t, output); event.string("state") != "ok" || event.string("operation") != "clear" {
		t.Fatalf("clear result: %+v", event)
	}
	if len(requests) != 3 {
		t.Fatalf("requests: %+v", requests)
	}
	for i, req := range requests {
		if req.Path != "/api/mobile/v1/referral/candidate" || !strings.HasPrefix(req.Auth, "Bearer ") {
			t.Fatalf("unauthenticated/wrong route: %+v", req)
		}
		if i < 2 && (req.Key != "original-key-0001" || req.Body != `{"code":"Ab12"}` || req.Method != http.MethodPost) {
			t.Fatalf("original replay changed: %+v", req)
		}
	}
	if requests[2].Method != http.MethodDelete || requests[2].Body != "" || requests[2].Key != "original-key-0002" {
		t.Fatalf("clear wire: %+v", requests[2])
	}
}

func TestReferralInfoExpiredAccountForeignAndStaleResponses(t *testing.T) {
	fixtures := referralNativeFixtures(t)
	for _, mode := range []string{"expired", "foreign", "stale", "unlinked"} {
		t.Run(mode, func(t *testing.T) {
			var paths []string
			mobile, output := referralNativeHarness(t, mode != "unlinked", func(w http.ResponseWriter, r *http.Request, m *managedMobile) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/api/mobile/v1/me":
					var me map[string]any
					_ = json.Unmarshal([]byte(meFixtureJSON), &me)
					me["account_ref"], me["account_state"] = referralTestAccount, "EXPIRED"
					me["entitlement"].(map[string]any)["status"] = "expired"
					_ = json.NewEncoder(w).Encode(me)
				case "/api/mobile/v1/referral":
					raw := fixtures["referralInfoFixture"]
					if mode == "foreign" {
						raw = bytes.ReplaceAll(raw, []byte(referralTestAccount), []byte("22345678-1234-1234-1234-123456789abc"))
					}
					if mode == "stale" {
						m.session.Refresh()
					}
					_, _ = w.Write(raw)
				default:
					t.Errorf("unexpected route: %s", r.URL.Path)
				}
			})
			mobile.handleReferralAction(context.Background(), managedReferralRequest{Action: referralActionInfo, RequestID: "info-1", AccountRef: referralTestAccount})
			event := referralLastResult(t, output)
			if mode == "expired" {
				if event.string("state") != "ok" || len(paths) != 2 || paths[0] != "/api/mobile/v1/me" {
					t.Fatalf("expired verified account blocked: %+v / %v", event, paths)
				}
			} else if event.string("state") != "error" || event["payload"] != nil {
				t.Fatalf("unsafe code leaked: %+v", event)
			}
			if mode == "unlinked" && len(paths) != 0 {
				t.Fatalf("unlinked own-code read reached network: %v", paths)
			}
		})
	}
}

func TestReferralRegistrationMissingClientPreservesOriginalIntent(t *testing.T) {
	output := &syncBuffer{}
	mobile := &managedMobile{bridge: newManagedBridge(output, "attempt", func() {})}
	request := managedReferralRequest{Action: referralActionRegistration, RequestID: "reg-1", CandidateID: referralTestAccount, IdempotencyKey: "original-registration-key"}
	mobile.handleReferralAction(context.Background(), request)
	event := referralLastResult(t, output)
	if event.string("state") != "error" || event.string("code") != "MOBILE_STATE_UNAVAILABLE" || event.string("idempotency_key") != request.IdempotencyKey || event.string("referral_candidate_id") != request.CandidateID || event["payload"] != nil {
		t.Fatalf("missing registration client produced false success or lost intent: %+v", event)
	}
}

func TestReferralFailuresPreserveCorrelationWithoutSuccess(t *testing.T) {
	for _, scenario := range []struct {
		name, code, body string
		status           int
	}{
		{"unknown", "TRANSPORT", `{"request_id":`, http.StatusOK},
		{"locked", "REFERRAL_CANDIDATE_LOCKED", `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"REFERRAL_CANDIDATE_LOCKED","retryable":false}`, http.StatusConflict},
		{"history", "REFERRAL_HISTORY_PENDING", `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"REFERRAL_HISTORY_PENDING","retryable":true}`, http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, r *http.Request, _ *managedMobile) {
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, scenario.body)
			})
			request := managedReferralRequest{Action: referralActionCandidateSet, RequestID: "set-1", IdempotencyKey: "original-key-0001", Code: "Ab12"}
			mobile.handleReferralAction(context.Background(), request)
			event := referralLastResult(t, output)
			if event.string("state") != "error" || event.string("code") != scenario.code || event.string("idempotency_key") != request.IdempotencyKey || event.string("client_request_id") != request.RequestID || event["payload"] != nil {
				t.Fatalf("failure lost original intent or invented success: %+v", event)
			}
		})
	}
}

func TestReferralCandidateDefinitiveRejectionRequiresExactStatusAndDecodedEnvelope(t *testing.T) {
	for _, scenario := range []struct {
		name, code string
		status     int
		malformed  bool
		retryable  bool
		definitive bool
	}{
		{"invalid404", "REFERRAL_CODE_INVALID", 404, false, false, true},
		{"bad400", "BAD_MESSAGE", 400, false, false, true},
		{"invalid404retryable", "REFERRAL_CODE_INVALID", 404, false, true, false},
		{"bad400retryable", "BAD_MESSAGE", 400, false, true, false},
		{"invalid502", "REFERRAL_CODE_INVALID", 502, false, false, false},
		{"bad409", "BAD_MESSAGE", 409, false, false, false},
		{"locked409", "REFERRAL_CANDIDATE_LOCKED", 409, false, false, false},
		{"conflict409", "IDEMPOTENCY_CONFLICT", 409, false, false, false},
		{"invalid404malformed", "REFERRAL_CODE_INVALID", 404, true, false, false},
		{"invalid404missingID", "REFERRAL_CODE_INVALID", 404, false, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, r *http.Request, _ *managedMobile) {
				w.WriteHeader(scenario.status)
				if scenario.malformed {
					_, _ = io.WriteString(w, `{"code":"REFERRAL_CODE_INVALID"}`)
					return
				}
				serverRequestID := "0123456789abcdef0123456789abcdef"
				if scenario.name == "invalid404missingID" {
					serverRequestID = ""
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"request_id": serverRequestID, "server_time": "2026-10-03T00:00:00Z",
					"schema_version": "1.0", "status": "error", "code": scenario.code, "retryable": scenario.retryable,
				})
			})
			request := managedReferralRequest{Action: referralActionCandidateSet, RequestID: "set-1", IdempotencyKey: "original-key-0001", Code: "Ab12"}
			mobile.handleReferralAction(context.Background(), request)
			event := referralLastResult(t, output)
			definitive, _ := event["definitive_rejection"].(bool)
			if definitive != scenario.definitive || event.string("client_request_id") != request.RequestID || event.string("idempotency_key") != request.IdempotencyKey || event.string("state") != "error" {
				t.Fatalf("unsafe or uncorrelated rejection proof: %+v", event)
			}
			if definitive && (event["http_status"] != float64(scenario.status) || event.string("request_id") != "0123456789abcdef0123456789abcdef") {
				t.Fatalf("rejection proof missing wire identity: %+v", event)
			}
		})
	}
}
