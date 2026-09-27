package servicechannel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

// TestGoldenServiceWireCompatibility proves the client seam accepts the authoritative
// shared golden document byte-shape: the request passes the bounded request contract,
// the frames round-trip through the shared codec, and both reply shapes classify
// exactly once through the strict response/error union.
func TestGoldenServiceWireCompatibility(t *testing.T) {
	raw, err := os.ReadFile("testdata/service_wire_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Request  requestFrame  `json:"request"`
		Response responseFrame `json:"response"`
		Error    errorFrame    `json:"error"`
	}
	if err := wlwire.StrictJSON(raw, &golden); err != nil {
		t.Fatalf("golden strict decode: %v", err)
	}
	if golden.Request.V != 1 || golden.Request.Op != "service.http" {
		t.Fatalf("golden request shape changed: %+v", golden.Request)
	}
	idBytes, err := hex.DecodeString(golden.Request.RequestID)
	if err != nil || len(idBytes) != 16 {
		t.Fatalf("golden request_id is not hex32: %q", golden.Request.RequestID)
	}
	var id wlwire.ID
	copy(id[:], idBytes)

	requestRaw, err := json.Marshal(golden.Request)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := validateRequest(requestRaw, id)
	if err != nil {
		t.Fatalf("golden request rejected by the bounded contract: %v", err)
	}
	if frame.Method != "GET" || frame.Path != "/api/mobile/v1/me" ||
		frame.Headers["Authorization"] != "Bearer golden" || frame.BodyB64 != "" {
		t.Fatalf("golden request fields changed: %+v", frame)
	}

	frames, err := wlwire.ServiceFrames(id, false, requestRaw)
	if err != nil {
		t.Fatalf("golden request framing: %v", err)
	}
	var assembler wlwire.ServiceAssembler
	var assembled []byte
	for _, fragment := range frames {
		replyID, body, err := assembler.Add(fragment, time.Now())
		if err != nil {
			t.Fatalf("golden reassembly: %v", err)
		}
		if body != nil {
			if replyID != id {
				t.Fatalf("golden frame id changed: %x", replyID)
			}
			assembled = body
		}
	}
	if !bytes.Equal(assembled, requestRaw) {
		t.Fatal("golden request did not round-trip through the shared codec")
	}

	responseRaw, err := json.Marshal(golden.Response)
	if err != nil {
		t.Fatal(err)
	}
	reply, serviceErr, err := parseServiceReply(golden.Request.RequestID, responseRaw)
	if err != nil || serviceErr != nil {
		t.Fatalf("golden response rejected: %v / %v", err, serviceErr)
	}
	if reply.status != 200 || reply.headers.Get("Content-Type") != "application/json" ||
		string(reply.body) != `{"v":1}` {
		t.Fatalf("golden response mapped unexpectedly: %+v %q", reply, reply.body)
	}

	errorRaw, err := json.Marshal(golden.Error)
	if err != nil {
		t.Fatal(err)
	}
	if _, serviceErr, err = parseServiceReply(golden.Request.RequestID, errorRaw); err != nil || serviceErr == nil {
		t.Fatalf("golden error frame not classified: %v / %v", err, serviceErr)
	}
	if serviceErr.Code != "SERVICE_UNAVAILABLE" || !serviceErr.Retryable {
		t.Fatalf("golden error frame changed: %+v", serviceErr)
	}
}

