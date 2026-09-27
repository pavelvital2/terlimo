package servicechannel

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

type fakeConn struct {
	local  *net.UDPAddr
	closed bool
}

func (c *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return c.local }
func (c *fakeConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestChannelTraceEstablishFailAndClass(t *testing.T) {
	var got []string
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		return nil, nil, errors.New("boom")
	}))
	ch.Trace = func(session uint64, class, event string, reused bool, port int) {
		got = append(got, class+":"+event)
	}
	ctx := WithRequestClass(context.Background(), "GATEWAYS")
	if _, err := ch.Exchange(ctx, Seed{}, wlwire.ID{}, []byte("x")); err == nil {
		t.Fatal("expected establish failure")
	}
	if len(got) != 1 || got[0] != "GATEWAYS:ESTABLISH_FAIL" {
		t.Fatalf("trace events wrong: %v", got)
	}
}

func TestChannelTraceEstablishOkPortWriteReadFail(t *testing.T) {
	type ev struct {
		session      uint64
		class, event string
		reused       bool
		port         int
	}
	var got []ev
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		return &fakeConn{local: &net.UDPAddr{Port: 54321}}, func() {}, nil
	}))
	ch.Trace = func(session uint64, class, event string, reused bool, port int) {
		got = append(got, ev{session, class, event, reused, port})
	}
	ctx := WithRequestClass(context.Background(), "ME")
	_, _ = ch.Exchange(ctx, Seed{}, wlwire.ID{}, []byte("x"))
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %v", got)
	}
	if got[0].event != "ESTABLISH_OK" || got[0].session != 1 || got[0].port != 54321 || got[0].reused {
		t.Fatalf("establish event wrong: %+v", got[0])
	}
	if got[1].event != "WRITE_OK" || got[1].class != "ME" || got[1].reused {
		t.Fatalf("write event wrong: %+v", got[1])
	}
	if got[2].event != "READ_FAIL" || got[2].reused {
		t.Fatalf("read event wrong: %+v", got[2])
	}
}

func TestRequestClassMapping(t *testing.T) {
	cases := map[string]string{
		"/api/mobile/v1/auth/challenge":                  "AUTH",
		"/api/mobile/v1/auth/session":                    "AUTH",
		"/api/mobile/v1/installations":                   "AUTH",
		"/api/mobile/v1/me":                              "ME",
		"/api/mobile/v1/gateways":                        "GATEWAYS",
		"/api/mobile/v1/access/sync":                     "ACCESS_SYNC",
		"/api/mobile/v1/registration/telegram/link":      "REG_LINK",
		"/api/mobile/v1/plans":                           "PLANS",
		"/api/mobile/v1/quotes":                          "QUOTES",
		"/api/mobile/v1/payments":                        "PAYMENTS",
		"/api/mobile/v1/payments/pay-1":                  "PAYMENTS",
		"/api/mobile/v1/payments/pay-1/checkout-session": "CHECKOUT",
		"/api/mobile/v1/unknown":                         "OTHER",
	}
	for path, want := range cases {
		if got := requestClassForPath(path); got != want {
			t.Fatalf("class(%s)=%s want %s", path, got, want)
		}
	}
	if got := RequestClass(context.Background()); got != "OTHER" {
		t.Fatalf("default class=%s", got)
	}
}
