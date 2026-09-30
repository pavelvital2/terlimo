package servicechannel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

// scriptedConn answers each Read from a prepared frame list and records closure.
type scriptedConn struct {
	local  *net.UDPAddr
	reads  [][]byte
	idx    int
	closed bool
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if c.idx >= len(c.reads) {
		return 0, io.EOF
	}
	n := copy(p, c.reads[c.idx])
	c.idx++
	return n, nil
}
func (c *scriptedConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *scriptedConn) Close() error                     { c.closed = true; return nil }
func (c *scriptedConn) LocalAddr() net.Addr              { return c.local }
func (c *scriptedConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// gateConn blocks until Close, for timeout/cancel classification.
type gateConn struct {
	local  *net.UDPAddr
	done   chan struct{}
	closed atomic.Bool
}

func newGateConn() *gateConn {
	return &gateConn{local: &net.UDPAddr{Port: 40001}, done: make(chan struct{})}
}
func (c *gateConn) Read([]byte) (int, error) {
	<-c.done
	return 0, net.ErrClosed
}
func (c *gateConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *gateConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
	}
	return nil
}
func (c *gateConn) LocalAddr() net.Addr              { return c.local }
func (c *gateConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *gateConn) SetDeadline(time.Time) error      { return nil }
func (c *gateConn) SetReadDeadline(time.Time) error  { return nil }
func (c *gateConn) SetWriteDeadline(time.Time) error { return nil }

type traceRecord struct {
	session, exchange      uint64
	class, event, errClass string
	reused                 bool
	requests               int
	idleMS, elapsedMS      int64
	port                   int
}

func recordTrace(ch *Channel) *[]traceRecord {
	got := &[]traceRecord{}
	ch.Trace = func(ev TraceEvent) {
		*got = append(*got, traceRecord{ev.Session, ev.Exchange, ev.Class, ev.Event, ev.ErrClass,
			ev.Reused, ev.Requests, ev.IdleMS, ev.ElapsedMS, ev.Port})
	}
	return got
}

func events(recs []traceRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.event)
	}
	return out
}

func eqEvents(t *testing.T, got []traceRecord, want ...string) {
	t.Helper()
	g := events(got)
	if len(g) != len(want) {
		t.Fatalf("events=%v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("events=%v, want %v", g, want)
		}
	}
}

func replyFrames(t *testing.T, id wlwire.ID) [][]byte {
	t.Helper()
	frames, err := wlwire.ServiceFrames(id, true, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestChannelTraceSuccessfulExchange(t *testing.T) {
	id := wlwire.ID{1, 2, 3}
	conn := &scriptedConn{local: &net.UDPAddr{Port: 54321}, reads: replyFrames(t, id)}
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		return conn, func() {}, nil
	}))
	got := recordTrace(ch)
	ctx := WithRequestClass(context.Background(), "GATEWAYS")
	body, err := ch.Exchange(ctx, Seed{}, id, []byte("x"))
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("exchange body=%q err=%v", body, err)
	}
	eqEvents(t, *got, "ESTABLISH_OK", "WRITE_BEGIN", "WRITE_OK", "READ_BEGIN", "READ_OK", "EXCHANGE_END")
	end := (*got)[len(*got)-1]
	if end.exchange == 0 || end.session != 1 || end.class != "GATEWAYS" || end.errClass != "NONE" ||
		end.elapsedMS < 0 || end.port != 54321 || end.reused {
		t.Fatalf("end record wrong: %+v", end)
	}
	for _, r := range *got {
		if r.exchange != 1 {
			t.Fatalf("local exchange id must be monotonic per call: %+v", r)
		}
	}
}

func TestChannelTracePeerCloseEOFAndNewGeneration(t *testing.T) {
	id := wlwire.ID{4}
	first := &scriptedConn{local: &net.UDPAddr{Port: 41000}} // no reads -> EOF
	dials := 0
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		return &scriptedConn{local: &net.UDPAddr{Port: 41001}, reads: replyFrames(t, wlwire.ID{5})},
			func() {}, nil
	}))
	got := recordTrace(ch)
	_, err := ch.Exchange(WithRequestClass(context.Background(), "ME"), Seed{}, id, []byte("x"))
	if !errors.Is(err, ErrTransportFailed) {
		t.Fatalf("err=%v, want the fixed TRANSPORT_FAILED code", err)
	}
	readFail := (*got)[len(*got)-2]
	if readFail.event != "READ_FAIL" || readFail.errClass != "EOF" {
		t.Fatalf("read fail record wrong: %+v", readFail)
	}
	if end := (*got)[len(*got)-1]; end.errClass != "EOF" {
		t.Fatalf("terminal class wrong: %+v", end)
	}
	if !first.closed {
		t.Fatal("failed session must be closed")
	}

	// Next exchange must establish a NEW generation and never reuse the closed session.
	before := len(*got)
	if _, err := ch.Exchange(WithRequestClass(context.Background(), "ME"), Seed{}, wlwire.ID{5}, []byte("y")); err != nil {
		t.Fatalf("second exchange: %v", err)
	}
	next := (*got)[before:]
	if next[0].event != "ESTABLISH_OK" || next[0].session != 2 || next[0].reused {
		t.Fatalf("second session must be a fresh generation: %+v", next[0])
	}
}

