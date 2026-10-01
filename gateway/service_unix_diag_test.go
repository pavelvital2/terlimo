package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"wg-turn-client/internal/wlwire"
)

// Functional transport fixture, not a reproduction of the runtime AUTH failure.
type unixDiagConn struct {
	readErr, writeErr, deadlineErr error
	payload                        []byte
	written                        bytes.Buffer
	reads, writes                  int
	readSizes                      []int
	deadlines                      []time.Time
	deadlineAt                     time.Time
	cancel                         context.CancelFunc
	closed                         chan struct{}
	once                           sync.Once
}

func (c *unixDiagConn) Read(b []byte) (int, error) {
	c.reads++
	c.readSizes = append(c.readSizes, len(b))
	if c.cancel != nil {
		<-c.closed
		return 0, net.ErrClosed
	}
	if len(c.payload) > 0 {
		n := copy(b, c.payload)
		c.payload = c.payload[n:]
		return n, nil
	}
	return 0, c.readErr
}
func (c *unixDiagConn) Write(b []byte) (int, error) {
	c.writes++
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	c.written.Write(b)
	if c.cancel != nil {
		c.cancel()
	}
	return len(b), nil
}
func (c *unixDiagConn) Close() error       { c.once.Do(func() { close(c.closed) }); return nil }
func (*unixDiagConn) LocalAddr() net.Addr  { return &net.UnixAddr{Net: "unix"} }
func (*unixDiagConn) RemoteAddr() net.Addr { return &net.UnixAddr{Net: "unix"} }
func (c *unixDiagConn) SetDeadline(d time.Time) error {
	c.deadlineAt = time.Now()
	c.deadlines = append(c.deadlines, d)
	return c.deadlineErr
}
func (*unixDiagConn) SetReadDeadline(time.Time) error  { panic("new read deadline") }
func (*unixDiagConn) SetWriteDeadline(time.Time) error { panic("new write deadline") }

type unixDiagTimeout struct{}

func (unixDiagTimeout) Error() string   { return "secret-fixture-timeout" }
func (unixDiagTimeout) Timeout() bool   { return true }
func (unixDiagTimeout) Temporary() bool { return true }

