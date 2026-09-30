package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wg-turn-client/internal/wlwire"
)

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *lockedLogBuffer {
	t.Helper()
	target := &lockedLogBuffer{}
	previous := log.Writer()
	log.SetOutput(target)
	t.Cleanup(func() { log.SetOutput(previous) })
	return target
}

func startSuccessServiceStub(t *testing.T, socket string) {
	t.Helper()
	unix, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close() })
	go func() {
		for attempt := 0; attempt < 4; attempt++ {
			up, acceptErr := unix.Accept()
			if acceptErr != nil {
				return
			}
			var req serviceRequest
			if json.NewDecoder(up).Decode(&req) == nil {
				_ = json.NewEncoder(up).Encode(serviceResponse{
					V: 1, RequestID: req.RequestID, Status: 200,
					Headers: map[string]string{"Content-Type": "application/json"}, BodyB64: "",
				})
			}
			_ = up.Close()
		}
	}()
}

func TestServiceTimeMarksReceiveForwardReply(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "service.sock")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	startSuccessServiceStub(t, socket)
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)

	var id wlwire.ID
	id[0] = 9
	serviceWriteRequest(t, pair.client, id,
		serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
	gotID, body := serviceReadResponse(t, pair.client)
	if gotID != id {
		t.Fatal("response id mismatch")
	}
	var resp serviceResponse
	if err := wlwire.StrictJSON(body, &resp); err != nil || resp.Status != 200 {
		t.Fatalf("unexpected response: %v %v", err, resp.Status)
	}

	text := logs.String()
	for _, expected := range []string{
		"[SVCTIME] recv", "class=ME", "seq=1",
		"[SVCTIME] fwd_begin", "[SVCTIME] fwd_end", "relay_code=\"\"",
		"[SVCTIME] write_begin", "kind=response", "[SVCTIME] write_end",
		"elapsed_ms=",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in time marks:\n%s", expected, text)
		}
	}
	for _, forbidden := range []string{"Authorization", "Bearer", "password", "token="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("time marks leaked %q", forbidden)
		}
	}
}

func TestServiceTimeMarksBadFrameFixedError(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)

	var id wlwire.ID
	id[0] = 4
	serviceWriteRequest(t, pair.client, id,
		serviceRequestBody(t, id, "PATCH", "/api/mobile/v1/me", nil, nil))
	_, body := serviceReadResponse(t, pair.client)
	var errFrame serviceErrorFrame
	if err := wlwire.StrictJSON(body, &errFrame); err != nil {
		t.Fatal(err)
	}
	if errFrame.Error.Code != "SERVICE_BAD_METHOD" {
		t.Fatalf("unexpected error code %q", errFrame.Error.Code)
	}

	text := logs.String()
	if strings.Contains(text, "[SVCTIME] fwd_begin") {
		t.Fatalf("invalid frame must not reach forward marks:\n%s", text)
	}
	for _, expected := range []string{
		"[SVCTIME] recv", "class=invalid", "code=\"SERVICE_BAD_METHOD\"",
		"kind=error code=\"SERVICE_BAD_METHOD\"", "result=attempted",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in time marks:\n%s", expected, text)
		}
	}
	if strings.Contains(text, hex.EncodeToString(id[:])) {
		t.Fatalf("raw request id must not appear in time marks")
	}
}

func TestServiceTimeMarksAuthClassAndLocalCounter(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "service.sock")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	startSuccessServiceStub(t, socket)
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)
	for index, path := range []string{"/api/mobile/v1/auth/challenge", "/api/mobile/v1/auth/session"} {
		var id wlwire.ID
		id[0] = byte(20 + index)
		req := serviceRequest{
			V: 1, Op: "service.http", SessionMode: "bounded",
			RequestID: hex.EncodeToString(id[:]),
			Method:    "POST", Path: path, Query: "",
			Headers: map[string]string{}, BodyB64: "",
		}
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		serviceWriteRequest(t, pair.client, id, encoded)
		if _, body := serviceReadResponse(t, pair.client); len(body) == 0 {
			t.Fatal("empty response")
		}
	}
	text := logs.String()
	if !strings.Contains(text, "class=AUTH") || !strings.Contains(text, "seq=1") {
		t.Fatalf("auth class/counter marks missing:\n%s", text)
	}
}

func svctimeLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "[SVCTIME]") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestServiceTimeMarksBoundedMethodAndAttemptedWrite(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	const sentinel = "SVC_SENTINEL_XYZ"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cases := []struct {
		id      byte
		method  string
		version int
		code    string
	}{
		{id: 31, method: "GET\n" + sentinel, version: 1, code: "SERVICE_BAD_METHOD"},
		{id: 32, method: sentinel + strings.Repeat("A", 4096), version: 2, code: "SERVICE_BAD_FRAME"},
	}
	for _, item := range cases {
		pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
		var id wlwire.ID
		id[0] = item.id
		req := serviceRequest{
			V: item.version, Op: "service.http", RequestID: hex.EncodeToString(id[:]),
			Method: item.method, Path: "/api/mobile/v1/me", Query: "",
			Headers: map[string]string{}, BodyB64: "",
		}
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		serviceWriteRequest(t, pair.client, id, encoded)
		_, body := serviceReadResponse(t, pair.client)
		var errFrame serviceErrorFrame
		if err := wlwire.StrictJSON(body, &errFrame); err != nil {
			t.Fatal(err)
		}
		if errFrame.Error.Code != item.code {
			t.Fatalf("frame %d: code=%q want %q", item.id, errFrame.Error.Code, item.code)
		}
		pair.close(t)
	}

	text := logs.String()
	lines := svctimeLines(text)
	if len(lines) == 0 {
		t.Fatal("no SVCTIME marks")
	}
	if strings.Contains(text, sentinel) {
		t.Fatalf("sentinel leaked into logs:\n%s", text)
	}
	if !strings.Contains(text, "method=INVALID") {
		t.Fatalf("hostile method was not bounded:\n%s", text)
	}
	if strings.Contains(text, "result=written") {
		t.Fatalf("void error write must not claim written:\n%s", text)
	}
	if !strings.Contains(text, "result=attempted") {
		t.Fatalf("expected attempted mark for error write:\n%s", text)
	}
	for _, line := range lines {
		if strings.ContainsAny(strings.TrimSuffix(line, "\n"), "\r") {
			t.Fatalf("control character in SVCTIME line: %q", line)
		}
	}
}

func TestServiceTimeMarksClosedConnKeepsAttemptedOnly(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	var id wlwire.ID
	id[0] = 41
	serviceWriteRequest(t, pair.client, id,
		serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
	// Close before the relay's bounded failure: the error write cannot succeed.
	pair.close(t)
	time.Sleep(200 * time.Millisecond)

	text := logs.String()
	if strings.Contains(text, "result=written") {
		t.Fatalf("closed connection reported written:\n%s", text)
	}
}

func startRelayErrorStub(t *testing.T, socket, code string) {
	t.Helper()
	unix, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close() })
	go func() {
		for attempt := 0; attempt < 4; attempt++ {
			up, acceptErr := unix.Accept()
			if acceptErr != nil {
				return
			}
			var req serviceRequest
			if json.NewDecoder(up).Decode(&req) == nil {
				_ = json.NewEncoder(up).Encode(serviceErrorFrame{
					V: 1, RequestID: req.RequestID,
					Error: serviceErrorDetail{Code: code, Retryable: false},
				})
			}
			_ = up.Close()
		}
	}()
}

func TestServiceTimeMarksRelayErrorCodeBounded(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", "public-classifier-seed")
	store := newWrapKeyStore()
	if err := store.SetServiceClassifier("public-classifier-seed"); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "service.sock")
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", socket)
	const sentinel = "SVC_SENTINEL_XYZ"
	hostile := sentinel + "\nX"
	if len(hostile) > 64 {
		t.Fatal("fixture too long")
	}
	startRelayErrorStub(t, socket, hostile)
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := startServiceDTLS(t, ctx, store.serviceAccessIdentity())
	defer pair.close(t)

	var id wlwire.ID
	id[0] = 51
	serviceWriteRequest(t, pair.client, id,
		serviceRequestBody(t, id, "GET", "/api/mobile/v1/me", nil, nil))
	_, body := serviceReadResponse(t, pair.client)
	var errFrame serviceErrorFrame
	if err := wlwire.StrictJSON(body, &errFrame); err != nil {
		t.Fatal(err)
	}
	if errFrame.Error.Code != hostile {
		t.Fatalf("wire code changed: got %q want %q", errFrame.Error.Code, hostile)
	}

	text := logs.String()
	if strings.Contains(text, sentinel) {
		t.Fatalf("relay error sentinel leaked into logs:\n%s", text)
	}
	if !strings.Contains(text, `code="OTHER"`) {
		t.Fatalf("relay error code was not bounded in SVCTIME:\n%s", text)
	}
}
