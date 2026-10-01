package main

import (
	"bytes"
	"context"
	"github.com/cbeuw/connutil"
	"net"
	"strings"
	"testing"
	"time"
	"wg-turn-client/servicechannel"
)

type frameDiagRelay struct {
	net.PacketConn
	calls int
}

func (r *frameDiagRelay) WriteTo(p []byte, _ net.Addr) (int, error) {
	r.calls++
	return 0, net.ErrClosed
}
func TestManagedFrameDiagOutboundRealPipe(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var out bytes.Buffer
		c := servicechannel.NewFrameCapture(enabled, &out)
		ctx, cancel := context.WithCancel(context.Background())
		a, b := connutil.AsyncPacketPipe()
		relay := &frameDiagRelay{}
		d := servicechannel.NewPumpDiag(servicechannel.WithFrameCapture(ctx, c))
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer cancel()
			runManagedOutboundPump(a, relay, &net.UDPAddr{}, bytes.Repeat([]byte{1}, wrapKeyLen), nil, d)
		}()
		if _, err := b.Write([]byte("SECRET_ENCRYPTED_RECORD")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("pump did not exit")
		}
		a.Close()
		c.Close()
		if ctx.Err() == nil || relay.calls != 1 {
			t.Fatal("changed pump/cancel behavior")
		}
		if !enabled && out.Len() != 0 {
			t.Fatal("OFF output")
		}
		if enabled {
			for _, token := range []string{"PUMP_DEQUEUE", "WRAP", "TURN_WRITE", "reason=RELAY_WRITE"} {
				if !strings.Contains(out.String(), token) {
					t.Fatal(out.String())
				}
			}
		}
		if strings.Contains(out.String(), "SECRET") {
			t.Fatal("secret marker")
		}
	}
}
