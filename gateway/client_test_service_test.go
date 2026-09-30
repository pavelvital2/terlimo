package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"

	"wg-turn-client/internal/wlwire"
)

func TestServiceRegLinkTraceClassify(t *testing.T) {
	cases := []struct {
		name string
		tr   serviceRegLinkTrace
		want string
	}{
		{"written_ok", serviceRegLinkTrace{ParseOK: true, Status: 200, PayloadLen: 723}, "written_ok"},
		{"watcher_cancel", serviceRegLinkTrace{RelayCode: "SERVICE_UNAVAILABLE", WatcherCancelled: true}, "watcher_cancel"},
		{"relay_error", serviceRegLinkTrace{RelayErr: "SERVICE_UNAVAILABLE"}, "relay_error"},
		{"write_error", serviceRegLinkTrace{ParseOK: true, Status: 200, PayloadLen: 723, WriteErr: "write udp 127.0.0.1: connection refused"}, "write_error"},
		{"parse_error", serviceRegLinkTrace{ParseOK: false}, "parse_error"},
	}
	for _, c := range cases {
		if got := c.tr.classify(); got != c.want {
			t.Fatalf("%s: classify=%q want %q", c.name, got, c.want)
		}
	}
	// The trace line must remain secret-free: none of these markers may appear.
	full := serviceRegLinkTrace{
		RelayCode: "SERVICE_UNAVAILABLE", RelayErr: "SERVICE_UNAVAILABLE",
		WatcherCancelled: true, ParseOK: false, Status: 200, PayloadLen: 723,
		WriteErr: "write udp: timeout",
	}.String()
	for _, forbidden := range []string{"body", "token", "nonce", "bearer", "deep_link", "tg://", "https://"} {
		if strings.Contains(strings.ToLower(full), forbidden) {
			t.Fatalf("trace leaks %q: %s", forbidden, full)
		}
	}
}

func TestServicePathAllowlistRegistrationLink(t *testing.T) {
	const link = "/api/mobile/v1/registration/telegram/link"
	if !servicePathAllowed("POST", link) {
		t.Fatalf("expected allowed: POST %s", link)
	}
	denied := []struct{ method, path string }{
		{"GET", link},
		{"PUT", link},
		{"DELETE", link},
		{"POST", link + "/"},
		{"POST", link + "/extra"},
		{"POST", "/api/mobile/v1/registration/telegram/confirm"},
		{"POST", "/api/mobile/v1/registration/telegram"},
		{"POST", "/api/mobile/v1/registration/telegram/link2"},
		{"POST", "/api/mobile/v1/registration/telegram/link%2f"},
	}
	for _, c := range denied {
		if servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected denied: %s %s", c.method, c.path)
		}
	}
}

func TestServicePathAllowlistExact(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/auth/challenge"},
		{"POST", "/api/mobile/v1/installations"},
		{"POST", "/api/mobile/v1/auth/session"},
		{"POST", "/api/mobile/v1/access/sync"},
		{"POST", "/api/mobile/v1/onboarding/intents"},
		{"POST", "/api/mobile/v1/registration/telegram/link"},
		{"POST", "/api/mobile/v1/trial/activate"},
		{"GET", "/api/mobile/v1/me"},
		{"GET", "/api/mobile/v1/gateways"},
		{"GET", "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef"},
	}
	for _, c := range allowed {
		if !servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected allowed: %s %s", c.method, c.path)
		}
	}
	denied := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/auth/challenge"},
		{"GET", "/api/mobile/v1/trial/activate"},
		{"POST", "/api/mobile/v1/me"},
		{"GET", "/internal/onboarding/evidence"},
		{"POST", "/api/mobile/v1/onboarding/intents/extra"},
		{"GET", "/api/mobile/v1/operations/not-a-uuid"},
		{"GET", "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcde"},
		{"GET", "http://host/api/mobile/v1/me"},
		{"POST", "/api/mobile/v1/../../etc/passwd"},
		{"CONNECT", "host:443"},
	}
	for _, c := range denied {
		if servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected denied: %s %s", c.method, c.path)
		}
	}
	if serviceValidateHeaders(map[string]string{"X-Forwarded-Host": "evil"}) {
		t.Fatal("hop-by-hop header accepted")
	}
	if serviceValidateHeaders(map[string]string{"Authorization": "Bearer a\r\nInjected: x"}) {
		t.Fatal("CRLF header accepted")
	}
	if !serviceValidateHeaders(map[string]string{"Authorization": "Bearer token", "Idempotency-Key": "k"}) {
		t.Fatal("allowed headers rejected")
	}
}

