package main

// This file holds the regression test for the cancel->restart attribution race
// on the live marker channel. It deliberately uses only symbols that already
// exist on the parent commit so it can be copied into a temporary worktree at
// the parent commit and shown to FAIL there.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestConnectedEchoOutstandingPongAfterCancelCannotSettleNext reproduces the
// cancel->restart race: the first live probe is cancelled while its pong is
// still in flight, so that pong is an old answer to an already-abandoned ping.
// A fresh RequestRTT on the same live session must never be settled by it.
//
// Parent behavior (964aad7): cancel rebased pongSeq onto pingSeq, the fresh
// probe armed expectSeq=2, the old pong was counted as pongSeq=2 and falsely
// settled the fresh probe with a non-zero RTT and no error. Fixed behavior:
// the cancelled ping stays outstanding, the channel is dirty, RequestRTT fails
// closed with RTT_ECHO_CHANNEL_DIRTY, writes no marker, and only the old pong's
// arrival reconciles the counters.
func TestConnectedEchoOutstandingPongAfterCancelCannotSettleNext(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)

	firstCtx, firstCancel := context.WithCancel(context.Background())
	first := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(firstCtx)
		first <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	firstCancel()
	cancelled := <-first
	if !errors.Is(cancelled.err, context.Canceled) || cancelled.rtt != 0 {
		t.Fatalf("cancelled probe rtt=%v err=%v", cancelled.rtt, cancelled.err)
	}

	// The old pong is still in flight. A fresh probe must either fail closed
	// before writing anything, or it must never be settled by that old pong.
	second := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(context.Background())
		second <- echoProbeResult{rtt: rtt, err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for conn.writeCount() < 2 && time.Now().Before(deadline) {
		select {
		case got := <-second:
			// Fail-closed path: no second marker was written and the request
			// was refused with the dirty-channel sentinel.
			if got.err == nil || got.rtt != 0 || got.err.Error() != "RTT_ECHO_CHANNEL_DIRTY" {
				t.Fatalf("dirty channel result rtt=%v err=%v", got.rtt, got.err)
			}
			if conn.writeCount() != 1 {
				t.Fatalf("dirty channel accepted a marker write: %d", conn.writeCount())
			}
			// The old pong finally arrives and reconciles the outstanding ping
			// without ever settling a fresh request.
			handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
			if ping, pong := echo.seq.snapshot(); ping != 1 || pong != 1 {
				t.Fatalf("old pong did not reconcile: ping=%d pong=%d", ping, pong)
			}
			return
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if conn.writeCount() < 2 {
		t.Fatal("fresh probe neither failed closed nor wrote")
	}
	// Parent path: the fresh probe armed its own waiter (marker 2 written); the
	// old in-flight pong must not settle it. On the parent code this delivery
	// settles the fresh probe on the rebased sequence.
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	select {
	case got := <-second:
		t.Fatalf("old outstanding pong settled the fresh probe: rtt=%v err=%v", got.rtt, got.err)
	case <-time.After(300 * time.Millisecond):
		t.Fatal("fresh probe stayed pending after an old pong")
	}
}
