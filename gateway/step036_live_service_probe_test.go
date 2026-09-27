package main

// Operator-only synthetic probe: exercises the LIVE node's real client service-http
// path (WRAP/DTLS -> node service classifier -> service handler -> backend relay
// socket). No credentials, no phone, no registration link is created; the request is
// deliberately unauthenticated so the backend is expected to answer 401.
import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"wg-turn-client/internal/wlwire"
)

func TestStep036LiveServiceProbe(t *testing.T) {
	if os.Getenv("STEP036_LIVE_SERVICE_PROBE") != "1" {
		t.Skip("explicit TEST operator live probe only")
	}
	addr := os.Getenv("STEP036_LIVE_SERVICE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:57500"
	}
	seed := os.Getenv("STEP036_LIVE_SERVICE_SEED")
	if seed == "" {
		t.Fatal("STEP036_LIVE_SERVICE_SEED required")
	}
	path := os.Getenv("STEP036_LIVE_SERVICE_PATH")
	if path == "" {
		path = "/api/mobile/v1/registration/telegram/link"
	}
	key, err := deriveWrapKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &wrapPacketConn{inner: udp, key: key}
	atomic.StoreInt32(&wrapped.selected, 1)
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dtls.Client(wrapped, raddr, &dtls.Config{
		InsecureSkipVerify:    true,
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.RandomCIDGenerator(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.HandshakeContext(ctx); err != nil {
		t.Fatalf("live node WRAP/DTLS handshake: %v", err)
	}
	t.Log("live node handshake ok")

	var id wlwire.ID
	id[0] = 1
	payload := serviceRequestBody(t, id, "POST", path, map[string]string{"Content-Type": "application/json"}, []byte("{}"))
	serviceWriteRequest(t, c, id, payload)
	respID, body := serviceReadResponse(t, c)
	if respID != id {
		t.Fatal("response id mismatch")
	}
	var errFrame serviceErrorFrame
	if err := wlwire.StrictJSON(body, &errFrame); err == nil && errFrame.Error.Code != "" {
		t.Fatalf("node error frame (did NOT reach backend): %s", errFrame.Error.Code)
	}
	var resp serviceResponse
	if err := wlwire.StrictJSON(body, &resp); err != nil {
		t.Fatalf("response decode: %v", err)
	}
	t.Logf("reached backend: status=%d path=%s", resp.Status, path)
	if resp.Status == 503 {
		t.Fatalf("backend unavailable: status=%d", resp.Status)
	}
	if resp.Status != 401 {
		t.Fatalf("expected 401 SESSION_INVALID, got %d", resp.Status)
	}
}
