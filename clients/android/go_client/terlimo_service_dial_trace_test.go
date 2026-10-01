package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type dialTestPacket struct {
	mu      sync.Mutex
	calls   int
	closed  int
	data    []byte
	addr    net.Addr
	n       int
	err     error
	entered chan struct{}
	release chan struct{}
}

func (p *dialTestPacket) WriteTo(data []byte, addr net.Addr) (int, error) {
	p.mu.Lock()
	p.calls++
	p.data = append([]byte(nil), data...)
	p.addr = addr
	p.mu.Unlock()
	if p.entered != nil {
		p.entered <- struct{}{}
		<-p.release
	}
	return p.n, p.err
}
func (p *dialTestPacket) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, io.EOF }
func (p *dialTestPacket) Close() error                           { p.closed++; return p.err }
func (p *dialTestPacket) LocalAddr() net.Addr                    { return &net.UDPAddr{} }
func (p *dialTestPacket) SetDeadline(time.Time) error            { return p.err }
func (p *dialTestPacket) SetReadDeadline(time.Time) error        { return p.err }
func (p *dialTestPacket) SetWriteDeadline(time.Time) error       { return p.err }

type dialTestWriter struct {
	bytes.Buffer
	calls int
}

func (w *dialTestWriter) Write(p []byte) (int, error) { w.calls++; return w.Buffer.Write(p) }

// Override WriteString so io.WriteString also observes the single output call.
func (w *dialTestWriter) WriteString(s string) (int, error) {
	w.calls++
	return w.Buffer.WriteString(s)
}

func TestServiceDialScopeAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if mobileServiceDialTrace(ctx) != nil {
		t.Fatal("unscoped dial observed")
	}
	trace := newServiceDialTrace()
	child := context.WithValue(ctx, serviceDialKey{}, trace)
	if mobileServiceDialTrace(child) != trace || mobileServiceDialTrace(ctx) != nil || child.Done() != ctx.Done() {
		t.Fatal("scope/ownership changed")
	}
	cancel()
	if child.Err() != context.Canceled {
		t.Fatal("cancellation changed")
	}
	raw := &dialTestPacket{}
	if observeFirstServiceDialWrite(raw, nil, 1, turnTransportUDP) != raw {
		t.Fatal("ordinary caller wrapped")
	}
}
func TestServiceDialBoundsAndSafeOutput(t *testing.T) {
	a, b := newServiceDialTrace(), newServiceDialTrace()
	if a.call == b.call {
		t.Fatal("call reused")
	}
	for i := 1; i <= serviceDialEventCap+5; i++ {
		a.note(dialSocketEnd, i, turnTransportUDP, errors.New("private-host/token/password"), false)
	}
	var w dialTestWriter
	a.finish(errors.New("private-host/token/password"), &w)
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != serviceDialEventCap+1 || !strings.Contains(lines[len(lines)-1], "truncated=1") || w.calls != 1 {
		t.Fatalf("bounds/write: %d/%d", len(lines), w.calls)
	}
	if strings.Contains(w.String(), "private-") {
		t.Fatal("raw error exposed")
	}
	a.note(dialSocketEnd, 999, turnTransportTLS, nil, false)
	a.finish(nil, &w)
	if w.calls != 1 {
		t.Fatal("duplicate/late emission")
	}
	for _, line := range lines {
		if len(line) > 256 {
			t.Fatal("host line bound")
		}
	}
}
func TestServiceDialFirstWritePassThrough(t *testing.T) {
	trace := newServiceDialTrace()
	failure := errors.New("private error")
	raw := &dialTestPacket{n: 2, err: failure}
	addr := &net.UDPAddr{Port: 123}
	payload := []byte("opaque bytes")
	conn := observeFirstServiceDialWrite(raw, trace, 2, turnTransportTLS)
	for range 3 {
		n, err := conn.WriteTo(payload, addr)
		if n != 2 || err != failure {
			t.Fatal("return changed")
		}
	}
	if raw.calls != 3 || !bytes.Equal(raw.data, payload) || raw.addr != addr {
		t.Fatal("delegation changed")
	}
	if conn.SetDeadline(time.Now()) != failure || conn.Close() != failure || raw.closed != 1 {
		t.Fatal("packetconn methods changed")
	}
	var out bytes.Buffer
	trace.finish(nil, &out)
	if strings.Count(out.String(), "stage=FIRST_WRITE_BEGIN") != 1 || strings.Count(out.String(), "stage=FIRST_WRITE_END") != 1 {
		t.Fatal(out.String())
	}
}
func TestServiceDialConcurrentWriteAndLateCompletion(t *testing.T) {
	trace := newServiceDialTrace()
	raw := &dialTestPacket{n: 1, entered: make(chan struct{}, 8), release: make(chan struct{})}
	conn := observeFirstServiceDialWrite(raw, trace, 1, turnTransportUDP)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = conn.WriteTo([]byte{1}, &net.UDPAddr{}) }()
	}
	for range 8 {
		<-raw.entered
	}
	var out bytes.Buffer
	trace.finish(context.Canceled, &out)
	before := out.String()
	close(raw.release)
	wg.Wait()
	if strings.Count(before, "stage=FIRST_WRITE_BEGIN") != 1 || strings.Contains(before, "stage=FIRST_WRITE_END") {
		t.Fatal("incomplete span fabricated")
	}
	if out.String() != before {
		t.Fatal("late event changed output")
	}
}
func TestServiceDialFinishBoundaryExcludesRacingFutureEnd(t *testing.T) {
	trace := newServiceDialTrace()
	trace.note(dialFirstWriteBegin, 1, turnTransportUDP, nil, true)
	// Model completion recorded after the captured finish time but before seal lock.
	trace.events[trace.count] = serviceDialEvent{elapsed: time.Hour, candidate: 1, transport: turnTransportUDP, stage: dialFirstWriteEnd, result: "OK"}
	trace.count++
	var out bytes.Buffer
	trace.finish(context.Canceled, &out)
	if strings.Contains(out.String(), "FIRST_WRITE_END") {
		t.Fatal("phantom late end")
	}
}
func TestServiceDialErrorClasses(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{{nil, "OK"}, {context.Canceled, "CANCELED"}, {context.DeadlineExceeded, "TIMEOUT"}, {io.EOF, "EOF"}, {net.ErrClosed, "CLOSED"}, {errors.New("private error"), "OTHER"}}
	for _, c := range cases {
		if got := serviceDialResult(c.err); got != c.want {
			t.Fatalf("class %s", got)
		}
	}
}
func TestServiceDialAllocateDefaultDelegation(t *testing.T) {
	ep := turnEndpoint{Host: "127.0.0.1", Port: "-1", Transport: turnTransportUDP}
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	creds := &Credentials{}
	a, b := &dialTestPacket{}, &dialTestPacket{}
	trace := newServiceDialTrace()
	ca, ra, ea := allocateTURNOnConn(ep, peer, creds, a)
	cb, rb, eb := allocateTURNOnConnObserved(ep, peer, creds, b, trace, 3)
	if ca != nil || cb != nil || ra != nil || rb != nil || ea == nil || eb == nil || ea.Error() != eb.Error() || a.closed != 1 || b.closed != 1 || a.calls != 0 || b.calls != 0 {
		t.Fatal("constructor failure/delegation changed")
	}
	var out bytes.Buffer
	trace.finish(eb, &out)
	if !strings.Contains(out.String(), "stage=CLIENT_BEGIN") || !strings.Contains(out.String(), "stage=CLIENT_END result=OTHER") || strings.Contains(out.String(), "ALLOCATE_BEGIN") {
		t.Fatal("early return boundaries")
	}
}
