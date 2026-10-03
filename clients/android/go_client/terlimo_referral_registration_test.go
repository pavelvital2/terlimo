package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Wire values are the reviewed ba3934f registration fixtures. The account fixture
// substitutes its own authenticated UUID for the example's cccccccc account.
const nativeRegistrationKey = "registration-key-0001"
const nativeRegistrationProof = `{"candidate_id":"12345678-1234-1234-1234-123456789abc","registration_id":"aaaaaaaa-1234-1234-1234-123456789abc","idempotency_key":"registration-key-0001"}`
const nativeRegistrationPending = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"pending","token":"tok123","bot_username":"terlimo_bot","deep_link":"https://t.me/terlimo_bot?start=tok123","expires_at":"2026-10-03T00:10:00Z","expires_in":600,"referral_registration":` + nativeRegistrationProof + `}}`
const nativeRegistrationRegistered = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"registered","referral_attribution":{"receipt_id":"bbbbbbbb-1234-1234-1234-123456789abc","account_ref":"12345678-1234-1234-1234-123456789abc","candidate_id":"12345678-1234-1234-1234-123456789abc","registration_id":"aaaaaaaa-1234-1234-1234-123456789abc","idempotency_key":"registration-key-0001","state":"attached","reason":null}}}`
const nativeRegistrationExpiry = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"REGISTRATION_EXPIRED","retryable":false,"details":{"state":"expired","referral_registration":` + nativeRegistrationProof + `}}`

func nativeRegistrationRequest() managedReferralRequest {
	return managedReferralRequest{Action: referralActionRegistration, RequestID: "original-registration-flight", CandidateID: referralTestAccount, IdempotencyKey: nativeRegistrationKey}
}

func nativeRegistrationMe(w http.ResponseWriter, linked bool) {
	var me map[string]any
	_ = json.Unmarshal([]byte(meFixtureJSON), &me)
	me["account_ref"], me["telegram_linked"] = referralTestAccount, linked
	_ = json.NewEncoder(w).Encode(me)
}

func TestReferralRegistrationKeyedPendingReplayPreservesFullPayload(t *testing.T) {
	var requests []capturedBridgeRequest
	mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, r *http.Request, _ *managedMobile) {
		raw, _ := io.ReadAll(r.Body)
		requests = append(requests, capturedBridgeRequest{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key"), Auth: r.Header.Get("Authorization"), Body: string(raw)})
		_, _ = io.WriteString(w, nativeRegistrationPending)
	})
	request := nativeRegistrationRequest()
	for i := 0; i < 2; i++ {
		mobile.handleReferralAction(context.Background(), request)
		event := referralLastResult(t, output)
		if event.string("type") != "telegram_registration" || event.string("state") != "pending" || event.string("operation") != "registration" || event.string("idempotency_key") != request.IdempotencyKey || event.string("client_request_id") != request.RequestID || event.string("referral_candidate_id") != request.CandidateID {
			t.Fatalf("keyed correlation lost: %+v", event)
		}
		var wanted map[string]any
		_ = json.Unmarshal([]byte(nativeRegistrationPending), &wanted)
		if !reflect.DeepEqual(event["payload"], wanted) || event.string("deep_link") != "https://t.me/terlimo_bot?start=tok123" {
			t.Fatalf("immutable pending payload changed: %+v", event)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("blind retries or missing keyed request: %+v", requests)
	}
	for _, wire := range requests {
		if wire.Method != "POST" || wire.Path != "/api/mobile/v1/registration/telegram/link" || wire.Key != request.IdempotencyKey || wire.Body != `{"referral_candidate_id":"`+request.CandidateID+`"}` || !strings.HasPrefix(wire.Auth, "Bearer ") {
			t.Fatalf("keyed registration wire changed: %+v", wire)
		}
	}
}

func TestReferralRegistrationRegisteredReceiptRequiresFreshAccount(t *testing.T) {
	for _, mode := range []string{"attached", "rejected", "foreign_receipt", "unlinked_me", "revoked_me", "stale_me", "stale_registration", "missing_receipt"} {
		t.Run(mode, func(t *testing.T) {
			var paths []string
			mobile, output := referralNativeHarness(t, true, func(w http.ResponseWriter, r *http.Request, m *managedMobile) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/api/mobile/v1/registration/telegram/link":
					if mode == "stale_registration" {
						m.session.Refresh()
					}
					raw := nativeRegistrationRegistered
					if mode == "rejected" {
						raw = strings.Replace(raw, `"state":"attached","reason":null`, `"state":"rejected","reason":"self"`, 1)
					}
					if mode == "foreign_receipt" {
						raw = strings.Replace(raw, `"account_ref":"`+referralTestAccount+`"`, `"account_ref":"cccccccc-1234-1234-1234-123456789abc"`, 1)
					}
					if mode == "missing_receipt" {
						raw = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"registered"}}`
					}
					_, _ = io.WriteString(w, raw)
				case "/api/mobile/v1/me":
					if mode == "revoked_me" {
						var me map[string]any
						_ = json.Unmarshal([]byte(meFixtureJSON), &me)
						me["account_ref"], me["account_state"] = referralTestAccount, "REVOKED_SESSION"
						_ = json.NewEncoder(w).Encode(me)
						return
					}
					if mode == "stale_me" {
						m.session.Refresh()
					}
					nativeRegistrationMe(w, mode != "unlinked_me")
				default:
					t.Errorf("unexpected route: %s", r.URL.Path)
				}
			})
			request := nativeRegistrationRequest()
			request.AccountRef = referralTestAccount
			mobile.handleReferralAction(context.Background(), request)
			event := referralLastResult(t, output)
			if mode == "attached" || mode == "rejected" {
				if event.string("state") != "registered" || event.string("account_ref") != request.AccountRef || event.string("captured_account_ref") != request.AccountRef || len(paths) != 2 || paths[1] != "/api/mobile/v1/me" {
					t.Fatalf("verified terminal registration not delivered: %+v / %v", event, paths)
				}
				me := event["fresh_me"].(map[string]any)
				if me["account_ref"] != request.AccountRef || me["telegram_linked"] != true || me["schema_version"] != "1.0" {
					t.Fatalf("fresh account proof invalid: %+v", me)
				}
			} else if event.string("state") != "error" || event["payload"] != nil || event["fresh_me"] != nil {
				t.Fatalf("unverified attribution became terminal: %+v", event)
			}
		})
	}
}