func TestChannelTraceReadTimeoutClass(t *testing.T) {
	conn := newGateConn()
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		return conn, func() {}, nil
	}))
	ch.Timeout = 150 * time.Millisecond
	got := recordTrace(ch)
	if _, err := ch.Exchange(WithRequestClass(context.Background(), "AUTH"), Seed{}, wlwire.ID{6}, []byte("x")); err == nil {
		t.Fatal("expected read timeout")
	}
	var readFail traceRecord
	for _, r := range *got {
		if r.event == "READ_FAIL" {
			readFail = r
		}
	}
	if readFail.errClass != "TIMEOUT" {
		t.Fatalf("read fail class=%q want TIMEOUT (records %+v)", readFail.errClass, *got)
	}
	end := (*got)[len(*got)-1]
	if end.event != "EXCHANGE_END" || end.errClass != "TIMEOUT" {
		t.Fatalf("terminal class=%q want TIMEOUT", end.errClass)
	}
}

func TestChannelTraceCancelClass(t *testing.T) {
	conn := newGateConn()
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		return conn, func() {}, nil
	}))
	got := recordTrace(ch)
	ctx, cancel := context.WithCancel(WithRequestClass(context.Background(), "ME"))
	done := make(chan error, 1)
	go func() {
		_, err := ch.Exchange(ctx, Seed{}, wlwire.ID{7}, []byte("x"))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	var readFail traceRecord
	for _, r := range *got {
		if r.event == "READ_FAIL" {
			readFail = r
		}
	}
	if readFail.errClass != "CANCELED" {
		t.Fatalf("read fail class=%q want CANCELED (records %+v)", readFail.errClass, *got)
	}
	if end := (*got)[len(*got)-1]; end.errClass != "CANCELED" {
		t.Fatalf("terminal class=%q want CANCELED", end.errClass)
	}
}

func TestChannelTraceReuseStaleSeedNewGeneration(t *testing.T) {
	id := wlwire.ID{8}
	first := &scriptedConn{local: &net.UDPAddr{Port: 42000}, reads: replyFrames(t, id)}
	dials := 0
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		return &scriptedConn{local: &net.UDPAddr{Port: 42001}, reads: replyFrames(t, wlwire.ID{9})}, func() {}, nil
	}))
	got := recordTrace(ch)
	if _, err := ch.Exchange(WithRequestClass(context.Background(), "ME"), Seed{}, id, []byte("x")); err != nil {
		t.Fatal(err)
	}
	before := len(*got)
	seed := Seed{PeerIP: "192.0.2.10", DTLSPort: 1}
	if _, err := ch.Exchange(WithRequestClass(context.Background(), "ME"), seed, wlwire.ID{9}, []byte("y")); err != nil {
		t.Fatal(err)
	}
	next := (*got)[before:]
	if next[0].event != "REUSE_STALE_SEED" || !next[0].reused || !first.closed {
		t.Fatalf("stale seed must close the old session: %+v closed=%v", next[0], first.closed)
	}
	if next[1].event != "ESTABLISH_OK" || next[1].session != 2 || next[1].reused {
		t.Fatalf("next session must be fresh: %+v", next[1])
	}
}

func TestChannelTraceReuseStaleIdleAndRequests(t *testing.T) {
	id := wlwire.ID{10}
	for name, mutate := range map[string]func(*Channel){
		"REUSE_STALE_IDLE": func(c *Channel) { c.lastReply = time.Now().Add(-maxSessionIdle - time.Second) },
		"REUSE_STALE_REQ":  func(c *Channel) { c.requests = maxSessionRequests },
	} {
		first := &scriptedConn{local: &net.UDPAddr{Port: 43000}, reads: replyFrames(t, id)}
		dials := 0
		ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
			dials++
			if dials == 1 {
				return first, func() {}, nil
			}
			return &scriptedConn{local: &net.UDPAddr{Port: 43001}, reads: replyFrames(t, wlwire.ID{11})}, func() {}, nil
		}))
		got := recordTrace(ch)
		if _, err := ch.Exchange(WithRequestClass(context.Background(), "ME"), Seed{}, id, []byte("x")); err != nil {
			t.Fatal(err)
		}
		mutate(ch)
		before := len(*got)
		if _, err := ch.Exchange(WithRequestClass(context.Background(), "ME"), Seed{}, wlwire.ID{11}, []byte("y")); err != nil {
			t.Fatal(err)
		}
		if next := (*got)[before]; next.event != name || !next.reused || !first.closed {
			t.Fatalf("%s: stale session must be closed: %+v closed=%v", name, next, first.closed)
		}
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
