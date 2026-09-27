package accountaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func injectTrial(t *testing.T, me string, block map[string]any) string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(me), &object); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	object["trial"] = block
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestMeTrialBlockStrict(t *testing.T) {
	me, err := DecodeMeStrict([]byte(injectTrial(t, hourMeActive, map[string]any{
		"state": "available", "can_activate": true, "reason": nil, "starts_at": nil, "ends_at": nil})))
	if err != nil || !me.Trial.CanActivate || me.Trial.State != "available" {
		t.Fatalf("available trial: %+v %v", me.Trial, err)
	}
	active, err := DecodeMeStrict([]byte(injectTrial(t, hourMeActive, map[string]any{
		"state": "active", "can_activate": false, "reason": nil,
		"starts_at": "2026-09-24T10:00:00Z", "ends_at": "2026-10-01T10:00:00Z"})))
	if err != nil || active.Trial.State != "active" || active.Trial.EndsAt == nil {
		t.Fatalf("active trial: %+v %v", active.Trial, err)
	}
	// Absent trial block (S3-A backend) stays valid.
	if _, err := DecodeMeStrict([]byte(hourMeActive)); err != nil {
		t.Fatalf("absent trial must stay compatible: %v", err)
	}
	// Unknown state and an active state without both instants are rejected.
	if _, err := DecodeMeStrict([]byte(injectTrial(t, hourMeActive, map[string]any{
		"state": "weird", "can_activate": false, "reason": nil, "starts_at": nil, "ends_at": nil}))); err == nil {
		t.Fatal("unknown trial state must be rejected")
	}
	if _, err := DecodeMeStrict([]byte(injectTrial(t, hourMeActive, map[string]any{
		"state": "active", "can_activate": false, "reason": nil, "starts_at": nil, "ends_at": nil}))); err == nil {
		t.Fatal("active trial without instants must be rejected")
	}
}

func TestTrialActivationStrictReplayAndShape(t *testing.T) {
	ok := `{"request_id":"0123456789abcdef0123456789abcdef","schema_version":"1.0","status":"ok",` +
		`"trial":{"state":"active","replay":false,"starts_at":"2026-09-24T10:00:00Z",` +
		`"ends_at":"2026-10-01T10:00:00Z","days":7,"account_state":"ACTIVE_TRIAL"}}`
	activation, err := DecodeTrialActivationStrict([]byte(ok))
	if err != nil || activation.State != "active" || activation.Replay || activation.Days != 7 {
		t.Fatalf("activation: %+v %v", activation, err)
	}
	replay := `{"request_id":"0123456789abcdef0123456789abcdef","schema_version":"1.0","status":"ok",` +
		`"trial":{"state":"active","replay":true,"starts_at":"2026-09-24T10:00:00Z",` +
		`"ends_at":"2026-10-01T10:00:00Z","days":7,"account_state":"ACTIVE_TRIAL"}}`
	if replayed, err := DecodeTrialActivationStrict([]byte(replay)); err != nil || !replayed.Replay {
		t.Fatalf("replay must decode with replay=true: %+v %v", replayed, err)
	}
	bad := `{"request_id":"0123456789abcdef0123456789abcdef","schema_version":"1.0","status":"ok",` +
		`"trial":{"state":"active","replay":false,"starts_at":"2026-09-24T10:00:00Z",` +
		`"ends_at":"2026-10-01T10:00:00Z","days":3,"account_state":"ACTIVE_TRIAL"}}`
	if _, err := DecodeTrialActivationStrict([]byte(bad)); err == nil {
		t.Fatal("non-7-day activation must be rejected")
	}
}

func TestActivateTrialClientErrorsAreFixedCodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-24T10:00:00Z",` +
			`"schema_version":"1.0","status":"error","code":"CHANNEL_MEMBERSHIP_REQUIRED","retryable":false}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("b")}
	_, apiError, err := client.ActivateTrial(context.Background())
	if err != nil || apiError == nil || apiError.Code != "CHANNEL_MEMBERSHIP_REQUIRED" {
		t.Fatalf("channel-required must surface as a fixed code: %+v %v", apiError, err)
	}
}

func TestProjectionCarriesTrialDisplaySubset(t *testing.T) {
	reason := "hour_expired_before_registration"
	projection := buildProjection(MeResponse{
		Trial: meTrial{State: "ineligible", CanActivate: false, Reason: &reason},
	}, 1, nil)
	payload := projection.Payload()
	trial, ok := payload["trial"].(map[string]any)
	if !ok {
		t.Fatalf("trial missing from payload: %v", payload["trial"])
	}
	if trial["state"] != "ineligible" || trial["can_activate"] != false ||
		trial["reason"] != "hour_expired_before_registration" {
		t.Fatalf("trial payload wrong: %v", trial)
	}
}

func TestMeTrialBlockAcceptsReplayKey(t *testing.T) {
	// Server /me.trial now carries an additive `replay` key; strict decode must accept it.
	withReplay := injectTrial(t, hourMeActive, map[string]any{
		"state": "available", "can_activate": true, "reason": nil, "starts_at": nil, "ends_at": nil, "replay": false})
	if _, err := DecodeMeStrict([]byte(withReplay)); err != nil {
		t.Fatalf("/me.trial with replay must decode: %v", err)
	}
	bad := injectTrial(t, hourMeActive, map[string]any{
		"state": "available", "can_activate": true, "reason": nil, "starts_at": nil, "ends_at": nil, "replay": "x"})
	if _, err := DecodeMeStrict([]byte(bad)); err == nil {
		t.Fatal("non-boolean replay must be rejected")
	}
}