func TestServiceUnixDiagTransportParity(t *testing.T) {
	old := serviceFrameCapture
	t.Cleanup(func() { serviceFrameCapture = old })
	req := serviceRequest{V: 1, Op: "http", RequestID: strings.Repeat("a", 32), Method: "POST", Path: "/api/mobile/v1/auth/challenge", Headers: map[string]string{"Authorization": "secret-header"}, BodyB64: "secret-body"}
	cases := []struct {
		name, stage, outcome, ctx, code string
		retry                           bool
		makeConn                        func(*unixDiagConn)
	}{
		{name: "prectx", stage: "PRECTX", outcome: "CANCELED", ctx: "CANCELED", code: "SERVICE_UNAVAILABLE", retry: true},
		{name: "prectx_deadline", stage: "PRECTX", outcome: "DEADLINE", ctx: "DEADLINE", code: "SERVICE_UNAVAILABLE", retry: true},
		{name: "socket_path", stage: "SOCKET_PATH", outcome: "OTHER", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true},
		{name: "dial", stage: "DIAL", outcome: "OTHER", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true},
		{name: "encode", stage: "ENCODE", outcome: "OTHER", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true, makeConn: func(c *unixDiagConn) { c.writeErr = errors.New("secret-encode-error") }},
		{name: "read_eof", stage: "READSLICE", outcome: "EOF", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true, makeConn: func(c *unixDiagConn) { c.readErr = io.EOF }},
		{name: "read_timeout", stage: "READSLICE", outcome: "TIMEOUT", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true, makeConn: func(c *unixDiagConn) { c.readErr = unixDiagTimeout{} }},
		{name: "read_deadline", stage: "READSLICE", outcome: "DEADLINE", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true, makeConn: func(c *unixDiagConn) { c.readErr = os.ErrDeadlineExceeded }},
		{name: "ctx_close", stage: "READSLICE", outcome: "CLOSED", ctx: "CANCELED", code: "SERVICE_UNAVAILABLE", retry: true},
		{name: "deadline_set_error", stage: "READSLICE", outcome: "EOF", ctx: "NONE", code: "SERVICE_UNAVAILABLE", retry: true, makeConn: func(c *unixDiagConn) { c.deadlineErr = errors.New("secret-deadline-error"); c.readErr = io.EOF }},
		{name: "buffer_full", code: "SERVICE_BAD_RESPONSE", retry: true, makeConn: func(c *unixDiagConn) {
			c.payload = bytes.Repeat([]byte("x"), wlwire.ServiceMaxFrame+1)
			c.readErr = io.EOF
		}},
		{name: "parse_bad", code: "SERVICE_BAD_RESPONSE", retry: true, makeConn: func(c *unixDiagConn) { c.payload = []byte("not-json\n"); c.readErr = io.EOF }},
		{name: "success", makeConn: func(c *unixDiagConn) {
			c.payload = []byte(`{"v":1,"request_id":"` + req.RequestID + `","status":200,"headers":{},"body_b64":""}` + "\n")
			c.readErr = io.EOF
		}},
		{name: "parsed_error", makeConn: func(c *unixDiagConn) {
			c.payload = []byte(`{"v":1,"request_id":"` + req.RequestID + `","error":{"code":"SERVICE_UNAVAILABLE","retryable":true}}` + "\n")
			c.readErr = io.EOF
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := serviceDiagLog(t)
			var offWritten []byte
			var offReadSizes []int
			var offReads, offWrites int
			var offResp serviceResponse
			var offErr *serviceErrorFrame
			for _, on := range []bool{false, true} {
				logs.Reset()
				serviceFrameCapture = newServiceFrameCapture(on, time.Now())
				t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", "/secret-fixture.sock")
				ctx, cancel := context.WithCancel(context.Background())
				if tc.name == "prectx" {
					cancel()
				}
				if tc.name == "prectx_deadline" {
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				}
				if tc.name == "socket_path" {
					t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", "secret-relative-path")
				}
				c := &unixDiagConn{closed: make(chan struct{})}
				if tc.makeConn != nil {
					tc.makeConn(c)
				}
				if tc.name == "ctx_close" {
					c.cancel = cancel
				}
				dialCalls := 0
				dial := func(_ context.Context, network, path string) (net.Conn, error) {
					dialCalls++
					if network != "unix" || path != "/secret-fixture.sock" {
						t.Fatal("dial arguments changed")
					}
					if tc.name == "dial" {
						return nil, errors.New("secret-dial-error")
					}
					return c, nil
				}
				resp, errFrame, code, retry := serviceRelayWithDial(ctx, req, dial)
				cancel()
				if code != tc.code || retry != tc.retry {
					t.Fatalf("return changed: %s %t", code, retry)
				}
				if tc.name == "success" && resp.Status != 200 {
					t.Fatal("lost response")
				}
				if tc.name == "parsed_error" && (errFrame == nil || errFrame.Error.Code != "SERVICE_UNAVAILABLE" || !errFrame.Error.Retryable) {
					t.Fatal("lost parsed error")
				}
				early := strings.HasPrefix(tc.name, "prectx") || tc.name == "socket_path"
				if early && dialCalls != 0 {
					t.Fatal("early return dialed")
				}
				if !early && dialCalls != 1 {
					t.Fatal("dial count changed")
				}
				if !early && tc.name != "dial" {
					if len(c.deadlines) != 1 {
						t.Fatal("deadline count changed")
					}
					budget := c.deadlines[0].Sub(c.deadlineAt)
					if budget < serviceIODeadline-time.Second || budget > serviceIODeadline {
						t.Fatalf("deadline changed: %v", budget)
					}
					select {
					case <-c.closed:
					default:
						t.Fatal("connection not closed")
					}
				}
				text := logs.String()
				if !on {
					if text != "" {
						t.Fatal("OFF output")
					}
					offWritten = append([]byte(nil), c.written.Bytes()...)
					offReadSizes = append([]int(nil), c.readSizes...)
					offReads, offWrites = c.reads, c.writes
					offResp, offErr = resp, errFrame
				} else {
					if !bytes.Equal(offWritten, c.written.Bytes()) || !reflect.DeepEqual(offReadSizes, c.readSizes) || offReads != c.reads || offWrites != c.writes || !reflect.DeepEqual(offResp, resp) || !reflect.DeepEqual(offErr, errFrame) {
						t.Fatal("ON transport parity failed")
					}
					if tc.stage == "" {
						if text != "" {
							t.Fatal("new marker for buffer/parse/success")
						}
					} else {
						if strings.Count(text, "phase=unix_terminal") != 1 || strings.Count(text, "\n") != 1 {
							t.Fatalf("marker count: %q", text)
						}
						for _, want := range []string{"stage=" + tc.stage, "outcome=" + tc.outcome, "ctx=" + tc.ctx, "wire_id=" + req.RequestID, "elapsed_ms=", "io_elapsed_ms="} {
							if !strings.Contains(text, want) {
								t.Fatalf("missing %s: %s", want, text)
							}
						}
						noIO := early || tc.name == "dial" || tc.name == "deadline_set_error"
						if strings.Contains(text, "io_elapsed_ms=-1") != noIO {
							t.Fatal("wrong IO timer presence")
						}
					}
					for _, secret := range []string{"secret-", "secret-relative-path", "not-json"} {
						if strings.Contains(text, secret) {
							t.Fatal("secret/raw error leak")
						}
					}
				}
			}
		})
	}
}