func TestServiceClassifierCollisionFailClosed(t *testing.T) {
	store := newWrapKeyStore()
	if err := store.SetPasswords("main-secret", []string{"pass-one"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetServiceClassifier("pass-one"); err == nil {
		t.Fatal("service classifier colliding with an active password accepted")
	}
	if err := store.SetServiceClassifier("main-secret"); err == nil {
		t.Fatal("service classifier colliding with the main password accepted")
	}
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	if identity := store.serviceAccessIdentity(); !identity.isService || identity.password != "public-classifier-seed" {
		t.Fatalf("bad service identity: %+v", identity)
	}
	if err := store.SetPasswords("main-secret", []string{"public-classifier-seed"}); err == nil {
		t.Fatal("keyset refresh with colliding password accepted")
	}
	if err := store.AddPassword("public-classifier-seed"); err == nil {
		t.Fatal("provisioning a colliding password accepted")
	}
	if err := store.SetPasswords("main-secret", []string{"other-pass"}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceWireGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "service_wire_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	var req serviceRequest
	if err := wlwire.StrictJSON(fixture.Request, &req); err != nil {
		t.Fatalf("golden request: %v", err)
	}
	if req.V != 1 || req.Op != "service.http" || req.Method != "GET" || req.Path != "/api/mobile/v1/me" {
		t.Fatalf("golden request mismatch: %+v", req)
	}
	var resp serviceResponse
	if err := wlwire.StrictJSON(fixture.Response, &resp); err != nil {
		t.Fatalf("golden response: %v", err)
	}
	if resp.V != 1 || resp.Status != 200 {
		t.Fatalf("golden response mismatch: %+v", resp)
	}
	var errFrame serviceErrorFrame
	if err := wlwire.StrictJSON(fixture.Error, &errFrame); err != nil {
		t.Fatalf("golden error: %v", err)
	}
	if errFrame.V != 1 || errFrame.Error.Code == "" {
		t.Fatalf("golden error mismatch: %+v", errFrame)
	}
}

type serviceDTLSPair struct {
	server net.Listener
	client net.Conn
	done   chan struct{}
}

func startServiceDTLS(t *testing.T, ctx context.Context, identity accessIdentity) *serviceDTLSPair {
	return startServiceDTLSWith(t, ctx, ctx, identity)
}

func startServiceDTLSWith(t *testing.T, handshakeCtx, serveCtx context.Context, identity accessIdentity) *serviceDTLSPair {
	t.Helper()
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := dtls.Listen("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &dtls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if conn.(*dtls.Conn).HandshakeContext(handshakeCtx) != nil {
			return
		}
		clientTestServiceServe(serveCtx, conn, identity)
	}()
	client, err := dtls.Dial("udp4", listener.Addr().(*net.UDPAddr), &dtls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.HandshakeContext(handshakeCtx); err != nil {
		t.Fatal(err)
	}
	return &serviceDTLSPair{server: listener, client: client, done: done}
}

func (p *serviceDTLSPair) close(t *testing.T) {
	if p.client != nil {
		_ = p.client.Close()
	}
	if p.server != nil {
		_ = p.server.Close()
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("service handler did not return")
	}
}

func serviceRequestBody(t *testing.T, id wlwire.ID, method, path string, headers map[string]string, body []byte) []byte {
	t.Helper()
	req := serviceRequest{
		V:         1,
		Op:        "service.http",
		RequestID: hex.EncodeToString(id[:]),
		Method:    method,
		Path:      path,
		Query:     "",
		Headers:   headers,
		BodyB64:   base64.RawURLEncoding.EncodeToString(body),
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func serviceWriteRequest(t *testing.T, c net.Conn, id wlwire.ID, payload []byte) {
	t.Helper()
	frames, err := wlwire.ServiceFrames(id, false, payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if _, err = c.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
}

func serviceReadResponse(t *testing.T, c net.Conn) (wlwire.ID, []byte) {
	t.Helper()
	var assembler wlwire.ServiceAssembler
	buf := make([]byte, 32+wlwire.ServiceFragment+1)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	for {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n < 6 || buf[5] != 1 {
			t.Fatal("response flag")
		}
		id, body, err := assembler.Add(buf[:n], time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			return id, body
		}
	}
}

// Larger than the WLBS MaxBody (16 KiB) request and larger than the evidence 256 KiB
// response ceiling, over a real DTLS channel with the service codec.
func TestServiceRelayDTLSLargePayload(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()

	requestBody := bytes.Repeat([]byte("q"), 20*1024)
	responseBody := bytes.Repeat([]byte("r"), 300*1024)
	socket := filepath.Join(t.TempDir(), "service.sock")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	unix, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	go func() {
		up, err := unix.Accept()
		if err != nil {
			return
		}
		defer up.Close()
		var req serviceRequest
		if json.NewDecoder(up).Decode(&req) != nil {
			return
		}
		resp := serviceResponse{
			V:         1,
			RequestID: req.RequestID,
			Status:    200,
			Headers:   map[string]string{"Content-Type": "application/json"},
			BodyB64:   base64.RawURLEncoding.EncodeToString(responseBody),
		}
		_ = json.NewEncoder(up).Encode(resp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, identity)
	defer pair.close(t)

	var id wlwire.ID
	id[0] = 1
	serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, "POST", "/api/mobile/v1/access/sync", map[string]string{"Authorization": "Bearer session"}, requestBody))
	respID, body := serviceReadResponse(t, pair.client)
	if respID != id {
		t.Fatal("response id mismatch")
	}
	var resp serviceResponse
	if err := wlwire.StrictJSON(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d", resp.Status)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(resp.BodyB64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, responseBody) {
		t.Fatalf("response body mismatch: %d bytes", len(decoded))
	}
}

func TestServiceSuccessfulExchangeReleasesSlotBeforeClientClose(t *testing.T) {
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
		_ = json.NewEncoder(up).Encode(serviceResponse{
			V: 1, RequestID: req.RequestID, Status: 200,
			Headers: map[string]string{"Content-Type": "application/json"}, BodyB64: "",
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)
	var id wlwire.ID
	id[0] = 9
	serviceWriteRequest(t, pair.client, id,
		serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
	if gotID, _ := serviceReadResponse(t, pair.client); gotID != id {
		t.Fatal("response id mismatch")
	}
	// Keep the client connection open: a completed one-shot exchange must no
	// longer occupy the per-source slot while the next auth call starts.
	waitInFlight(t, 0)
}

func TestServiceBoundedSequentialSession(t *testing.T) {
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
	backendDone := make(chan struct{})
	backendSawTransportMode := make(chan bool, 2)
	go func() {
		defer close(backendDone)
		for i := 0; i < 2; i++ {
			up, acceptErr := unix.Accept()
			if acceptErr != nil {
				return
			}
			var req serviceRequest
			if json.NewDecoder(up).Decode(&req) == nil {
				backendSawTransportMode <- req.SessionMode != ""
				_ = json.NewEncoder(up).Encode(serviceResponse{
					V: 1, RequestID: req.RequestID, Status: 200,
					Headers: map[string]string{}, BodyB64: "",
				})
			}
			_ = up.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)
	for i := byte(1); i <= 2; i++ {
		var id wlwire.ID
		id[0] = i
		var req serviceRequest
		if err := json.Unmarshal(serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil), &req); err != nil {
			t.Fatal(err)
		}
		req.SessionMode = "bounded"
		payload, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		serviceWriteRequest(t, pair.client, id, payload)
		gotID, _ := serviceReadResponse(t, pair.client)
		if gotID != id {
			t.Fatalf("response %d id mismatch", i)
		}
	}
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("backend did not receive both requests")
	}
	for i := 0; i < 2; i++ {
		if <-backendSawTransportMode {
			t.Fatal("transport session mode leaked to backend API")
		}
	}
}

func TestServiceRelayUnavailableAndDeniedFrames(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Denied path: rejected before any relay dial, error frame carries request_id.
	pair := startServiceDTLS(t, ctx, identity)
	var id wlwire.ID
	id[0] = 2
	serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, "GET", "/internal/onboarding/evidence", nil, nil))
	_, body := serviceReadResponse(t, pair.client)
	var errFrame serviceErrorFrame
	if err := wlwire.StrictJSON(body, &errFrame); err != nil {
		t.Fatal(err)
	}
	if errFrame.Error.Code != "SERVICE_PATH_DENIED" || errFrame.RequestID != hex.EncodeToString(id[:]) {
		t.Fatalf("unexpected frame: %+v", errFrame)
	}
	pair.close(t)

	// Missing backend socket: bounded transport error frame, never HTTP success.
	pair2 := startServiceDTLS(t, ctx, identity)
	var id2 wlwire.ID
	id2[0] = 3
	serviceWriteRequest(t, pair2.client, id2, serviceRequestBody(t, id2, "GET", "/api/mobile/v1/me", nil, nil))
	_, body2 := serviceReadResponse(t, pair2.client)
	var errFrame2 serviceErrorFrame
	if err := wlwire.StrictJSON(body2, &errFrame2); err != nil {
		t.Fatal(err)
	}
	if errFrame2.Error.Code != "SERVICE_UNAVAILABLE" || !errFrame2.Error.Retryable {
		t.Fatalf("unexpected error frame: %+v", errFrame2)
	}
	pair2.close(t)
}

func TestServiceCancellationWithoutAllocation(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, identity)
	var id wlwire.ID
	id[0] = 4
	// Announce a large frame and send only its first fragment, then close.
	frames, err := wlwire.ServiceFrames(id, false, bytes.Repeat([]byte("z"), 40*1024))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pair.client.Write(frames[0]); err != nil {
		t.Fatal(err)
	}
	_ = pair.client.Close()
	pair.client = nil
	pair.close(t) // asserts the handler returns promptly
}

func waitInFlight(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if serviceGlobalLimiter().inFlight() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("limiter in-flight = %d, want %d", serviceGlobalLimiter().inFlight(), want)
}

func TestServiceCancelDuringIncompleteFrameReleasesSlot(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	ctx, cancel := context.WithCancel(context.Background())
	pair := startServiceDTLS(t, ctx, identity)
	frames, err := wlwire.ServiceFrames(svcTestID(5), false, bytes.Repeat([]byte("z"), 40*1024))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pair.client.Write(frames[0]); err != nil {
		t.Fatal(err)
	}
	waitInFlight(t, 1) // admission slot is held before/without a full frame
	start := time.Now()
	cancel()
	pair.close(t)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("handler did not stop promptly after cancel: %s", elapsed)
	}
	if got := serviceGlobalLimiter().inFlight(); got != 0 {
		t.Fatalf("slot leaked after cancel: in-flight=%d", got)
	}
}

func TestServiceCancelDuringBackendStallReleasesSlot(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	socket := filepath.Join(t.TempDir(), "service.sock")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	unix, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		up, err := unix.Accept()
		if err != nil {
			return
		}
		defer up.Close()
		var req serviceRequest
		if json.NewDecoder(up).Decode(&req) != nil {
			return
		}
		close(accepted)
		<-release // stall: no response until the test cancels
	}()

	ctx, cancel := context.WithCancel(context.Background())
	pair := startServiceDTLS(t, ctx, identity)
	id := svcTestID(6)
	serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("backend never accepted the relayed request")
	}
	waitInFlight(t, 1)
	start := time.Now()
	cancel()
	pair.close(t)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("handler did not stop promptly after cancel during backend stall: %s", elapsed)
	}
	if got := serviceGlobalLimiter().inFlight(); got != 0 {
		t.Fatalf("slot leaked after backend-stall cancel: in-flight=%d", got)
	}
	close(release)
}

