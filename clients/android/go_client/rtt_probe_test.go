package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

// ─── synthetic connection ────────────────────────────────────────────────────

type echoRecord struct {
	data  []byte
	delay time.Duration
	err   error
}

type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

type echoTestConn struct {
	mu       sync.Mutex
	queue    []echoRecord
	reads    int
	writes   [][]byte
	deadline time.Time
	wake     chan struct{}
}

func newEchoTestConn(records ...echoRecord) *echoTestConn {
	return &echoTestConn{queue: records, wake: make(chan struct{}, 1)}
}

func (c *echoTestConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.queue) == 0 {
		deadline := c.deadline
		c.mu.Unlock()
		return c.waitDeadline(deadline)
	}
	record := c.queue[0]
	c.queue = c.queue[1:]
	c.mu.Unlock()
	if record.delay > 0 {
		time.Sleep(record.delay)
	}
	if record.err != nil {
		return 0, record.err
	}
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return copy(p, record.data), nil
}

func (c *echoTestConn) waitDeadline(deadline time.Time) (int, error) {
	for {
		if deadline.IsZero() {
			<-c.wake
			return 0, fakeTimeoutError{}
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return 0, fakeTimeoutError{}
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			return 0, fakeTimeoutError{}
		case <-c.wake:
			timer.Stop()
			c.mu.Lock()
			deadline = c.deadline
			c.mu.Unlock()
		}
	}
}

func (c *echoTestConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}

func (c *echoTestConn) Close() error                       { return nil }
func (c *echoTestConn) LocalAddr() net.Addr                { return nil }
func (c *echoTestConn) RemoteAddr() net.Addr               { return nil }
func (c *echoTestConn) SetDeadline(t time.Time) error      { return c.setDeadline(t) }
func (c *echoTestConn) SetReadDeadline(t time.Time) error  { return c.setDeadline(t) }
func (c *echoTestConn) SetWriteDeadline(t time.Time) error { return c.setDeadline(t) }

func (c *echoTestConn) setDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *echoTestConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

func (c *echoTestConn) deadlineValue() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func (c *echoTestConn) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// ─── measureEchoRTT ──────────────────────────────────────────────────────────

func TestMeasureEchoRTTReturnsWriteToPongDelayOnSameConn(t *testing.T) {
	conn := newEchoTestConn(echoRecord{data: []byte{keepaliveByte}, delay: 12 * time.Millisecond})
	rtt, err := measureEchoRTT(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if rtt < 12*time.Millisecond || rtt > time.Second {
		t.Fatalf("unexpected echo RTT %v", rtt)
	}
	if conn.writeCount() != 1 || len(conn.writes[0]) != 1 || conn.writes[0][0] != keepaliveByte {
		t.Fatalf("probe wrote %v", conn.writes)
	}
}

func TestMeasureEchoRTTIgnoresBoundedNonEchoRecords(t *testing.T) {
	conn := newEchoTestConn(
		echoRecord{data: []byte("noise")},
		echoRecord{data: []byte{0x01, 0x02}},
		echoRecord{data: []byte{keepaliveByte}},
	)
	rtt, err := measureEchoRTT(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if rtt <= 0 || conn.readCount() != 3 {
		t.Fatalf("rtt=%v reads=%d", rtt, conn.readCount())
	}
}

func TestMeasureEchoRTTDeadlineWithoutEchoFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rtt, err := measureEchoRTT(ctx, newEchoTestConn())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error %v", err)
	}
	if rtt != 0 {
		t.Fatalf("duration on timeout %v", rtt)
	}
}

func TestMeasureEchoRTTCancelFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := newEchoTestConn()
	result := make(chan struct {
		rtt time.Duration
		err error
	}, 1)
	go func() {
		rtt, err := measureEchoRTT(ctx, conn)
		result <- struct {
			rtt time.Duration
			err error
		}{rtt, err}
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case got := <-result:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancel error %v", got.err)
		}
		if got.rtt != 0 {
			t.Fatalf("duration on cancel %v", got.rtt)
		}
	case <-time.After(time.Second):
		t.Fatal("measureEchoRTT ignored cancellation")
	}
}

func TestMeasureEchoRTTBoundedRecordsFail(t *testing.T) {
	records := make([]echoRecord, echoProbeRecords+1)
	for i := range records {
		records[i] = echoRecord{data: []byte("x")}
	}
	conn := newEchoTestConn(records...)
	rtt, err := measureEchoRTT(context.Background(), conn)
	if !errors.Is(err, errEchoUnconfirmed) {
		t.Fatalf("unbounded junk accepted: %v", err)
	}
	if rtt != 0 {
		t.Fatalf("duration on junk %v", rtt)
	}
	if conn.readCount() != echoProbeRecords {
		t.Fatalf("read %d records, want %d", conn.readCount(), echoProbeRecords)
	}
}

func TestMeasureEchoRTTNilConnFails(t *testing.T) {
	if rtt, err := measureEchoRTT(context.Background(), nil); err == nil || rtt != 0 {
		t.Fatal("nil connection measured")
	}
}

// ─── runNodeProbe seam ───────────────────────────────────────────────────────

func probeTestController(t *testing.T) (*managedController, *probeBridgeRecorder) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	recorder := &probeBridgeRecorder{}
	bridge := newManagedBridge(recorder, "attempt", func() {})
	recorder.bridge = bridge
	c := &managedController{bridge: bridge, link: &wlbs.Link{SubscriptionRef: "sub"}}
	if err := restoreManagedCatalog(&c.store, runnerCatalog(now), "sub", now); err != nil {
		t.Fatal(err)
	}
	return c, recorder
}

type probeBridgeRecorder struct {
	mu     sync.Mutex
	frames []bridgeMessage
	bridge *managedBridge
}

func (r *probeBridgeRecorder) Write(raw []byte) (int, error) {
	var m bridgeMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, errors.New("BRIDGE_MESSAGE_INVALID")
	}
	r.mu.Lock()
	r.frames = append(r.frames, m)
	r.mu.Unlock()
	if m.string("type") == "persist" {
		r.bridge.mu.Lock()
		ch := r.bridge.waiters[m.string("request_id")]
		r.bridge.mu.Unlock()
		if ch != nil {
			select {
			case ch <- bridgeMessage{"type": "persist_result"}:
			default:
			}
		}
	}
	return len(raw), nil
}

func (r *probeBridgeRecorder) probeResults() []bridgeMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []bridgeMessage
	for _, m := range r.frames {
		if m.string("type") == "node_probe_result" {
			out = append(out, m)
		}
	}
	return out
}

