package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"wg-turn-client/internal/wlwire"
)

func TestServiceReferralGate(t *testing.T) {
	var id wlwire.ID
	id[0] = 11
	for _, c := range []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/api/mobile/v1/referral", true},
		{"POST", "/api/mobile/v1/referral/candidate", true},
		{"DELETE", "/api/mobile/v1/referral/candidate", true},
		{"POST", "/api/mobile/v1/referral", false},
		{"DELETE", "/api/mobile/v1/referral", false},
		{"GET", "/api/mobile/v1/referral/candidate", false},
		{"PUT", "/api/mobile/v1/referral/candidate", false},
		{"PATCH", "/api/mobile/v1/referral/candidate", false},
		{"GET", "/api/mobile/v1/referral/", false},
		{"DELETE", "/api/mobile/v1/referral/candidate/", false},
		{"POST", "/api/mobile/v1/referral/%63andidate", false},
		{"GET", "/api/mobile/v1/referral/../me", false},
		{"GET", "/api/mobile/v1/referral//", false},
		{"GET", "https://foreign.test/api/mobile/v1/referral", false},
		{"POST", "/api/mobile/v1/internal/telegram/referral", false},
		{"POST", "/api/mobile/v1/internal/telegram/account", false},
		{"POST", "/api/mobile/v1/internal/telegram/billing", false},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if got := servicePathAllowed(c.method, c.path); got != c.allowed {
				t.Errorf("gate=%v want %v", got, c.allowed)
			}
			_, code := serviceValidateRequest(serviceRequestBody(t, id, c.method, c.path, map[string]string{"Authorization": "Bearer synthetic", "Idempotency-Key": "referral-k"}, nil), id)
			if (code == "") != c.allowed {
				t.Errorf("validate code=%q allowed=%v", code, c.allowed)
			}
		})
	}
}

// The existing DTLS + Unix relay harness exercises the actual gateway gate,
// not a direct call to the fake backend. Byte equality preserves account/body fields.
func TestServiceReferralForward(t *testing.T) {
	info, err := os.ReadFile("testdata/referral_info.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path   string
		body, response []byte
	}{
		{"GET", "/api/mobile/v1/referral", nil, info},
		{"POST", "/api/mobile/v1/referral/candidate", []byte(`{"code":"Synthetic42"}`), []byte(`{"request_id":"0123456789abcdef0123456789abcdef","candidate":{"id":"11111111-1111-4111-8111-111111111111","state":"pending","code":"Synthetic42"}}`)},
		{"DELETE", "/api/mobile/v1/referral/candidate", nil, []byte(`{"request_id":"0123456789abcdef0123456789abcdef","candidate":{"state":"cleared"}}`)},
	} {
		t.Run(c.method, func(t *testing.T) {
			t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
			t.Setenv("WL_TEST_SERVICE_SEED", "synthetic-referral-seed")
			store := newWrapKeyStore()
			if err := store.SetServiceClassifier("synthetic-referral-seed"); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(t.TempDir(), "service.sock")
			t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			seen := make(chan serviceRequest, 1)
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				var req serviceRequest
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				seen <- req
				_ = json.NewEncoder(conn).Encode(serviceResponse{V: 1, RequestID: req.RequestID, Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, BodyB64: base64.RawURLEncoding.EncodeToString(c.response)})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
			defer pair.close(t)
			var id wlwire.ID
			id[0] = 12
			serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, c.method, c.path, map[string]string{"Authorization": "Bearer synthetic", "Idempotency-Key": "referral-k"}, c.body))
			gotID, raw := serviceReadResponse(t, pair.client)
			if gotID != id {
				t.Fatal("response id mismatch")
			}
			var resp serviceResponse
			if err := wlwire.StrictJSON(raw, &resp); err != nil {
				t.Fatal(err)
			}
			body, err := base64.RawURLEncoding.Strict().DecodeString(resp.BodyB64)
			if err != nil || resp.Status != 200 || !bytes.Equal(body, c.response) {
				t.Fatalf("response changed status=%d err=%v", resp.Status, err)
			}
			select {
			case req := <-seen:
				body, err := base64.RawURLEncoding.Strict().DecodeString(req.BodyB64)
				if err != nil || !bytes.Equal(body, c.body) || req.Path != c.path || req.Method != c.method || req.Headers["Authorization"] != "Bearer synthetic" || req.Headers["Idempotency-Key"] != "referral-k" {
					t.Fatal("forwarded request changed")
				}
			case <-ctx.Done():
				t.Fatal("backend not reached")
			}
		})
	}
}