func TestParseServiceReplyStrictUnion(t *testing.T) {
	id := mustID(t, "000102030405060708090a0b0c0d0e0f")
	valid := responseJSON(t, id, 200, map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`))
	if _, serviceErr, err := parseServiceReply(RequestID(id), valid); err != nil || serviceErr != nil {
		t.Fatalf("valid reply rejected: %v %v", err, serviceErr)
	}

	cases := []struct {
		name string
		raw  []byte
	}{
		{"wrong request id", func() []byte {
			var frame responseFrame
			_ = json.Unmarshal(valid, &frame)
			frame.RequestID = "ffffffffffffffffffffffffffffffff"
			raw, _ := json.Marshal(frame)
			return raw
		}()},
		{"status below range", responseJSON(t, id, 99, nil, nil)},
		{"status above range", responseJSON(t, id, 600, nil, nil)},
		{"unknown header", responseJSON(t, id, 200, map[string]string{"Server": "x"}, nil)},
		{"oversized header value", responseJSON(t, id, 200, map[string]string{"Content-Type": strings.Repeat("a", 1025)}, nil)},
		{"bad base64 body", []byte(`{"v":1,"request_id":"000102030405060708090a0b0c0d0e0f","status":200,"headers":{},"body_b64":"!!!not-base64!!!"}`)},
		{"both unions", []byte(`{"v":1,"request_id":"000102030405060708090a0b0c0d0e0f","status":200,"headers":{},"body_b64":"","error":{"code":"X","retryable":false}}`)},
		{"extra field", []byte(`{"v":1,"request_id":"000102030405060708090a0b0c0d0e0f","status":200,"headers":{},"body_b64":"","extra":1}`)},
		{"empty error code", errorJSON(t, id, "", false)},
		{"long error code", errorJSON(t, id, strings.Repeat("c", 65), false)},
		{"error with extra field", []byte(`{"v":1,"request_id":"000102030405060708090a0b0c0d0e0f","error":{"code":"X","retryable":false},"extra":1}`)},
	}
	for _, testCase := range cases {
		if _, _, err := parseServiceReply(RequestID(id), testCase.raw); !errors.Is(err, ErrResponseRejected) {
			t.Fatalf("%s: expected ErrResponseRejected, got %v", testCase.name, err)
		}
	}

	// A decoded response body above 1 MiB must be rejected from its encoded size alone.
	oversize := responseJSON(t, id, 200, nil, bytes.Repeat([]byte{0x61}, maxResponseBody+1))
	if _, _, err := parseServiceReply(RequestID(id), oversize); !errors.Is(err, ErrResponseRejected) {
		t.Fatalf("oversize response accepted: %v", err)
	}

	// A decoded response body of exactly 1 MiB is allowed.
	exact := responseJSON(t, id, 200, nil, bytes.Repeat([]byte{0x61}, maxResponseBody))
	if _, _, err := parseServiceReply(RequestID(id), exact); err != nil {
		t.Fatalf("1 MiB response rejected: %v", err)
	}
}

func TestValidateRequestBounds(t *testing.T) {
	id := mustID(t, "000102030405060708090a0b0c0d0e0f")
	base := requestFrame{
		V:         1,
		Op:        "service.http",
		RequestID: RequestID(id),
		Method:    "POST",
		Path:      "/api/mobile/v1/access/sync",
		Query:     "",
		Headers:   map[string]string{"Content-Type": "application/json", "Authorization": "Bearer t"},
	}
	build := func(mutate func(*requestFrame)) (requestFrame, error) {
		frame := base
		frame.Headers = map[string]string{}
		for name, value := range base.Headers {
			frame.Headers[name] = value
		}
		mutate(&frame)
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		return validateRequest(raw, id)
	}

	if _, err := build(func(*requestFrame) {}); err != nil {
		t.Fatalf("base request rejected: %v", err)
	}
	// Exactly 64 KiB of decoded body is accepted; one byte more is rejected.
	if _, err := build(func(frame *requestFrame) {
		frame.BodyB64 = encodeRawURL(bytes.Repeat([]byte{0x61}, maxRequestBody))
	}); err != nil {
		t.Fatalf("64 KiB request rejected: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.BodyB64 = encodeRawURL(bytes.Repeat([]byte{0x61}, maxRequestBody+1))
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("oversized request accepted: %v", err)
	}
	// Oversized encoded body is rejected before decoding.
	if _, err := build(func(frame *requestFrame) {
		frame.BodyB64 = strings.Repeat("A", encodedLimit(maxRequestBody)+1)
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("oversized encoded request accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.BodyB64 = "!!!"
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("bad base64 request accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.RequestID = "ABCDEF00010203040506070809ABCDEF"
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("non-hex32 request_id accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Path = "/api/mobile/v1/me"
		frame.Method = "GET"
		frame.Query = "?" + strings.Repeat("q", maxPathQuery)
	}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("oversized path+query accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Path = "/api/mobile/v1/admin"
	}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("unknown path accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Headers["X-Unknown"] = "1"
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("unknown header accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Headers["Authorization"] = "Bearer a\r\nInjected: 1"
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("CRLF header accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Headers["Authorization"] = strings.Repeat("a", 1025)
	}); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("oversized header value accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Path = "/api/mobile/v1/operations/not-a-uuid"
		frame.Method = "GET"
	}); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("non-uuid operation accepted: %v", err)
	}
	if _, err := build(func(frame *requestFrame) {
		frame.Path = "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef"
		frame.Method = "GET"
	}); err != nil {
		t.Fatalf("uuid operation rejected: %v", err)
	}
}

// TestHeaderAggregateCapUnderFixedAllowlist documents the aggregate 8 KiB header cap:
// the fixed allowlist itself can never reach it (four request names, two response
// names, each value capped at 1024), so the cap stays a strict enforced bound that a
// future allowlist expansion cannot silently exceed.
func TestHeaderAggregateCapUnderFixedAllowlist(t *testing.T) {
	requestTotal := 0
	for name := range requestHeaderAllowed {
		requestTotal += len(name) + maxHeaderValue
	}
	if requestTotal >= maxHeaders {
		t.Fatalf("request allowlist can exceed the aggregate cap: %d >= %d", requestTotal, maxHeaders)
	}
	responseTotal := 0
	for name := range responseHeaderAllowed {
		responseTotal += len(name) + maxHeaderValue
	}
	if responseTotal >= maxHeaders {
		t.Fatalf("response allowlist can exceed the aggregate cap: %d >= %d", responseTotal, maxHeaders)
	}
	headers := map[string]string{}
	for name := range requestHeaderAllowed {
		headers[name] = strings.Repeat("v", maxHeaderValue)
	}
	if !validRequestHeaders(headers) {
		t.Fatal("maximal allowed request header set rejected")
	}
}

func mustID(t *testing.T, value string) wlwire.ID {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad test id %q", value)
	}
	var id wlwire.ID
	copy(id[:], raw)
	return id
}