func TestServicePreCanceledCtxSkipsAdmissionAndCloses(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	hsCtx, cancelHS := context.WithCancel(context.Background())
	defer cancelHS()
	serveCtx, cancelServe := context.WithCancel(context.Background())
	cancelServe() // pre-canceled: admission must be skipped, no slot, no IO
	pair := startServiceDTLSWith(t, hsCtx, serveCtx, identity)
	pair.close(t)
	if got := serviceGlobalLimiter().inFlight(); got != 0 {
		t.Fatalf("pre-canceled serve took a slot: in-flight=%d", got)
	}
}

func TestServiceCancelCloseIsTerminalOverControlledConn(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		clientTestServiceServe(ctx, server, identity)
	}()
	waitInFlight(t, 1) // admission slot taken before any read
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
	if got := serviceGlobalLimiter().inFlight(); got != 0 {
		t.Fatalf("slot leaked: in-flight=%d", got)
	}
	// Close is terminal: a later deadline reset can no longer revive IO on this conn.
	if err := server.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		t.Fatal("closed session conn accepted a new read deadline")
	}
	_ = server.Close()
}

func svcTestID(n byte) wlwire.ID {
	var id wlwire.ID
	id[0] = n
	return id
}

func TestServiceRelayReplyUnionAndSingleFrame(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	identity := store.serviceAccessIdentity()

	run := func(t *testing.T, backend func(req serviceRequest) []byte) serviceErrorFrame {
		t.Helper()
		socket := filepath.Join(t.TempDir(), "service.sock")
		t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
		unix, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close()
		go func() {
			up, err := unix.Accept()
			if err != nil {
				return
			}
			defer up.Close()
			var req serviceRequest
			if json.NewDecoder(up).Decode(&req) != nil {
				return
			}
			_, _ = up.Write(backend(req))
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pair := startServiceDTLS(t, ctx, identity)
		defer pair.close(t)
		id := svcTestID(7)
		serviceWriteRequest(t, pair.client, id, serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
		_, body := serviceReadResponse(t, pair.client)
		var errFrame serviceErrorFrame
		if err := wlwire.StrictJSON(body, &errFrame); err != nil {
			t.Fatal(err)
		}
		return errFrame
	}

	// Backend error frame is propagated as a bounded transport error, not wrapped as success.
	frame := run(t, func(req serviceRequest) []byte {
		out, _ := json.Marshal(serviceErrorFrame{V: 1, RequestID: req.RequestID, Error: serviceErrorDetail{Code: "SERVICE_BACKEND_DENIED", Retryable: false}})
		return append(out, '\n')
	})
	if frame.Error.Code != "SERVICE_BACKEND_DENIED" || frame.Error.Retryable {
		t.Fatalf("error union not propagated: %+v", frame)
	}

	// A trailing second frame is rejected (exactly one newline-terminated frame).
	frame = run(t, func(req serviceRequest) []byte {
		out, _ := json.Marshal(serviceResponse{V: 1, RequestID: req.RequestID, Status: 200, Headers: map[string]string{}, BodyB64: ""})
		return append(append(out, '\n'), []byte("{\"v\":1}\n")...)
	})
	if frame.Error.Code != "SERVICE_BAD_RESPONSE" {
		t.Fatalf("trailing second frame accepted: %+v", frame)
	}
}

func TestServiceCollisionBootstrapProvisionRejectedBeforeSave(t *testing.T) {
	t.Setenv("WL_TEST_ENABLED", "1")
	seed := strings.Repeat("s", 40)
	oldKeys, oldDB := serverWrapKeys, db
	serverWrapKeys = newWrapKeyStore()
	db = &Database{ClientBootstrap: map[string]ClientTestBootstrap{}, MainPassword: "main"}
	t.Cleanup(func() { serverWrapKeys, db = oldKeys, oldDB })
	if err := serverWrapKeys.SetServiceClassifier(seed); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw, err := json.Marshal(clientTestCommand{
		Operation: "bootstrap_provision",
		Bootstrap: ClientTestBootstrap{CredentialID: "svc-collision", Secret: seed, ExpiresAt: time.Now().Add(time.Minute).Unix()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = clientTestAdmin(dir, raw, nil); err == nil || err.Error() != "CONFLICT" {
		t.Fatalf("colliding provision must fail closed: %v", err)
	}
	if len(db.ClientBootstrap) != 0 {
		t.Fatal("collision left an in-memory bootstrap entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "passwords.json")); err == nil {
		t.Fatal("collision persisted passwords.json")
	}
}

func TestServiceCollisionRefreshEntrypointFailsClosed(t *testing.T) {
	seed := strings.Repeat("s", 40)
	oldKeys, oldDB := serverWrapKeys, db
	serverWrapKeys = newWrapKeyStore()
	t.Cleanup(func() { serverWrapKeys, db = oldKeys, oldDB })
	if err := serverWrapKeys.SetServiceClassifier(seed); err != nil {
		t.Fatal(err)
	}
	db = &Database{
		MainPassword: "main",
		ClientBootstrap: map[string]ClientTestBootstrap{
			"collide": {CredentialID: "collide", Secret: seed, ExpiresAt: time.Now().Add(time.Minute).Unix()},
		},
	}
	if err := refreshWrapKeysFromDBLocked(); err == nil {
		t.Fatal("refresh with a colliding bootstrap secret must fail closed")
	}
}

func TestServiceTracePathSelection(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/registration/telegram/link"},
		{"GET", "/api/mobile/v1/gateways"},
		{"POST", "/api/mobile/v1/access/sync"},
	}
	for _, c := range allowed {
		if !serviceTracePath(c.method, c.path) {
			t.Fatalf("expected traced: %s %s", c.method, c.path)
		}
	}
	denied := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/registration/telegram/link"},
		{"POST", "/api/mobile/v1/gateways"},
		{"GET", "/api/mobile/v1/access/sync"},
		{"GET", "/api/mobile/v1/me"},
		{"POST", "/api/mobile/v1/onboarding/intents"},
		{"GET", "/api/mobile/v1/gateways/extra"},
	}
	for _, c := range denied {
		if serviceTracePath(c.method, c.path) {
			t.Fatalf("expected untraced: %s %s", c.method, c.path)
		}
	}
}

func TestServiceReadErrClass(t *testing.T) {
	if got := serviceReadErrClass(nil); got != "" {
		t.Fatalf("nil class=%q", got)
	}
	if got := serviceReadErrClass(wlwire.ErrMessage); got != "BAD_HEADER" {
		t.Fatalf("header class=%q", got)
	}
	if got := serviceReadErrClass(io.EOF); got != "EOF" {
		t.Fatalf("eof class=%q", got)
	}
	if got := serviceReadErrClass(errors.New("boom")); got != "OTHER" {
		t.Fatalf("other class=%q", got)
	}
}

// The service-channel gate must accept exactly GET /api/mobile/v1/usage and keep every
// wrong-method / near-match path denied; the diagnostic trace gate follows the same route
// so the next run can be correlated in [SVCREG] logs.
func TestServicePathAllowedUsageRoute(t *testing.T) {
	if !servicePathAllowed("GET", "/api/mobile/v1/usage") {
		t.Fatal("GET /api/mobile/v1/usage must be allowed")
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if servicePathAllowed(method, "/api/mobile/v1/usage") {
			t.Fatalf("%s /api/mobile/v1/usage must be denied", method)
		}
	}
	for _, path := range []string{
		"/api/mobile/v1/usage/",
		"/api/mobile/v1/usage/extra",
		"/api/mobile/v1/usages",
		"/api/mobile/v1/usage/../me",
		"/api/mobile/internal/usage",
	} {
		if servicePathAllowed("GET", path) {
			t.Fatalf("near-match %q must be denied", path)
		}
	}
	if !servicePathAllowed("GET", "/api/mobile/v1/me") || servicePathAllowed("POST", "/api/mobile/v1/me") {
		t.Fatal("existing /me boundary changed")
	}
}

func TestServiceValidateRequestUsageAllowed(t *testing.T) {
	var id wlwire.ID
	id[0] = 3
	if _, code := serviceValidateRequest(serviceRequestBody(t, id, "GET", "/api/mobile/v1/usage", nil, nil), id); code != "" {
		t.Fatalf("valid usage frame rejected with %q", code)
	}
	if _, code := serviceValidateRequest(serviceRequestBody(t, id, "POST", "/api/mobile/v1/usage", nil, nil), id); code != "SERVICE_PATH_DENIED" {
		t.Fatalf("POST usage frame code=%q, want SERVICE_PATH_DENIED", code)
	}
	// Denial happens before the gated trace: untraced paths produce no [SVCREG] lines,
	// which is exactly why the pre-fix rejection was silent in node logs.
	if serviceTracePath("GET", "/api/mobile/v1/private") {
		t.Fatal("unrelated path must not be traced")
	}
}

func TestServiceTracePathUsageGate(t *testing.T) {
	if !serviceTracePath("GET", "/api/mobile/v1/usage") {
		t.Fatal("usage route must be visible in [SVCREG] trace after the gate fix")
	}
	if serviceTracePath("POST", "/api/mobile/v1/usage") {
		t.Fatal("only GET usage may be traced")
	}
}

func TestServicePathAllowlistDevices(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/devices"},
		{"DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef"},
		{"DELETE", "/api/mobile/v1/devices/01234567-89AB-CDEF-0123-456789ABCDEF"},
	}
	for _, c := range allowed {
		if !servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected allowed: %s %s", c.method, c.path)
		}
	}
	denied := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/devices"},
		{"PUT", "/api/mobile/v1/devices"},
		{"GET", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef"},
		{"DELETE", "/api/mobile/v1/devices"},
		{"DELETE", "/api/mobile/v1/devices/"},
		{"DELETE", "/api/mobile/v1/devices/not-a-uuid"},
		{"DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcde"},
		{"DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef/extra"},
		{"DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef/"},
		{"DELETE", "/api/mobile/v1/devices/../me"},
		{"DELETE", "/api/mobile/v1/me"},
		{"DELETE", "/api/mobile/v1/devicesx/01234567-89ab-cdef-0123-456789abcdef"},
		{"DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef/../me"},
	}
	for _, c := range denied {
		if servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected denied: %s %s", c.method, c.path)
		}
	}
	// The existing /me boundary and GET devices stay method-exact.
	if servicePathAllowed("POST", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef") {
		t.Fatal("POST on device id must be denied")
	}
}

func TestServiceValidateRequestDevices(t *testing.T) {
	var id wlwire.ID
	id[0] = 7
	headers := map[string]string{"Authorization": "Bearer synthetic-token", "Idempotency-Key": "idem-1"}
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	allowed := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/devices"},
		{"DELETE", "/api/mobile/v1/devices/" + uuid},
	}
	for _, c := range allowed {
		if _, code := serviceValidateRequest(serviceRequestBody(t, id, c.method, c.path, headers, nil), id); code != "" {
			t.Fatalf("valid %s %s frame rejected with %q", c.method, c.path, code)
		}
	}
	denied := []struct{ method, path, want string }{
		{"POST", "/api/mobile/v1/devices", "SERVICE_PATH_DENIED"},
		{"GET", "/api/mobile/v1/devices/" + uuid, "SERVICE_PATH_DENIED"},
		{"DELETE", "/api/mobile/v1/devices", "SERVICE_PATH_DENIED"},
		{"DELETE", "/api/mobile/v1/devices/not-a-uuid", "SERVICE_PATH_DENIED"},
		{"DELETE", "/api/mobile/v1/devices/" + uuid + "/", "SERVICE_PATH_DENIED"},
		{"DELETE", "/api/mobile/v1/devices/" + uuid + "/extra", "SERVICE_PATH_DENIED"},
		{"DELETE", "/api/mobile/v1/devices/../me", "SERVICE_BAD_PATH"},
		{"DELETE", "/api/mobile/v1/me", "SERVICE_PATH_DENIED"},
		{"PUT", "/api/mobile/v1/devices/" + uuid, "SERVICE_BAD_METHOD"},
	}
	for _, c := range denied {
		if _, code := serviceValidateRequest(serviceRequestBody(t, id, c.method, c.path, headers, nil), id); code != c.want {
			t.Fatalf("%s %s code=%q want %q", c.method, c.path, code, c.want)
		}
	}
	// Existing operations keep their exact framing and method gates.
	regression := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/auth/challenge"},
		{"POST", "/api/mobile/v1/auth/session"},
		{"GET", "/api/mobile/v1/me"},
		{"GET", "/api/mobile/v1/gateways"},
		{"GET", "/api/mobile/v1/usage"},
		{"POST", "/api/mobile/v1/access/sync"},
	}
	for _, c := range regression {
		if _, code := serviceValidateRequest(serviceRequestBody(t, id, c.method, c.path, nil, nil), id); code != "" {
			t.Fatalf("regression %s %s rejected with %q", c.method, c.path, code)
		}
	}
}