func TestServiceUnixDiagRealDialFailure(t *testing.T) {
	old := serviceFrameCapture
	t.Cleanup(func() { serviceFrameCapture = old })
	serviceFrameCapture = newServiceFrameCapture(true, time.Now())
	logs := serviceDiagLog(t)
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	_, errFrame, code, retry := serviceRelay(context.Background(), serviceRequest{RequestID: strings.Repeat("b", 32)})
	if code != "SERVICE_UNAVAILABLE" || !retry || errFrame != nil || !strings.Contains(logs.String(), "stage=DIAL outcome=OTHER ctx=NONE") {
		t.Fatal("real Unix dial mismatch")
	}
}

func TestServiceUnixDiagSharedCaptureBudget(t *testing.T) {
	logs := serviceDiagLog(t)
	for _, unixFirst := range []bool{false, true} {
		logs.Reset()
		c := newServiceFrameCapture(true, time.Now())
		c.lines = serviceFrameDiagLines - 1
		d := newServiceFrameDiag(c, 1, 1)
		unix := func() {
			c.unixTerminal(context.Background(), strings.Repeat("c", 32), "READSLICE", io.EOF, time.Now(), time.Time{})
		}
		if unixFirst {
			unix()
			d.emit("read_end", "EOF", "NONE", 0)
		} else {
			d.emit("read_end", "EOF", "NONE", 0)
			unix()
		}
		unix()
		d.emit("read_end", "EOF", "NONE", 0)
		if c.lines != serviceFrameDiagLines || !c.closed || strings.Count(logs.String(), "\n") != 2 || strings.Count(logs.String(), "truncated=true reason=LINE_LIMIT") != 1 {
			t.Fatal("not a shared one-marker cap")
		}
	}
	logs.Reset()
	c := newServiceFrameCapture(true, time.Now().Add(-serviceFrameDiagWindow-time.Second))
	c.unixTerminal(context.Background(), "bad-secret-id\n", "DIAL", io.EOF, time.Now(), time.Time{})
	c.unixTerminal(context.Background(), "bad-secret-id\n", "DIAL", io.EOF, time.Now(), time.Time{})
	if strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), "reason=WINDOW_END") || strings.Contains(logs.String(), "bad-secret") {
		t.Fatal("window/redaction failed")
	}
	logs.Reset()
	c = newServiceFrameCapture(true, time.Now())
	c.unixTerminal(context.Background(), "bad-secret-id\n", "DIAL", io.EOF, time.Now(), time.Time{})
	if !strings.Contains(logs.String(), "wire_id=-") || strings.Contains(logs.String(), "bad-secret") {
		t.Fatal("invalid ID leaked")
	}
}

func TestServiceUnixDiagOffNoAllocations(t *testing.T) {
	logs := serviceDiagLog(t)
	var c *serviceFrameCaptureState
	ctx := context.Background()
	started := time.Now()
	if n := testing.AllocsPerRun(100, func() { c.unixTerminal(ctx, "bad-secret-id", "READSLICE", io.EOF, started, time.Time{}) }); n != 0 {
		t.Fatalf("OFF diagnostic allocations: %v", n)
	}
	if logs.Len() != 0 {
		t.Fatal("OFF output")
	}
}
