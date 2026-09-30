package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"wg-turn-client/internal/wlwire"
)

func TestS5PlansForwardedByGatewayDTLS(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "service.sock")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	unix, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	seen := make(chan serviceRequest, 1)
	go func() {
		up, acceptErr := unix.Accept()
		if acceptErr != nil {
			return
		}
		defer up.Close()
		var req serviceRequest
		if json.NewDecoder(up).Decode(&req) != nil {
			return
		}
		seen <- req
		_ = json.NewEncoder(up).Encode(serviceResponse{
			V: 1, RequestID: req.RequestID, Status: 200,
			Headers: map[string]string{"Content-Type": "application/json"},
			BodyB64: base64.RawURLEncoding.EncodeToString([]byte(`{"plans":[]}`)),
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)
	var id wlwire.ID
	id[0] = 25
	serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, "GET", "/api/mobile/v1/plans", nil, nil))
	gotID, body := serviceReadResponse(t, pair.client)
	if gotID != id {
		t.Fatal("response id mismatch")
	}
	var resp serviceResponse
	if err := wlwire.StrictJSON(body, &resp); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(resp.BodyB64)
	if err != nil || resp.Status != 200 || !bytes.Equal(decoded, []byte(`{"plans":[]}`)) {
		t.Fatalf("gateway response status=%d decode_error=%v", resp.Status, err)
	}
	select {
	case req := <-seen:
		if req.Method != "GET" || req.Path != "/api/mobile/v1/plans" || req.RequestID != resp.RequestID {
			t.Fatal("wrong relay request")
		}
	case <-time.After(time.Second):
		t.Fatal("gateway did not forward plans to relay")
	}
}

func TestS5ServicePathsExact(t *testing.T) {
	// payment_orders.id is generated as a UUID and emitted in this canonical form.
	const paymentID = "550e8400-e29b-41d4-a716-446655440000"
	allowed := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/plans"},
		{"POST", "/api/mobile/v1/quotes"},
		{"POST", "/api/mobile/v1/payments"},
		{"GET", "/api/mobile/v1/payments/" + paymentID},
		{"POST", "/api/mobile/v1/payments/" + paymentID + "/checkout-session"},
		{"POST", "/api/mobile/v1/payments/order_A-123/checkout-session"},
	}
	for _, item := range allowed {
		if !servicePathAllowed(item.method, item.path) {
			t.Fatalf("expected allowed: %s %s", item.method, item.path)
		}
	}
	denied := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/plans"},
		{"GET", "/api/mobile/v1/quotes"},
		{"GET", "/api/mobile/v1/payments"},
		{"POST", "/api/mobile/v1/payments/" + paymentID},
		{"GET", "/api/mobile/v1/payments/not-a-uuid"},
		{"GET", "/api/mobile/v1/payments/order_A-123"},
		{"GET", "/api/mobile/v1/payments/" + paymentID + "/extra"},
		{"GET", "/api/mobile/v1/payments/" + paymentID + "%2fextra"},
		{"GET", "/api/mobile/v1/payments/../me"},
		{"GET", "/api/mobile/v1/payments/" + paymentID + "/checkout-session"},
		{"POST", "/api/mobile/v1/payments/checkout-session"},
		{"POST", "/api/mobile/v1/payments/" + paymentID + "/checkout-session/"},
		{"GET", "/api/mobile/v1/plans/"},
		{"GET", "/api/mobile/v1/plans/extra"},
		{"GET", "/api/mobile/v1/payment/" + paymentID},
	}
	for _, item := range denied {
		if servicePathAllowed(item.method, item.path) {
			t.Fatalf("expected denied: %s %s", item.method, item.path)
		}
	}
}