func TestServicePathAllowlistAnnouncements(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/announcements"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef/read"},
		{"POST", "/api/mobile/v1/announcements/01234567-89AB-CDEF-0123-456789ABCDEF/read"},
	}
	for _, c := range allowed {
		if !servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected allowed: %s %s", c.method, c.path)
		}
	}
	denied := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/announcements"},
		{"PUT", "/api/mobile/v1/announcements"},
		{"DELETE", "/api/mobile/v1/announcements"},
		{"GET", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef/read"},
		{"POST", "/api/mobile/v1/announcements/"},
		{"POST", "/api/mobile/v1/announcements/not-a-uuid/read"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcde/read"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef/read/"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef/read/extra"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef/READ"},
		{"POST", "/api/mobile/v1/announcements/01234567-89ab-cdef-0123-456789abcdef/../read"},
		{"POST", "/api/mobile/v1/announcements/../me"},
		{"POST", "/api/mobile/v1/announcementsx/01234567-89ab-cdef-0123-456789abcdef/read"},
	}
	for _, c := range denied {
		if servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected denied: %s %s", c.method, c.path)
		}
	}
}

func TestServiceValidateRequestAnnouncements(t *testing.T) {
	var id wlwire.ID
	id[0] = 8
	headers := map[string]string{"Authorization": "Bearer synthetic-token", "Idempotency-Key": "idem-1"}
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	allowed := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/announcements"},
		{"POST", "/api/mobile/v1/announcements/" + uuid + "/read"},
	}
	for _, c := range allowed {
		if _, code := serviceValidateRequest(serviceRequestBody(t, id, c.method, c.path, headers, nil), id); code != "" {
			t.Fatalf("valid %s %s frame rejected with %q", c.method, c.path, code)
		}
	}
	denied := []struct{ method, path, want string }{
		{"POST", "/api/mobile/v1/announcements", "SERVICE_PATH_DENIED"},
		{"GET", "/api/mobile/v1/announcements/" + uuid + "/read", "SERVICE_PATH_DENIED"},
		{"POST", "/api/mobile/v1/announcements/not-a-uuid/read", "SERVICE_PATH_DENIED"},
		{"POST", "/api/mobile/v1/announcements/" + uuid + "/read/", "SERVICE_PATH_DENIED"},
		{"POST", "/api/mobile/v1/announcements/" + uuid + "/extra", "SERVICE_PATH_DENIED"},
		{"POST", "/api/mobile/v1/announcements/../me", "SERVICE_BAD_PATH"},
		{"PUT", "/api/mobile/v1/announcements/" + uuid + "/read", "SERVICE_BAD_METHOD"},
	}
	for _, c := range denied {
		if _, code := serviceValidateRequest(serviceRequestBody(t, id, c.method, c.path, headers, nil), id); code != c.want {
			t.Fatalf("%s %s code=%q want %q", c.method, c.path, code, c.want)
		}
	}
	// Announcement frames keep the fixed bounded diagnostic class.
	if class := serviceFrameClass("/api/mobile/v1/announcements", ""); class != "ANNOUNCEMENTS" {
		t.Fatalf("GET announcements class=%q", class)
	}
	if class := serviceFrameClass("/api/mobile/v1/announcements/"+uuid+"/read", ""); class != "ANNOUNCEMENTS" {
		t.Fatalf("POST read class=%q", class)
	}
}