func TestReferralRegistrationFirstRegisteredReplayAndCapturedAccountFence(t *testing.T) {
	for _, mode := range []string{"lost_pending", "foreign_captured"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			mobile, output := referralNativeHarness(t, true, func(w http.ResponseWriter, r *http.Request, _ *managedMobile) {
				if r.URL.Path == "/api/mobile/v1/me" {
					nativeRegistrationMe(w, true)
					return
				}
				calls++
				_, _ = io.WriteString(w, nativeRegistrationRegistered)
			})
			request := nativeRegistrationRequest()
			if mode == "foreign_captured" {
				request.AccountRef = "cccccccc-1234-1234-1234-123456789abc"
			}
			mobile.handleReferralAction(context.Background(), request)
			event := referralLastResult(t, output)
			if mode == "lost_pending" {
				if calls != 1 || event.string("state") != "registered" || event["payload"] == nil || event["fresh_me"] == nil {
					t.Fatalf("first registered replay was lost: %+v", event)
				}
			} else if calls != 0 || event.string("state") != "error" || event.string("code") != "REFERRAL_ACCOUNT_MISMATCH" {
				t.Fatalf("foreign captured account sent mutation: %+v / %d", event, calls)
			}
		})
	}
}

func TestReferralRegistrationLegacyNoCandidateWireUnchanged(t *testing.T) {
	var wire capturedBridgeRequest
	mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, r *http.Request, _ *managedMobile) {
		raw, _ := io.ReadAll(r.Body)
		wire = capturedBridgeRequest{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key"), Body: string(raw)}
		_, _ = io.WriteString(w, nativeRegistrationPending)
	})
	mobile.handleRegistration(context.Background(), "request_telegram_registration")
	event := referralLastResult(t, output)
	if wire.Method != "POST" || wire.Path != "/api/mobile/v1/registration/telegram/link" || wire.Body != "{}" || wire.Key != "" || event.string("state") != "pending" || event["idempotency_key"] != nil || event["operation"] != nil {
		t.Fatalf("legacy request/event changed: %+v / %+v", wire, event)
	}
}

func TestReferralRegistrationExpiryOnlyValidated410ClosesIntent(t *testing.T) {
	for _, scenario := range []struct {
		name, raw string
		status    int
		terminal  bool
	}{
		{"exact", nativeRegistrationExpiry, 410, true},
		{"wrong_status", nativeRegistrationExpiry, 409, false},
		{"foreign_key", strings.Replace(nativeRegistrationExpiry, nativeRegistrationKey, "registration-key-0002", 1), 410, false},
		{"bare", `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"REGISTRATION_EXPIRED","retryable":false}`, 410, false},
		{"missing_request_id", strings.Replace(nativeRegistrationExpiry, `"request_id":"0123456789abcdef0123456789abcdef"`, `"request_id":""`, 1), 410, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, _ *http.Request, _ *managedMobile) {
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, scenario.raw)
			})
			request := nativeRegistrationRequest()
			mobile.handleReferralAction(context.Background(), request)
			event := referralLastResult(t, output)
			terminal, _ := event["referral_registration_expired"].(bool)
			if terminal != scenario.terminal || event.string("state") != "error" || event.string("idempotency_key") != request.IdempotencyKey {
				t.Fatalf("unsafe expiry proof: %+v", event)
			}
			if terminal && (event["http_status"] != float64(410) || event["payload"] == nil) {
				t.Fatalf("expiry envelope lost: %+v", event)
			}
		})
	}
}

func TestReferralRegistrationCancelledAndUnknownRetainOriginalIntent(t *testing.T) {
	for _, mode := range []string{"cancelled", "unknown", "foreign_pending", "generic400"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			mobile, output := referralNativeHarness(t, false, func(w http.ResponseWriter, _ *http.Request, _ *managedMobile) {
				calls++
				switch mode {
				case "unknown":
					_, _ = io.WriteString(w, `{"request_id":`)
				case "foreign_pending":
					_, _ = io.WriteString(w, strings.Replace(nativeRegistrationPending, nativeRegistrationKey, "registration-key-0002", 1))
				case "generic400":
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"BAD_MESSAGE","retryable":false}`)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			request := nativeRegistrationRequest()
			mobile.handleReferralAction(ctx, request)
			if mode == "cancelled" {
				if calls != 0 || output.String() != "" {
					t.Fatal("cancelled flight sent request or event")
				}
				return
			}
			event := referralLastResult(t, output)
			if event.string("state") != "error" || event.string("idempotency_key") != request.IdempotencyKey || event["payload"] != nil || event["definitive_rejection"] != nil || event["referral_registration_expired"] != nil {
				t.Fatalf("unresolved registration was closed: %+v", event)
			}
		})
	}
}
