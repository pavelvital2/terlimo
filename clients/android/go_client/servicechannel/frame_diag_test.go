package servicechannel

import (
	"bytes"
	"context"
	"errors"
	"github.com/cbeuw/connutil"
	"net"
	"strings"
	"testing"
	"wg-turn-client/wlwire"
)

func TestFrameDiagOffParity(t *testing.T) {
	var out bytes.Buffer
	c := NewFrameCapture(false, &out)
	ctx := context.Background()
	if c != nil || WithFrameCapture(ctx, c) != ctx || NewPumpDiag(ctx) != nil {
		t.Fatal("OFF state")
	}
	if n := testing.AllocsPerRun(100, func() { c.frame(1, 1, wlwire.ID{1}, 0, 10, 10, nil); c.Close() }); n != 0 {
		t.Fatalf("OFF alloc=%v", n)
	}
	if out.Len() != 0 {
		t.Fatal("OFF output")
	}
}
func TestFrameDiagWindowAndLimit(t *testing.T) {
	for _, mode := range []string{"limit", "window", "close"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			c := NewFrameCapture(true, &out)
			if mode == "window" {
				c.mu.Lock()
				c.started = c.started.Add(-frameDiagWindow)
				c.mu.Unlock()
			}
			count := 1
			if mode == "limit" {
				count = 515
			}
			for i := 0; i < count; i++ {
				c.frame(1, 1, wlwire.ID{1}, 0, 10, 10, nil)
			}
			c.Close()
			c.Close()
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) > 513 || strings.Count(out.String(), "final=true") != 1 {
				t.Fatal(out.String())
			}
			if mode != "close" && !strings.Contains(out.String(), "truncated=true") {
				t.Fatal(out.String())
			}
		})
	}
}
func TestFrameDiagActualWritesUniqueIDs(t *testing.T) {
	var out bytes.Buffer
	c := NewFrameCapture(true, &out)
	defer c.Close()
	id1, id2 := wlwire.ID{1}, wlwire.ID{2}
	replies := append(replyFrames(t, id1), replyFrames(t, id2)...)
	conn := &scriptedConn{reads: replies}
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) { return conn, func() { conn.Close() }, nil }))
	ch.FrameCapture = c
	for _, id := range []wlwire.ID{id1, id2} {
		if _, err := ch.Exchange(context.Background(), Seed{}, id, bytes.Repeat([]byte("SECRET_BODY"), 120)); err != nil {
			t.Fatal(err)
		}
	}
	ch.Close()
	for _, id := range []wlwire.ID{id1, id2} {
		if strings.Count(out.String(), "wire_id="+RequestID(id)) != 2 {
			t.Fatal(out.String())
		}
	}
	if !strings.Contains(out.String(), "xid=2") || !strings.Contains(out.String(), "offset=1024") || strings.Contains(out.String(), "SECRET_BODY") {
		t.Fatal(out.String())
	}
}
func TestFrameDiagRealPipeAndCancel(t *testing.T) {
	var out bytes.Buffer
	c := NewFrameCapture(true, &out)
	defer c.Close()
	d := NewPumpDiag(WithFrameCapture(context.Background(), c))
	a, b := connutil.AsyncPacketPipe()
	defer a.Close()
	payload := []byte("SECRET_CIPHERTEXT")
	if _, err := b.Write(payload); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, _, err := a.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatal("pipe")
	}
	d.Record("PUMP_DEQUEUE", 1, n, n, nil)
	d.Record("WRAP", 1, n, n, nil)
	d.Record("TURN_WRITE", 1, n, 0, net.ErrClosed)
	d.Close("RELAY_WRITE", net.ErrClosed)
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "wire_id=01") || !strings.Contains(out.String(), "conn=1 record=1") {
		t.Fatal(out.String())
	}
	conn := newGateConn()
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) { return conn, func() { conn.Close() }, nil }))
	ch.FrameCapture = c
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ch.Exchange(cancelCtx, Seed{}, wlwire.ID{3}, []byte("x"))
	if !errors.Is(err, context.Canceled) || !conn.closed.Load() {
		t.Fatalf("cancel %v closed=%v", err, conn.closed.Load())
	}
	ch.Close()
	before := out.Len()
	d.Record("TURN_WRITE", 2, 1, 1, nil)
	if out.Len() != before {
		t.Fatal("capture reopened")
	}
}

func TestFrameDiagCancelBlockedRead(t *testing.T) {
	var out bytes.Buffer
	c := NewFrameCapture(true, &out)
	defer c.Close()
	conn := newGateConn()
	ch := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) { return conn, func() { conn.Close() }, nil }))
	ch.FrameCapture = c
	reading := make(chan struct{})
	ch.Observe = func(stage string) {
		if stage == "FRAME_READ_BEGIN" {
			close(reading)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := ch.Exchange(ctx, Seed{}, wlwire.ID{4}, []byte("x")); done <- err }()
	<-reading
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !conn.closed.Load() || ch.conn != nil {
		t.Fatal("canceled connection retained")
	}
	ch.Close()
}
