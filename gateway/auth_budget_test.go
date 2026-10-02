package main

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// Logical upstream completion instant: no 16-second sleeps or live sockets.
// Read observes the actual socket deadline chosen by production, not a duplicated profile.
type lateBudgetConn struct {
	unixDiagConn
	completeAfter time.Duration
}

func (c *lateBudgetConn) Read(b []byte) (int, error) {
	if c.completeAfter > c.deadlines[0].Sub(c.deadlineAt) {
		return 0, os.ErrDeadlineExceeded
	}
	return c.unixDiagConn.Read(b)
}
func TestAuthBudgetLateFiniteAndNonAuth(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", "/fixture.sock")
	for _, tc := range []struct {
		name, path   string
		delay        time.Duration
		old, success bool
	}{
		{"old_late", "/api/mobile/v1/auth/challenge", 16 * time.Second, true, false},
		{"challenge_late", "/api/mobile/v1/auth/challenge", 16 * time.Second, false, true},
		{"session_late", "/api/mobile/v1/auth/session", 16 * time.Second, false, true},
		{"auth_over", "/api/mobile/v1/auth/session", 23 * time.Second, false, false},
		{"me_unchanged", "/api/mobile/v1/me", 16 * time.Second, false, false},
		{"gateways_unchanged", "/api/mobile/v1/gateways", 16 * time.Second, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := serviceRequest{Method: "POST", Path: tc.path, RequestID: strings.Repeat("a", 32)}
			if !strings.Contains(tc.path, "/auth/") {
				req.Method = "GET"
			}
			c := &lateBudgetConn{unixDiagConn: unixDiagConn{closed: make(chan struct{}), payload: []byte(`{"v":1,"request_id":"` + req.RequestID + `","status":200,"headers":{},"body_b64":""}` + "\n")}, completeAfter: tc.delay}
			dial := func(context.Context, string, string) (net.Conn, error) { return c, nil }
			call := serviceRelayWithDial
			if tc.old {
				call = ordinaryRelayWithDialForTest
			}
			resp, _, code, _ := call(context.Background(), req, dial)
			if (code == "" && resp.Status == 200) != tc.success {
				t.Fatalf("success=%t status=%d code=%s", tc.success, resp.Status, code)
			}
			if len(c.deadlines) != 1 {
				t.Fatal("deadline reset")
			}
		})
	}
}
func TestAuthBudgetCallerRemainingAndCancel(t *testing.T) {
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", "/fixture.sock")
	req := serviceRequest{Method: "POST", Path: "/api/mobile/v1/auth/challenge", RequestID: strings.Repeat("a", 32)}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	c := &lateBudgetConn{unixDiagConn: unixDiagConn{closed: make(chan struct{})}, completeAfter: 16 * time.Second}
	_, _, code, _ := serviceRelayWithDial(ctx, req, func(context.Context, string, string) (net.Conn, error) { return c, nil })
	if code != "SERVICE_UNAVAILABLE" || c.deadlines[0].Sub(c.deadlineAt) > 12*time.Second {
		t.Fatal("caller budget enlarged")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	blocked := &unixDiagConn{closed: make(chan struct{}), cancel: cancel2}
	begin := time.Now()
	_, _, code, _ = serviceRelayWithDial(ctx2, req, func(context.Context, string, string) (net.Conn, error) { return blocked, nil })
	if code != "SERVICE_UNAVAILABLE" || time.Since(begin) > time.Second {
		t.Fatal("cancel cleanup not bounded")
	}
	select {
	case <-blocked.closed:
	default:
		t.Fatal("socket not closed")
	}
}