func swapProbeSeams(t *testing.T, connect managedProbeConnectFunc, echo func(context.Context, net.Conn) (time.Duration, error)) {
	t.Helper()
	previousConnect, previousEcho := managedProbeConnect, managedProbeEcho
	managedProbeConnect, managedProbeEcho = connect, echo
	t.Cleanup(func() { managedProbeConnect, managedProbeEcho = previousConnect, previousEcho })
}

func TestManagedRunNodeProbeMeasuresRTTExcludingSetup(t *testing.T) {
	c, _ := probeTestController(t)
	conn := newEchoTestConn()
	swapProbeSeams(t,
		func(context.Context, wlbs.Node) (net.Conn, func(), error) {
			time.Sleep(15 * time.Millisecond)
			return conn, func() {}, nil
		},
		func(_ context.Context, got net.Conn) (time.Duration, error) {
			if got != conn {
				return 0, errors.New("UNEXPECTED_CONN")
			}
			return 7 * time.Millisecond, nil
		})
	measurement, err := c.runNodeProbe(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if measurement.RTT != 7*time.Millisecond {
		t.Fatalf("RTT polluted by setup: %v", measurement.RTT)
	}
	if measurement.Setup < 15*time.Millisecond {
		t.Fatalf("setup not measured: %v", measurement.Setup)
	}
	message := managedProbeResult("test", measurement, nil)
	if got := message["rtt_ms"]; got != int64(7) {
		t.Fatalf("rtt_ms %#v", got)
	}
	if got, ok := message["transport_setup_ms"].(int64); !ok || got < 15 {
		t.Fatalf("transport_setup_ms %#v", message["transport_setup_ms"])
	}
}

func TestManagedProbeFailurePathsCarryNoRTT(t *testing.T) {
	c, _ := probeTestController(t)
	swapProbeSeams(t,
		func(context.Context, wlbs.Node) (net.Conn, func(), error) {
			return nil, nil, errors.New("TRANSPORT_FAILED")
		},
		func(context.Context, net.Conn) (time.Duration, error) {
			return 0, errors.New("UNEXPECTED_ECHO")
		})
	measurement, err := c.runNodeProbe(context.Background(), "test")
	if err == nil || measurement.RTT != 0 {
		t.Fatal("failed transport measured")
	}
	message := managedProbeResult("test", measurement, err)
	if message["status"] != "failed" {
		t.Fatalf("status %v", message["status"])
	}
	if _, ok := message["rtt_ms"]; ok {
		t.Fatal("failure exposed rtt_ms")
	}
	if _, ok := message["transport_setup_ms"]; ok {
		t.Fatal("failure exposed transport_setup_ms")
	}

	conn := newEchoTestConn()
	swapProbeSeams(t,
		func(context.Context, wlbs.Node) (net.Conn, func(), error) {
			time.Sleep(10 * time.Millisecond)
			return conn, func() {}, nil
		},
		func(context.Context, net.Conn) (time.Duration, error) {
			return 0, context.DeadlineExceeded
		})
	measurement, err = c.runNodeProbe(context.Background(), "test")
	if !errors.Is(err, context.DeadlineExceeded) || measurement.RTT != 0 {
		t.Fatal("timed out echo measured", err)
	}
	if measurement.Setup < 10*time.Millisecond {
		t.Fatalf("setup diagnostics lost: %v", measurement.Setup)
	}
	message = managedProbeResult("test", measurement, err)
	if message["status"] != "timeout" {
		t.Fatalf("status %v", message["status"])
	}
	if _, ok := message["rtt_ms"]; ok {
		t.Fatal("timeout exposed rtt_ms")
	}
}

// ─── probeEpoch fence ────────────────────────────────────────────────────────

func TestManagedProbeEpochFenceDropsStale(t *testing.T) {
	c := &managedController{}
	stale := c.nextProbeEpoch()
	c.nextProbeEpoch()
	if c.probeResultIsFresh(stale) {
		t.Fatal("stale epoch accepted")
	}
	fresh := c.nextProbeEpoch()
	if !c.probeResultIsFresh(fresh) {
		t.Fatal("fresh epoch rejected")
	}
}

type waitNodeResult struct {
	id  string
	err error
}

func TestManagedProbeEpochDropsStaleAfterProbeStop(t *testing.T) {
	c, recorder := probeTestController(t)
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	firstFinished := make(chan struct{})
	swapProbeSeams(t,
		func(ctx context.Context, _ wlbs.Node) (net.Conn, func(), error) {
			if calls.Add(1) == 1 {
				close(firstStarted)
				<-ctx.Done()
				close(firstFinished)
				return nil, nil, ctx.Err()
			}
			return newEchoTestConn(), func() {}, nil
		},
		func(context.Context, net.Conn) (time.Duration, error) {
			return 7 * time.Millisecond, nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan waitNodeResult, 1)
	go func() {
		id, err := c.waitForNode(ctx)
		result <- waitNodeResult{id, err}
	}()
	bridge := c.bridge
	bridge.probe <- managedProbeRequest{NodeID: "test"}
	<-firstStarted
	bridge.probeStop <- struct{}{}
	<-firstFinished
	time.Sleep(50 * time.Millisecond)
	if got := recorder.probeResults(); len(got) != 0 {
		t.Fatalf("stale probe result delivered: %v", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// At most one probe may be outstanding: the buffered channel keeps an
		// unanswered request, and the outcome (busy or result) is awaited
		// before the next attempt.
		select {
		case bridge.probe <- managedProbeRequest{NodeID: "test"}:
		default:
		}
		fresh := recorder.probeResults()
		var ok []bridgeMessage
		for _, m := range fresh {
			if m["status"] == "ok" {
				ok = append(ok, m)
			}
		}
		if len(ok) > 1 {
			t.Fatalf("multiple probe results: %v", fresh)
		}
		if len(ok) == 1 {
			if ok[0]["rtt_ms"] != float64(7) {
				t.Fatalf("fresh rtt_ms %#v", ok[0]["rtt_ms"])
			}
			// Selection, not ctx cancel, terminates the loop: the selection
			// branch joins the probe goroutine before returning, so no probe
			// may outlive this test.
			bridge.selection <- "test"
			select {
			case r := <-result:
				if r.err != nil || r.id != "test" {
					t.Fatalf("waitForNode %q %v", r.id, r.err)
				}
			case <-time.After(time.Second):
				t.Fatal("waitForNode did not stop")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fresh probe result missing: %v", fresh)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagedProbeEpochSelectionDropsStaleResult(t *testing.T) {
	c, recorder := probeTestController(t)
	firstStarted := make(chan struct{})
	swapProbeSeams(t,
		func(ctx context.Context, _ wlbs.Node) (net.Conn, func(), error) {
			close(firstStarted)
			<-ctx.Done()
			return nil, nil, ctx.Err()
		},
		func(context.Context, net.Conn) (time.Duration, error) {
			return 0, errors.New("UNEXPECTED_ECHO")
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan waitNodeResult, 1)
	go func() {
		id, err := c.waitForNode(ctx)
		result <- waitNodeResult{id, err}
	}()
	bridge := c.bridge
	bridge.probe <- managedProbeRequest{NodeID: "test"}
	<-firstStarted
	before := c.probeEpoch.Load()
	bridge.selection <- "test"
	select {
	case r := <-result:
		if r.err != nil || r.id != "test" {
			t.Fatalf("selection result %q %v", r.id, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selection did not complete")
	}
	if c.probeEpoch.Load() <= before {
		t.Fatal("selection did not bump probeEpoch")
	}
	if got := recorder.probeResults(); len(got) != 0 {
		t.Fatalf("stale probe result survived selection: %v", got)
	}
}

// ─── connected echo probe (S5 07.2) ─────────────────────────────────────────

func waitForConnectedWrite(t *testing.T, conn *echoTestConn, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for conn.writeCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("connected marker write %d never happened", want)
		}
		time.Sleep(time.Millisecond)
	}
}

// respondToConnectedProbe simulates the live session read loop: it waits for
// the next 0xFF marker write and feeds the matching pong back after delay.
func respondToConnectedProbe(echo *connectedEcho, conn *echoTestConn, delay time.Duration) {
	baseline := conn.writeCount()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for conn.writeCount() <= baseline {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	}()
}

func TestConnectedEchoMeasuresWriteToPongDelay(t *testing.T) {
	conn := newEchoTestConn()
	echo := newConnectedEcho(conn, &sync.Mutex{})
	respondToConnectedProbe(echo, conn, 25*time.Millisecond)
	rtt, err := echo.RequestRTT(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rtt < 25*time.Millisecond || rtt > time.Second {
		t.Fatalf("unexpected connected echo rtt %v", rtt)
	}
	if conn.writeCount() != 1 || len(conn.writes[0]) != 1 || conn.writes[0][0] != keepaliveByte {
		t.Fatalf("connected probe wrote %v", conn.writes)
	}
	message := connectedProbeResult("gw-0", rtt, nil)
	if message["status"] != "ok" {
		t.Fatalf("connected status %v", message["status"])
	}
	if rttMs, ok := message["rtt_ms"].(int64); !ok || rttMs < 25 {
		t.Fatalf("connected rtt_ms %#v", message["rtt_ms"])
	}
	if _, ok := message["transport_setup_ms"]; ok {
		t.Fatal("connected result exposed transport_setup_ms")
	}
}

func TestRTTEchoSequencerIgnoresStaleAndUnsolicitedPongs(t *testing.T) {
	// Stale pong: the answer to an earlier ping arrives after the probe was
	// claimed, armed and written. It must not settle the probe; its own answer
	// still does.
	stale := &rttEchoSequencer{}
	if _, ok := stale.claimMarkerWrite(); !ok {
		t.Fatal("marker write suppressed without a waiter")
	}
	seq, ok := stale.claimMarkerWrite()
	if !ok {
		t.Fatal("probe claim suppressed without a waiter")
	}
	w, err := stale.armAt(seq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stale.notePong()
	if !w.pending() {
		t.Fatal("stale pong settled the probe")
	}
	stale.notePong()
	if w.pending() {
		t.Fatal("matching pong did not settle the probe")
	}
	if result := <-w.result; result.err != nil {
		t.Fatalf("matching pong error %v", result.err)
	}

	// Unsolicited/extra pong: it is ignored for RTT and must not shift the
	// sequence, so a later probe's real answer is still matched.
	extra := &rttEchoSequencer{}
	if _, ok := extra.claimMarkerWrite(); !ok {
		t.Fatal("marker write suppressed without a waiter")
	}
	extra.notePong()
	// The extra answer has no ping to answer: it is clamped away, not counted
	// against the probe that is about to claim the next sequence.
	extra.notePong()
	if ping, pong := extra.snapshot(); ping != 1 || pong != 1 {
		t.Fatalf("unsolicited pong shifted the sequence: ping=%d pong=%d", ping, pong)
	}
	seq, ok = extra.claimMarkerWrite()
	if !ok {
		t.Fatal("probe claim suppressed without a waiter")
	}
	w, err = extra.armAt(seq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	extra.notePong()
	if w.pending() {
		t.Fatal("probe answer was lost after an unsolicited pong")
	}
	if result := <-w.result; result.err != nil {
		t.Fatalf("probe answer error %v", result.err)
	}
}

func TestConnectedEchoSecondProbeIsBusy(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(ctx)
		first <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	if _, err := echo.RequestRTT(context.Background()); !errors.Is(err, errEchoProbeBusy) {
		t.Fatalf("second probe error %v", err)
	}
	if conn.writeCount() != 1 {
		t.Fatalf("busy probe wrote %d markers", conn.writeCount())
	}
	cancel()
	got := <-first
	if !errors.Is(got.err, context.Canceled) || got.rtt != 0 {
		t.Fatalf("cancelled probe rtt=%v err=%v", got.rtt, got.err)
	}
	if !writeConnectedMarker(conn, mu, echo, time.Now()) || conn.writeCount() != 2 {
		t.Fatal("hold was not released after cancel")
	}
}

func TestConnectedEchoTimeoutLeavesChannelDirtyUntilAnswered(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(ctx)
		result <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	// The periodic keepalive and the wake retries are suppressed: success is
	// reported without writing, so their timeout semantics are untouched.
	if !writeConnectedMarker(conn, mu, echo, time.Now()) {
		t.Fatal("suppressed marker reported failure")
	}
	if conn.writeCount() != 1 {
		t.Fatal("marker write stole the probe window")
	}
	got := <-result
	if !errors.Is(got.err, context.DeadlineExceeded) || got.rtt != 0 {
		t.Fatalf("timeout rtt=%v err=%v", got.rtt, got.err)
	}
	// The timed-out ping stays outstanding: the channel is dirty and the live
	// echo fails closed until its answer arrives.
	if echo.channelClean() {
		t.Fatal("timed-out ping was rebased away")
	}
	if _, err := echo.RequestRTT(context.Background()); !errors.Is(err, errEchoChannelDirty) {
		t.Fatalf("dirty channel admitted a probe: %v", err)
	}
	if conn.writeCount() != 1 {
		t.Fatalf("dirty channel accepted a marker write: %d", conn.writeCount())
	}
	// Marker writes resume with the next contiguous sequence; its own answer
	// still leaves the timed-out ping outstanding.
	if !writeConnectedMarker(conn, mu, echo, time.Now()) || conn.writeCount() != 2 {
		t.Fatal("marker writes did not resume after timeout")
	}
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if echo.channelClean() {
		t.Fatal("marker answer erased the outstanding probe ping")
	}
	// The timed-out probe's late answer finally reconciles the channel; a later
	// probe is measured normally on the same session.
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if !echo.channelClean() {
		t.Fatal("channel did not reconcile after the late answer")
	}
	respondToConnectedProbe(echo, conn, 10*time.Millisecond)
	rtt, err := echo.RequestRTT(context.Background())
	if err != nil || rtt < 10*time.Millisecond {
		t.Fatalf("later probe rtt=%v err=%v", rtt, err)
	}
}

func TestConnectedEchoSessionCloseCancelsPendingProbe(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	result := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(context.Background())
		result <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	echo.close()
	got := <-result
	if !errors.Is(got.err, context.Canceled) || got.rtt != 0 {
		t.Fatalf("closed session rtt=%v err=%v", got.rtt, got.err)
	}
	if _, err := echo.RequestRTT(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("probe on closed session %v", err)
	}
	if !writeConnectedMarker(conn, mu, echo, time.Now()) || conn.writeCount() != 2 {
		t.Fatal("hold was not released on session close")
	}
	echo.close()
}

func TestConnectedEchoNonMarkerRecordsStayOnDataPath(t *testing.T) {
	conn := newEchoTestConn()
	echo := newConnectedEcho(conn, &sync.Mutex{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(ctx)
		result <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)

	packet := wireGuardTestPacket(4, 52)
	original := append([]byte(nil), packet...)
	liveness := 0
	if !handleConnectedRecord(packet, echo, func() { liveness++ }) {
		t.Fatal("a user packet was consumed as a keepalive marker")
	}
	if !bytes.Equal(packet, original) {
		t.Fatal("a user packet was mutated on the dispatcher path")
	}
	if liveness != 0 {
		t.Fatal("a user packet was counted as a keepalive pong")
	}
	select {
	case got := <-result:
		t.Fatalf("user traffic settled the pending probe: %v", got.err)
	default:
	}
	if handleConnectedRecord([]byte{keepaliveByte}, echo, func() { liveness++ }) {
		t.Fatal("the keepalive marker entered the data path")
	}
	if liveness != 1 {
		t.Fatalf("keepalive liveness count = %d", liveness)
	}
	got := <-result
	if got.err != nil || got.rtt <= 0 {
		t.Fatalf("probe rtt=%v err=%v", got.rtt, got.err)
	}
}

func TestConnectedProbeResultCarriesNoSetupAndNoRTTOnFailure(t *testing.T) {
	cases := []struct {
		err    error
		status string
	}{
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "cancelled"},
		{errEchoProbeBusy, "busy"},
		{errEchoProbeUnavailable, "failed"},
		{errEchoSessionStale, "failed"},
	}
	for _, tc := range cases {
		message := connectedProbeResult("gw-0", 0, tc.err)
		if message["status"] != tc.status {
			t.Fatalf("%v -> status %v", tc.err, message["status"])
		}
		if _, ok := message["rtt_ms"]; ok {
			t.Fatalf("%v exposed rtt_ms", tc.err)
		}
		if _, ok := message["transport_setup_ms"]; ok {
			t.Fatalf("%v exposed transport_setup_ms", tc.err)
		}
	}
	message := connectedProbeResult("gw-0", 12*time.Millisecond, nil)
	if message["status"] != "ok" || message["rtt_ms"] != int64(12) {
		t.Fatalf("successful connected result %v", message)
	}
	if _, ok := message["transport_setup_ms"]; ok {
		t.Fatal("connected result exposed transport_setup_ms")
	}
}

func TestDispatcherConnectedEchoGenerationFence(t *testing.T) {
	d := &Dispatcher{}
	if _, err := d.RequestConnectedRTT(context.Background()); !errors.Is(err, errEchoProbeUnavailable) {
		t.Fatalf("unregistered dispatcher: %v", err)
	}
	mu := &sync.Mutex{}
	first := newConnectedEcho(newEchoTestConn(), mu)
	if first.generation == 0 {
		t.Fatal("connected echo has no generation")
	}
	d.registerConnectedEcho(first)
	if d.connectedEcho.Load() != first {
		t.Fatal("registered session is not current")
	}
	secondConn := newEchoTestConn()
	second := newConnectedEcho(secondConn, mu)
	if second.generation <= first.generation {
		t.Fatalf("generations not increasing: %d then %d", first.generation, second.generation)
	}
	d.registerConnectedEcho(second)
	// An older session must never displace the newer registration, and a stale
	// unregister must never clear it.
	d.registerConnectedEcho(first)
	if d.connectedEcho.Load() != second {
		t.Fatal("an older generation displaced the newer session")
	}
	d.unregisterConnectedEcho(first)
	if d.connectedEcho.Load() != second {
		t.Fatal("a stale unregister removed the current session")
	}
	// A closed session fails closed and can be replaced.
	second.close()
	if _, err := d.RequestConnectedRTT(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed session probe: %v", err)
	}
	third := newConnectedEcho(secondConn, mu)
	d.registerConnectedEcho(third)
	respondToConnectedProbe(third, secondConn, 12*time.Millisecond)
	rtt, err := d.RequestConnectedRTT(context.Background())
	if err != nil || rtt < 12*time.Millisecond {
		t.Fatalf("live session probe rtt=%v err=%v", rtt, err)
	}
	d.unregisterConnectedEcho(third)
	if _, err := d.RequestConnectedRTT(context.Background()); !errors.Is(err, errEchoProbeUnavailable) {
		t.Fatalf("after unregister: %v", err)
	}
}

func TestDispatcherConnectedEchoDropsResultAfterSessionReplacement(t *testing.T) {
	d := &Dispatcher{}
	mu := &sync.Mutex{}
	conn := newEchoTestConn()
	session := newConnectedEcho(conn, mu)
	d.registerConnectedEcho(session)
	respondToConnectedProbe(session, conn, 30*time.Millisecond)
	result := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := d.RequestConnectedRTT(context.Background())
		result <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	// The session is replaced while the echo is in flight: the completed
	// measurement must be dropped, not delivered as fresh RTT.
	replacement := newConnectedEcho(newEchoTestConn(), mu)
	d.registerConnectedEcho(replacement)
	got := <-result
	if !errors.Is(got.err, errEchoSessionStale) || got.rtt != 0 {
		t.Fatalf("stale session result rtt=%v err=%v", got.rtt, got.err)
	}
}

// ─── connected event loop integration ───────────────────────────────────────

func swapManagedConnectedEcho(t *testing.T, echo func(context.Context, *Dispatcher) (time.Duration, error)) {
	t.Helper()
	previous := managedConnectedEcho
	managedConnectedEcho = echo
	t.Cleanup(func() { managedConnectedEcho = previous })
}

func TestManagedConnectedProbeCancelRestartFenceAndBusy(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	firstCancelled := make(chan struct{})
	swapManagedConnectedEcho(t, func(ctx context.Context, _ *Dispatcher) (time.Duration, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			return 0, ctx.Err()
		}
		time.Sleep(20 * time.Millisecond)
		return 20 * time.Millisecond, nil
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("connected probe was never handled")
	}
	// A second manual probe while the first is pending is answered busy and
	// never starts another echo.
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
	waitHostMessage(t, host, "busy connected probe", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" && message["status"] == "busy"
	})
	if calls.Load() != 1 {
		t.Fatalf("busy probe started an echo: calls=%d", calls.Load())
	}
	// cancel_probe releases the in-flight probe; its late result is fenced and
	// a fresh probe on the same node succeeds.
	host.send(t, map[string]any{"type": "cancel_probe"})
	select {
	case <-firstCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("connected probe was not cancelled")
	}
	deadline := time.Now().Add(5 * time.Second)
	var ok map[string]any
	for {
		host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
		if found, present := findHostMessage(host, func(message map[string]any) bool {
			return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" && message["status"] == "ok"
		}); present {
			ok = found
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fresh connected probe never succeeded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rtt, _ := ok["rtt_ms"].(float64); rtt < 20 {
		t.Fatalf("fresh connected probe rtt_ms %#v", ok["rtt_ms"])
	}
	if _, present := ok["transport_setup_ms"]; present {
		t.Fatal("connected result exposed transport_setup_ms")
	}
	// The cancelled probe never produced a result frame.
	host.mu.Lock()
	results := 0
	for _, message := range host.messages {
		if message["type"] == "node_probe_result" && message["node_id"] == "gw-0" && message["status"] != "busy" {
			results++
		}
	}
	host.mu.Unlock()
	if results != 1 {
		t.Fatalf("expected exactly one non-busy connected result, got %d", results)
	}
	cancel()
	<-errCh
}

func swapManagedNonCurrentProbe(t *testing.T, probe func(*managedController, context.Context, string) (managedProbeMeasurement, error)) {
	t.Helper()
	previous := managedNonCurrentProbe
	managedNonCurrentProbe = probe
	t.Cleanup(func() { managedNonCurrentProbe = previous })
}

// TestManagedConnectedNonCurrentProbeUsesDirectEchoWithoutSwitching proves the
// S5 07.2 non-current manual probe: while the VPN is Connected, a probe of a
// node that is not the current one is measured by the direct echo seam on its
// own connection. The live-session echo is untouched, the selection and the
// active child stay exactly as they were, and a second probe while the first
// is pending is answered busy.
func TestManagedConnectedNonCurrentProbeUsesDirectEchoWithoutSwitching(t *testing.T) {
	_, controller, _, host, stub, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var liveEchoCalls atomic.Int32
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		liveEchoCalls.Add(1)
		return 0, errors.New("UNEXPECTED_LIVE_ECHO")
	})
	type directCall struct {
		id          string
		deadline    time.Time
		hasDeadline bool
	}
	calls := make(chan directCall, 4)
	release := make(chan struct{})
	swapManagedNonCurrentProbe(t, func(_ *managedController, ctx context.Context, id string) (managedProbeMeasurement, error) {
		deadline, hasDeadline := ctx.Deadline()
		calls <- directCall{id: id, deadline: deadline, hasDeadline: hasDeadline}
		select {
		case <-release:
		case <-ctx.Done():
			return managedProbeMeasurement{}, ctx.Err()
		}
		return managedProbeMeasurement{Setup: 15 * time.Millisecond, RTT: 7 * time.Millisecond}, nil
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-1"})
	var call directCall
	select {
	case call = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("non-current probe never reached the direct seam")
	}
	if call.id != "gw-1" || !call.hasDeadline {
		t.Fatalf("direct seam call %#v", call)
	}
	if remaining := time.Until(call.deadline); remaining <= 0 || remaining > connectedProbeTimeout {
		t.Fatalf("non-current probe budget %v", remaining)
	}
	// A second manual probe while the non-current probe is pending is busy.
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
	waitHostMessage(t, host, "busy non-current probe", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" && message["status"] == "busy"
	})
	close(release)
	result := waitHostMessage(t, host, "non-current probe result", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-1" && message["status"] == "ok"
	})
	if rtt, _ := result["rtt_ms"].(float64); rtt != 7 {
		t.Fatalf("non-current rtt_ms %#v", result["rtt_ms"])
	}
	if setup, _ := result["transport_setup_ms"].(float64); setup < 15 {
		t.Fatalf("non-current transport_setup_ms %#v", result["transport_setup_ms"])
	}
	if liveEchoCalls.Load() != 0 {
		t.Fatalf("non-current probe touched the live session echo: %d", liveEchoCalls.Load())
	}
	// No switch, no selection change, no second child.
	if controller.selectionID() != "gw-0" {
		t.Fatalf("selection changed: %q", controller.selectionID())
	}
	if stub.count("start:gw-1") != 0 || stub.activeCount() != 1 {
		t.Fatalf("active data plane changed: %v", stub.eventsSnapshot())
	}
	cancel()
	<-errCh
}

// TestManagedConnectedCurrentProbeUsesLiveEchoOnly proves the current node
// keeps the existing live-session echo path and never falls into the direct
// non-current seam.
func TestManagedConnectedCurrentProbeUsesLiveEchoOnly(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var directCalls atomic.Int32
	swapManagedNonCurrentProbe(t, func(*managedController, context.Context, string) (managedProbeMeasurement, error) {
		directCalls.Add(1)
		return managedProbeMeasurement{Setup: time.Second, RTT: time.Second}, nil
	})
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 9 * time.Millisecond, nil
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
	result := waitHostMessage(t, host, "current-node probe result", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" && message["status"] == "ok"
	})
	if rtt, _ := result["rtt_ms"].(float64); rtt != 9 {
		t.Fatalf("live echo rtt_ms %#v", result["rtt_ms"])
	}
	if _, present := result["transport_setup_ms"]; present {
		t.Fatal("current-node result exposed direct setup diagnostics")
	}
	if directCalls.Load() != 0 {
		t.Fatalf("current node used the direct seam: %d", directCalls.Load())
	}
	cancel()
	<-errCh
}

// TestManagedConnectedNonCurrentProbeCancelDropsStaleAndNextWorks proves
// cancel_probe and the shared probeEpoch fence apply to the non-current path:
// the cancelled probe's completion is dropped, and a fresh probe afterwards
// succeeds.
func TestManagedConnectedNonCurrentProbeCancelDropsStaleAndNextWorks(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	firstCancelled := make(chan struct{})
	swapManagedNonCurrentProbe(t, func(_ *managedController, ctx context.Context, _ string) (managedProbeMeasurement, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			return managedProbeMeasurement{Setup: 5 * time.Millisecond}, ctx.Err()
		}
		return managedProbeMeasurement{Setup: 3 * time.Millisecond, RTT: 11 * time.Millisecond}, nil
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-1"})
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("non-current probe never started")
	}
	host.send(t, map[string]any{"type": "cancel_probe"})
	select {
	case <-firstCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("non-current probe was not cancelled")
	}
	deadline := time.Now().Add(5 * time.Second)
	var ok map[string]any
	for {
		host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-1"})
		if found, present := findHostMessage(host, func(message map[string]any) bool {
			return message["type"] == "node_probe_result" && message["node_id"] == "gw-1" && message["status"] == "ok"
		}); present {
			ok = found
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fresh non-current probe never succeeded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rtt, _ := ok["rtt_ms"].(float64); rtt != 11 {
		t.Fatalf("fresh non-current rtt_ms %#v", ok["rtt_ms"])
	}
	// Exactly one non-busy result: the cancelled probe's completion was fenced.
	host.mu.Lock()
	results := 0
	for _, message := range host.messages {
		if message["type"] == "node_probe_result" && message["node_id"] == "gw-1" && message["status"] != "busy" {
			results++
		}
	}
	host.mu.Unlock()
	if results != 1 {
		t.Fatalf("expected exactly one non-busy non-current result, got %d", results)
	}
	cancel()
	<-errCh
}

// TestManagedConnectedNonCurrentProbeFailureMapping proves the non-current
// path reuses the direct probe result vocabulary: failed and timeout carry no
// rtt_ms and no setup diagnostic.
func TestManagedConnectedNonCurrentProbeFailureMapping(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var calls atomic.Int32
	swapManagedNonCurrentProbe(t, func(_ *managedController, _ context.Context, _ string) (managedProbeMeasurement, error) {
		if calls.Add(1) == 1 {
			return managedProbeMeasurement{Setup: 5 * time.Millisecond}, errors.New("TRANSPORT_FAILED")
		}
		return managedProbeMeasurement{Setup: 5 * time.Millisecond}, context.DeadlineExceeded
	})
	awaitStatus := func(description, status string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-1"})
			if found, present := findHostMessage(host, func(message map[string]any) bool {
				return message["type"] == "node_probe_result" && message["node_id"] == "gw-1" && message["status"] == status
			}); present {
				return found
			}
			if time.Now().After(deadline) {
				t.Fatal(description + " never reached the host")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	failed := awaitStatus("failed non-current probe", "failed")
	if _, present := failed["rtt_ms"]; present {
		t.Fatal("failed non-current probe exposed rtt_ms")
	}
	if _, present := failed["transport_setup_ms"]; present {
		t.Fatal("failed non-current probe exposed transport_setup_ms")
	}
	timedOut := awaitStatus("timed out non-current probe", "timeout")
	if _, present := timedOut["rtt_ms"]; present {
		t.Fatal("timed out non-current probe exposed rtt_ms")
	}
	if _, present := timedOut["transport_setup_ms"]; present {
		t.Fatal("timed out non-current probe exposed transport_setup_ms")
	}
	cancel()
	<-errCh
}

// TestManagedNonCurrentProbeDefaultSeamUsesDirectPath pins the production
// wiring of the seam: the default delegates to the existing direct
// runNodeProbe path (catalog validation + own transport + keepalive echo).
func TestManagedNonCurrentProbeDefaultSeamUsesDirectPath(t *testing.T) {
	c, _ := probeTestController(t)
	conn := newEchoTestConn()
	swapProbeSeams(t,
		func(context.Context, wlbs.Node) (net.Conn, func(), error) { return conn, func() {}, nil },
		func(context.Context, net.Conn) (time.Duration, error) { return 4 * time.Millisecond, nil })
	measurement, err := managedNonCurrentProbe(c, context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if measurement.RTT != 4*time.Millisecond || measurement.Setup <= 0 {
		t.Fatalf("default seam measurement %#v", measurement)
	}
}

func TestRTTEchoSequencerKeepsLostAnswerOutstanding(t *testing.T) {
	q := &rttEchoSequencer{}
	if _, ok := q.claimMarkerWrite(); !ok {
		t.Fatal("marker write suppressed without a waiter")
	}
	seq, ok := q.claimMarkerWrite()
	if !ok {
		t.Fatal("probe claim suppressed without a waiter")
	}
	w, err := q.armAt(seq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	q.cancelProbe(w, context.DeadlineExceeded)
	// The lost answer is not rebased away: the ping stays outstanding and the
	// channel stays dirty until every written ping is answered.
	if q.channelClean() {
		t.Fatal("lost answer was rebased away")
	}
	// The very late answer for the cancelled probe reconciles the channel and
	// is consumed exactly once.
	q.notePong()
	if q.channelClean() {
		t.Fatal("one answer reconciled two outstanding pings")
	}
	q.notePong()
	if !q.channelClean() {
		t.Fatal("late answers did not reconcile the channel")
	}
	// A probe after reconciliation claims the next sequence and is settled only
	// by its own answer.
	nextSeq, ok := q.claimMarkerWrite()
	if !ok {
		t.Fatal("probe claim suppressed after reconciliation")
	}
	next, err := q.armAt(nextSeq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	q.notePong()
	if next.pending() {
		t.Fatal("probe after reconciliation was not matched")
	}
	if result := <-next.result; result.err != nil {
		t.Fatalf("probe after reconciliation error %v", result.err)
	}
}

// snapshot reads the correlation counters for assertions. It is defined here,
// on the production type, only for deterministic ordering/suppression tests.
func (q *rttEchoSequencer) snapshot() (ping, pong uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pingSeq, q.pongSeq
}

// gatedEchoConn blocks inside Write while the gate is enabled, so a test can
// deterministically hold the session write lock with a marker write in flight.
type gatedEchoConn struct {
	*echoTestConn
	gate    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newGatedEchoConn() *gatedEchoConn {
	return &gatedEchoConn{
		echoTestConn: newEchoTestConn(),
		entered:      make(chan struct{}, 4),
		release:      make(chan struct{}),
	}
}

func (c *gatedEchoConn) Write(p []byte) (int, error) {
	if c.gate.Load() {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.release
	}
	return c.echoTestConn.Write(p)
}

// echoWriteFault programs one failing write on a faultEchoConn: n == 0 with an
// error models a hard failure, n < len(p) with a nil error a short write.
type echoWriteFault struct {
	n   int
	err error
}

type faultEchoConn struct {
	*echoTestConn
	fault atomic.Pointer[echoWriteFault]
}

func newFaultEchoConn() *faultEchoConn {
	return &faultEchoConn{echoTestConn: newEchoTestConn()}
}

func (c *faultEchoConn) Write(p []byte) (int, error) {
	if f := c.fault.Swap(nil); f != nil {
		return f.n, f.err
	}
	return c.echoTestConn.Write(p)
}

// TestConnectedEchoProbeWaitsForMarkerWriteAndFailsClosed proves the corrected
// ordering between a background marker write and a manual connected probe: the
// marker claims its sequence under the session write lock and keeps that lock
// until its byte is written, so a probe that starts while the marker is inside
// Write cannot overtake it. While the marker's answer is outstanding the probe
// then fails closed on the dirty channel instead of arming a sequence an old
// pong could settle; only after the marker's own pong does a fresh probe claim
// the next contiguous sequence and get settled by its own answer.
func TestConnectedEchoProbeWaitsForMarkerWriteAndFailsClosed(t *testing.T) {
	conn := newGatedEchoConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	conn.gate.Store(true)

	// The test holds the session write lock: the background marker must block
	// on it WITHOUT claiming a sequence. Under the old ordering the claim
	// happened before the lock, so pingSeq would already be 1 here and this
	// assertion fails: that is the defect this test fences.
	mu.Lock()
	markerStart := make(chan struct{})
	markerDone := make(chan bool, 1)
	go func() {
		close(markerStart)
		markerDone <- writeConnectedMarker(conn, mu, echo, time.Now())
	}()
	<-markerStart
	time.Sleep(30 * time.Millisecond)
	if ping, _ := echo.seq.snapshot(); ping != 0 {
		mu.Unlock()
		t.Fatalf("marker claimed sequence %d before acquiring the write lock", ping)
	}
	mu.Unlock()

	select {
	case <-conn.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background marker never entered Write")
	}
	// The marker is inside Write holding the session write lock and has
	// claimed exactly its own sequence.
	if ping, _ := echo.seq.snapshot(); ping != 1 {
		t.Fatalf("background marker pingSeq=%d, want 1", ping)
	}

	probeResult := make(chan echoProbeResult, 1)
	probeStarted := make(chan struct{})
	go func() {
		close(probeStarted)
		rtt, err := echo.RequestRTT(context.Background())
		probeResult <- echoProbeResult{rtt: rtt, err: err}
	}()
	<-probeStarted
	// RequestRTT must block on the session write lock: while the marker write
	// is in flight it can claim no sequence and enter no write.
	time.Sleep(30 * time.Millisecond)
	if ping, _ := echo.seq.snapshot(); ping != 1 {
		t.Fatalf("probe claimed a sequence while the marker held the lock: pingSeq=%d", ping)
	}
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("probe wrote while the marker held the lock: writes=%d", got)
	}
	select {
	case got := <-probeResult:
		t.Fatalf("probe completed while the marker held the lock: rtt=%v err=%v", got.rtt, got.err)
	default:
	}

	// Release the marker: its byte reaches the wire with sequence 1. The probe
	// then sees the unanswered marker ping and fails closed on the dirty
	// channel instead of arming a sequence an old pong could settle.
	close(conn.release)
	if ok := <-markerDone; !ok {
		t.Fatal("background marker reported failure")
	}
	got := <-probeResult
	if !errors.Is(got.err, errEchoChannelDirty) || got.rtt != 0 {
		t.Fatalf("probe on the outstanding marker ping rtt=%v err=%v", got.rtt, got.err)
	}
	if conn.writeCount() != 1 {
		t.Fatalf("probe wrote on the dirty channel: writes=%d", conn.writeCount())
	}
	// The marker's own answer reconciles the channel; the next probe claims the
	// following contiguous sequence and is settled only by its own pong.
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if ping, pong := echo.seq.snapshot(); ping != 1 || pong != 1 {
		t.Fatalf("marker pong mis-attributed: ping=%d pong=%d, want 1/1", ping, pong)
	}
	const probeDelay = 30 * time.Millisecond
	respondToConnectedProbe(echo, conn.echoTestConn, probeDelay)
	rtt, err := echo.RequestRTT(context.Background())
	if err != nil {
		t.Fatalf("probe after reconciliation failed: %v", err)
	}
	if rtt < probeDelay || rtt > time.Second {
		t.Fatalf("probe rtt=%v, want >= its own %v delay", rtt, probeDelay)
	}
	if ping, _ := echo.seq.snapshot(); ping != 2 {
		t.Fatalf("probe after reconciliation pingSeq=%d, want 2", ping)
	}
}

// TestConnectedEchoSuppressedMarkersClaimNothing proves the suppression and
// atomicity rule: while a waiter is armed the background paths claim and write
// nothing (yet report success), and after completion the next marker uses the
// following contiguous sequence.
func TestConnectedEchoSuppressedMarkersClaimNothing(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(ctx)
		result <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	if ping, _ := echo.seq.snapshot(); ping != 1 {
		t.Fatalf("probe pingSeq=%d, want 1", ping)
	}
	for i := 0; i < 3; i++ {
		if !writeConnectedMarker(conn, mu, echo, time.Now()) {
			t.Fatalf("suppressed marker %d reported failure", i)
		}
	}
	if got := conn.writeCount(); got != 1 {
		t.Fatalf("suppressed markers wrote %d markers", got)
	}
	if ping, _ := echo.seq.snapshot(); ping != 1 {
		t.Fatalf("suppressed markers claimed a sequence: pingSeq=%d", ping)
	}

	// The probe's own pong settles it and releases the hold.
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	got := <-result
	if got.err != nil || got.rtt <= 0 {
		t.Fatalf("probe rtt=%v err=%v", got.rtt, got.err)
	}
	if ping, pong := echo.seq.snapshot(); ping != 1 || pong != 1 {
		t.Fatalf("after probe: ping=%d pong=%d, want 1/1", ping, pong)
	}
	// The next marker claims the following contiguous sequence and its pong is
	// counted exactly once.
	if !writeConnectedMarker(conn, mu, echo, time.Now()) {
		t.Fatal("marker after completion reported failure")
	}
	if got := conn.writeCount(); got != 2 {
		t.Fatalf("marker after completion writes=%d, want 2", got)
	}
	if ping, _ := echo.seq.snapshot(); ping != 2 {
		t.Fatalf("marker after completion pingSeq=%d, want 2", ping)
	}
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if ping, pong := echo.seq.snapshot(); ping != 2 || pong != 2 {
		t.Fatalf("marker pong mis-attributed: ping=%d pong=%d, want 2/2", ping, pong)
	}
}

// TestWriteConnectedMarkerRollsBackFailedWrite proves the failure path: a hard
// or short marker write rolls the claim back, clears the poisoned deadline,
// reports false, and leaves the correlation counters contiguous for the next
// marker and probe.
func TestWriteConnectedMarkerRollsBackFailedWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault echoWriteFault
	}{
		{"hard error", echoWriteFault{n: 0, err: errors.New("WRITE_FAILED")}},
		{"short write", echoWriteFault{n: 0, err: nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFaultEchoConn()
			mu := &sync.Mutex{}
			echo := newConnectedEcho(conn, mu)
			conn.fault.Store(&tc.fault)
			if writeConnectedMarker(conn, mu, echo, time.Now()) {
				t.Fatal("failed marker reported success")
			}
			if ping, pong := echo.seq.snapshot(); ping != 0 || pong != 0 {
				t.Fatalf("failed write left ping=%d pong=%d, want 0/0", ping, pong)
			}
			if !conn.deadlineValue().IsZero() {
				t.Fatalf("failed write left deadline %v", conn.deadlineValue())
			}
			if got := conn.writeCount(); got != 0 {
				t.Fatalf("failed write recorded %d writes", got)
			}
			// The next marker claims the correct contiguous sequence.
			if !writeConnectedMarker(conn, mu, echo, time.Now()) {
				t.Fatal("marker after rollback reported failure")
			}
			if got := conn.writeCount(); got != 1 {
				t.Fatalf("marker after rollback writes=%d, want 1", got)
			}
			if ping, _ := echo.seq.snapshot(); ping != 1 {
				t.Fatalf("marker after rollback pingSeq=%d, want 1", ping)
			}
			// Its own pong is counted once, and a later probe claims the next
			// sequence and is settled only by its own answer: the rolled-back
			// claim left no phantom sequence behind.
			handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
			if ping, pong := echo.seq.snapshot(); ping != 1 || pong != 1 {
				t.Fatalf("marker pong mis-attributed: ping=%d pong=%d, want 1/1", ping, pong)
			}
			respondToConnectedProbe(echo, conn.echoTestConn, 10*time.Millisecond)
			rtt, err := echo.RequestRTT(context.Background())
			if err != nil || rtt < 10*time.Millisecond {
				t.Fatalf("later probe rtt=%v err=%v", rtt, err)
			}
			if ping, _ := echo.seq.snapshot(); ping != 2 {
				t.Fatalf("later probe pingSeq=%d, want 2", ping)
			}
		})
	}
}

// TestConnectedEchoCancelKeepsPingOutstanding proves the fail-closed cancel
// recovery: a cancelled probe returns a zero duration, disarms, and leaves its
// ping outstanding (no rebase), so the channel stays dirty and the live echo is
// refused until every written ping is answered; then a later probe keeps a
// contiguous sequence and is settled only by its own pong.
func TestConnectedEchoCancelKeepsPingOutstanding(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan echoProbeResult, 1)
	go func() {
		rtt, err := echo.RequestRTT(ctx)
		result <- echoProbeResult{rtt: rtt, err: err}
	}()
	waitForConnectedWrite(t, conn, 1)
	cancel()
	got := <-result
	if !errors.Is(got.err, context.Canceled) || got.rtt != 0 {
		t.Fatalf("cancelled probe rtt=%v err=%v", got.rtt, got.err)
	}
	if echo.channelClean() {
		t.Fatal("cancelled probe was rebased away")
	}
	if _, err := echo.RequestRTT(context.Background()); !errors.Is(err, errEchoChannelDirty) {
		t.Fatalf("dirty channel admitted a probe: %v", err)
	}
	// Keepalive marker writes resume with the next contiguous sequence.
	if !writeConnectedMarker(conn, mu, echo, time.Now()) {
		t.Fatal("marker after cancel reported failure")
	}
	if got := conn.writeCount(); got != 2 {
		t.Fatalf("marker after cancel writes=%d, want 2", got)
	}
	if ping, _ := echo.seq.snapshot(); ping != 2 {
		t.Fatalf("marker after cancel pingSeq=%d, want 2", ping)
	}
	// The cancelled probe's very late answer and the marker's own answer
	// reconcile both outstanding pings; a single answer is not enough.
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if echo.channelClean() {
		t.Fatal("one late answer reconciled two outstanding pings")
	}
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if !echo.channelClean() {
		t.Fatal("channel did not reconcile after the late answers")
	}
	// The next probe claims sequence 3 and is settled only by its own pong.
	respondToConnectedProbe(echo, conn, 10*time.Millisecond)
	rtt, err := echo.RequestRTT(context.Background())
	if err != nil || rtt < 10*time.Millisecond {
		t.Fatalf("probe after cancel rtt=%v err=%v", rtt, err)
	}
	if ping, _ := echo.seq.snapshot(); ping != 3 {
		t.Fatalf("probe after cancel pingSeq=%d, want 3", ping)
	}
}