func TestServicePathAllowlistCheckoutSession(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"POST", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef/checkout-session"},
		{"POST", "/api/mobile/v1/payments/AbC.def-123_XY~z9/checkout-session"},
	}
	for _, c := range allowed {
		if !servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected allowed: %s %s", c.method, c.path)
		}
	}
	denied := []struct{ method, path string }{
		{"GET", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef/checkout-session"},
		{"DELETE", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef/checkout-session"},
		{"POST", "/api/mobile/v1/payments/checkout-session"},
		{"POST", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef/checkout-session/"},
		{"POST", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef/checkout"},
		{"POST", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef"},
		{"POST", "/api/mobile/v1/paymentsx/01234567-89ab-cdef-0123-456789abcdef/checkout-session"},
		{"POST", "/api/mobile/v1/payments/" + strings.Repeat("a", 129) + "/checkout-session"},
		{"POST", "/api/mobile/v1/payments/..%2Fme/checkout-session"},
	}
	for _, c := range denied {
		if servicePathAllowed(c.method, c.path) {
			t.Fatalf("expected denied: %s %s", c.method, c.path)
		}
	}
	// Validator keeps the global path gates ahead of the allowlist.
	var id wlwire.ID
	id[0] = 9
	if _, code := serviceValidateRequest(serviceRequestBody(t, id, "POST", "/api/mobile/v1/payments/../me/checkout-session", nil, nil), id); code != "SERVICE_BAD_PATH" {
		t.Fatalf("traversal code=%q", code)
	}
	if _, code := serviceValidateRequest(serviceRequestBody(t, id, "POST", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef/checkout-session", map[string]string{"Idempotency-Key": "idem-1"}, nil), id); code != "" {
		t.Fatalf("valid checkout frame rejected: %q", code)
	}
}
