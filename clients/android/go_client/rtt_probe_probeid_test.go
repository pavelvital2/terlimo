package main

// S5 07.2 probe attribution: local probe_id schema, dirty-channel fail-closed
// gate and the connected-loop direct fallback. These tests are compiled only
// with the fix; the parent-commit regression lives in
// rtt_probe_attribution_test.go.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

// ─── connected-channel fail-closed gate ─────────────────────────────────────

func TestConnectedEchoDirtyChannelFailsClosed(t *testing.T) {
	conn := newEchoTestConn()
	mu := &sync.Mutex{}
	echo := newConnectedEcho(conn, mu)
	// One marker written with no answer: the channel is dirty.
	if !writeConnectedMarker(conn, mu, echo, time.Now()) {
		t.Fatal("marker write failed")
	}
	if echo.channelClean() {
		t.Fatal("unanswered ping left the channel clean")
	}
	rtt, err := echo.RequestRTT(context.Background())
	if !errors.Is(err, errEchoChannelDirty) || rtt != 0 {
		t.Fatalf("dirty channel probe rtt=%v err=%v", rtt, err)
	}
	if conn.writeCount() != 1 {
		t.Fatalf("dirty channel accepted a marker write: %d", conn.writeCount())
	}
	// The answer reconciles the channel and re-admits the live echo.
	handleConnectedRecord([]byte{keepaliveByte}, echo, nil)
	if !echo.channelClean() {
		t.Fatal("pong did not reconcile the channel")
	}
	respondToConnectedProbe(echo, conn, 10*time.Millisecond)
	rtt, err = echo.RequestRTT(context.Background())
	if err != nil || rtt < 10*time.Millisecond {
		t.Fatalf("reconciled channel probe rtt=%v err=%v", rtt, err)
	}
}

// ─── local bridge probe_id schema ───────────────────────────────────────────

// readBridgeFrames runs the real bridge reader over the given host frames and
// returns the bridge and everything it wrote back before the reader returned.
func readBridgeFrames(t *testing.T, frames ...map[string]any) (*managedBridge, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	bridge := newManagedBridge(&out, "attempt", cancel)
	var raw strings.Builder
	for _, frame := range frames {
		frame["v"] = 1
		frame["attempt_id"] = "attempt"
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteByte('\n')
	}
	bridge.read(ctx, bufio.NewScanner(strings.NewReader(raw.String())))
	return bridge, out.String()
}

func TestManagedBridgeProbeIDValidationAndRoute(t *testing.T) {
	// A valid id is routed with attribution and produces no immediate frame.
	bridge, out := readBridgeFrames(t, map[string]any{"type": "probe_node", "node_id": "second", "probe_id": "run-1"})
	if out != "" {
		t.Fatalf("valid probe produced output %q", out)
	}
	select {
	case request := <-bridge.probe:
		if request.NodeID != "second" || request.ProbeID != "run-1" || !request.HasProbeID {
			t.Fatalf("routed request %#v", request)
		}
	default:
		t.Fatal("valid probe was not routed")
	}

	// A legacy command without the field is routed with no attribution and
	// produces no immediate frame.
	bridge, out = readBridgeFrames(t, map[string]any{"type": "probe_node", "node_id": "second"})
	if out != "" {
		t.Fatalf("legacy probe produced output %q", out)
	}
	select {
	case request := <-bridge.probe:
		if request.NodeID != "second" || request.HasProbeID || request.ProbeID != "" {
			t.Fatalf("legacy request %#v", request)
		}
	default:
		t.Fatal("legacy probe was not routed")
	}

	// A malformed or oversized id fails closed: status failed, no id echoed,
	// no rtt/setup, and no probe is ever routed.
	oversized := strings.Repeat("a", 65)
	cases := []struct {
		name  string
		value any
	}{
		{"empty", ""},
		{"space", "bad id"},
		{"slash", "run/1"},
		{"dot", "run.1"},
		{"unicode", "запуск"},
		{"oversized", oversized},
		{"not a string", float64(7)},
	}
	for _, tc := range cases {
		bridge, out = readBridgeFrames(t, map[string]any{"type": "probe_node", "node_id": "second", "probe_id": tc.value})
		select {
		case request := <-bridge.probe:
			t.Fatalf("%s: malformed probe was routed: %#v", tc.name, request)
		default:
		}
		var frame bridgeMessage
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &frame); err != nil {
			t.Fatalf("%s: output %q is not a frame: %v", tc.name, out, err)
		}
		if frame["type"] != "node_probe_result" || frame["node_id"] != "second" || frame["status"] != "failed" {
			t.Fatalf("%s: fail-closed frame %v", tc.name, frame)
		}
		for _, key := range []string{"probe_id", "rtt_ms", "transport_setup_ms"} {
			if _, present := frame[key]; present {
				t.Fatalf("%s: fail-closed frame exposed %s: %v", tc.name, key, frame)
			}
		}
	}

	// A full probe channel answers busy and still echoes the run id.
	bridge, out = readBridgeFrames(t,
		map[string]any{"type": "probe_node", "node_id": "second", "probe_id": "run-1"},
		map[string]any{"type": "probe_node", "node_id": "second", "probe_id": "run-2"})
	if out == "" {
		t.Fatal("full probe channel did not answer busy")
	}
	var busy bridgeMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &busy); err != nil {
		t.Fatalf("busy output %q: %v", out, err)
	}
	if busy["status"] != "busy" || busy["probe_id"] != "run-2" {
		t.Fatalf("busy frame %v", busy)
	}
	select {
	case request := <-bridge.probe:
		if request.ProbeID != "run-1" {
			t.Fatalf("first request displaced: %#v", request)
		}
	default:
		t.Fatal("first request missing")
	}
}

// ─── connected loop probe_id echo ───────────────────────────────────────────

func TestManagedConnectedProbeIDOkEcho(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 7 * time.Millisecond, nil
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-ok_1"})
	result := waitHostMessage(t, host, "attributed ok probe", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "ok" && message["probe_id"] == "run-ok_1"
	})
	if rtt, _ := result["rtt_ms"].(float64); rtt != 7 {
		t.Fatalf("ok rtt_ms %#v", result["rtt_ms"])
	}
	if _, present := result["transport_setup_ms"]; present {
		t.Fatal("live ok result exposed transport_setup_ms")
	}
	cancel()
	<-errCh
}

func TestManagedConnectedProbeIDTimeoutEcho(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 0, context.DeadlineExceeded
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-timeout"})
	result := waitHostMessage(t, host, "attributed timeout probe", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "timeout" && message["probe_id"] == "run-timeout"
	})
	for _, key := range []string{"rtt_ms", "transport_setup_ms"} {
		if _, present := result[key]; present {
			t.Fatalf("timeout result exposed %s", key)
		}
	}
	cancel()
	<-errCh
}

func TestManagedConnectedProbeIDFailedEcho(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 0, errEchoProbeUnavailable
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-failed"})
	result := waitHostMessage(t, host, "attributed failed probe", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "failed" && message["probe_id"] == "run-failed"
	})
	for _, key := range []string{"rtt_ms", "transport_setup_ms"} {
		if _, present := result[key]; present {
			t.Fatalf("failed result exposed %s", key)
		}
	}
	cancel()
	<-errCh
}

func TestManagedConnectedProbeIDLegacyNoEcho(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 7 * time.Millisecond, nil
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
	result := waitHostMessage(t, host, "legacy probe", func(message map[string]any) bool {
		if message["type"] != "node_probe_result" || message["node_id"] != "gw-0" || message["status"] != "ok" {
			return false
		}
		_, attributed := message["probe_id"]
		return !attributed
	})
	if rtt, _ := result["rtt_ms"].(float64); rtt != 7 {
		t.Fatalf("legacy rtt_ms %#v", result["rtt_ms"])
	}
	cancel()
	<-errCh
}

func TestManagedConnectedProbeIDBusyEcho(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	swapManagedConnectedEcho(t, func(ctx context.Context, _ *Dispatcher) (time.Duration, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return 11 * time.Millisecond, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-first"})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first probe never started")
	}
	// A second manual probe is answered busy with its own run id echoed.
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-busy"})
	busy := waitHostMessage(t, host, "attributed busy probe", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "busy" && message["probe_id"] == "run-busy"
	})
	if _, present := busy["rtt_ms"]; present {
		t.Fatal("busy result exposed rtt_ms")
	}
	// A legacy busy probe carries no id at all.
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0"})
	waitHostMessage(t, host, "legacy busy probe", func(message map[string]any) bool {
		if message["type"] != "node_probe_result" || message["status"] != "busy" || message["node_id"] != "gw-0" {
			return false
		}
		_, attributed := message["probe_id"]
		return !attributed
	})
	close(release)
	first := waitHostMessage(t, host, "first attributed result", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["status"] == "ok" && message["probe_id"] == "run-first"
	})
	if rtt, _ := first["rtt_ms"].(float64); rtt != 11 {
		t.Fatalf("first rtt_ms %#v", first["rtt_ms"])
	}
	cancel()
	<-errCh
}

func TestManagedConnectedProbeIDMalformedFailsClosed(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var liveCalls atomic.Int32
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		liveCalls.Add(1)
		return 3 * time.Millisecond, nil
	})
	malformed := []struct {
		name  string
		value any
	}{
		{"empty", ""},
		{"space", "bad id"},
		{"slash", "run/1"},
		{"dot", "run.1"},
		{"unicode", "запуск"},
		{"oversized", strings.Repeat("a", 65)},
		{"not a string", float64(7)},
	}
	for _, tc := range malformed {
		host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": tc.value})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		failed := make([]map[string]any, 0, len(malformed))
		for _, message := range host.messages {
			if message["type"] == "node_probe_result" && message["node_id"] == "gw-0" && message["status"] == "failed" {
				failed = append(failed, message)
			}
		}
		host.mu.Unlock()
		if len(failed) == len(malformed) {
			for _, frame := range failed {
				if _, present := frame["probe_id"]; present {
					t.Fatalf("malformed id echoed: %v", frame)
				}
				for _, key := range []string{"rtt_ms", "transport_setup_ms"} {
					if _, present := frame[key]; present {
						t.Fatalf("failed frame exposed %s: %v", key, frame)
					}
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("malformed probes produced %d failed frames, want %d", len(failed), len(malformed))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if liveCalls.Load() != 0 {
		t.Fatalf("malformed probe reached the live echo: %d", liveCalls.Load())
	}
	// The loop still answers a valid attributed probe afterwards.
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-after"})
	waitHostMessage(t, host, "probe after malformed frames", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["status"] == "ok" && message["probe_id"] == "run-after"
	})
	cancel()
	<-errCh
}

// ─── dirty live echo -> direct fallback ─────────────────────────────────────

type directProbeCall struct {
	id          string
	deadline    time.Time
	hasDeadline bool
}

func TestManagedConnectedDirtyLiveEchoFallsBackToDirectProbe(t *testing.T) {
	_, controller, _, host, stub, cancel, errCh := switchMobileTunnelSetup(t, 2)
	var liveCalls atomic.Int32
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		liveCalls.Add(1)
		return 0, errEchoChannelDirty
	})
	calls := make(chan directProbeCall, 1)
	swapManagedNonCurrentProbe(t, func(_ *managedController, ctx context.Context, id string) (managedProbeMeasurement, error) {
		deadline, hasDeadline := ctx.Deadline()
		calls <- directProbeCall{id: id, deadline: deadline, hasDeadline: hasDeadline}
		return managedProbeMeasurement{Setup: 21 * time.Millisecond, RTT: 13 * time.Millisecond}, nil
	})

	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-fallback"})
	result := waitHostMessage(t, host, "dirty fallback result", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "ok" && message["probe_id"] == "run-fallback"
	})
	select {
	case call := <-calls:
		if call.id != "gw-0" || !call.hasDeadline {
			t.Fatalf("fallback call %#v", call)
		}
		if remaining := time.Until(call.deadline); remaining <= 0 || remaining > connectedProbeTimeout {
			t.Fatalf("fallback budget %v", remaining)
		}
	default:
		t.Fatal("dirty live echo never reached the direct seam")
	}
	if rtt, _ := result["rtt_ms"].(float64); rtt != 13 {
		t.Fatalf("fallback rtt_ms %#v", result["rtt_ms"])
	}
	if setup, _ := result["transport_setup_ms"].(float64); setup < 21 {
		t.Fatalf("fallback transport_setup_ms %#v", result["transport_setup_ms"])
	}
	if liveCalls.Load() != 1 {
		t.Fatalf("live echo calls %d", liveCalls.Load())
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

func TestManagedConnectedDirtyLiveEchoFallbackTimeout(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 0, errEchoChannelDirty
	})
	swapManagedNonCurrentProbe(t, func(*managedController, context.Context, string) (managedProbeMeasurement, error) {
		return managedProbeMeasurement{Setup: 5 * time.Millisecond}, context.DeadlineExceeded
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-fallback-timeout"})
	result := waitHostMessage(t, host, "dirty fallback timeout", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "timeout" && message["probe_id"] == "run-fallback-timeout"
	})
	for _, key := range []string{"rtt_ms", "transport_setup_ms"} {
		if _, present := result[key]; present {
			t.Fatalf("fallback timeout exposed %s", key)
		}
	}
	cancel()
	<-errCh
}

func TestManagedConnectedDirtyLiveEchoFallbackFailed(t *testing.T) {
	_, _, _, host, _, cancel, errCh := switchMobileTunnelSetup(t, 2)
	swapManagedConnectedEcho(t, func(context.Context, *Dispatcher) (time.Duration, error) {
		return 0, errEchoChannelDirty
	})
	swapManagedNonCurrentProbe(t, func(*managedController, context.Context, string) (managedProbeMeasurement, error) {
		return managedProbeMeasurement{Setup: 5 * time.Millisecond}, errors.New("TRANSPORT_FAILED")
	})
	host.send(t, map[string]any{"type": "probe_node", "node_id": "gw-0", "probe_id": "run-fallback-failed"})
	result := waitHostMessage(t, host, "dirty fallback failed", func(message map[string]any) bool {
		return message["type"] == "node_probe_result" && message["node_id"] == "gw-0" &&
			message["status"] == "failed" && message["probe_id"] == "run-fallback-failed"
	})
	for _, key := range []string{"rtt_ms", "transport_setup_ms"} {
		if _, present := result[key]; present {
			t.Fatalf("fallback failure exposed %s", key)
		}
	}
	cancel()
	<-errCh
}

// ─── pre-catalog path probe_id ──────────────────────────────────────────────

func TestManagedWaitForNodeProbeIDBusyAndResultEcho(t *testing.T) {
	c, recorder := probeTestController(t)
	echoConn := newEchoTestConn()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	swapProbeSeams(t,
		func(ctx context.Context, _ wlbs.Node) (net.Conn, func(), error) {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			return echoConn, func() {}, nil
		},
		func(context.Context, net.Conn) (time.Duration, error) { return 6 * time.Millisecond, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan waitNodeResult, 1)
	go func() {
		id, err := c.waitForNode(ctx)
		done <- waitNodeResult{id, err}
	}()
	c.bridge.probe <- managedProbeRequest{NodeID: "test", ProbeID: "run-first", HasProbeID: true}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pre-catalog probe never reached the transport seam")
	}
	c.bridge.probe <- managedProbeRequest{NodeID: "test", ProbeID: "run-busy", HasProbeID: true}
	waitProbeRecorderFrame(t, recorder, "attributed pre-catalog busy", func(message bridgeMessage) bool {
		return message["status"] == "busy" && message["probe_id"] == "run-busy"
	})
	close(release)
	ok := waitProbeRecorderFrame(t, recorder, "attributed pre-catalog result", func(message bridgeMessage) bool {
		return message["status"] == "ok" && message["probe_id"] == "run-first"
	})
	if rtt, _ := ok["rtt_ms"].(float64); rtt != 6 {
		t.Fatalf("pre-catalog rtt_ms %#v", ok["rtt_ms"])
	}
	// A legacy command without the field produces a legacy frame: no id.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case c.bridge.probe <- managedProbeRequest{NodeID: "test"}:
		default:
		}
		if findProbeRecorderFrame(recorder, func(message bridgeMessage) bool {
			if message["status"] != "ok" {
				return false
			}
			_, attributed := message["probe_id"]
			return !attributed
		}) != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("legacy pre-catalog probe never succeeded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.bridge.selection <- "test"
	select {
	case r := <-done:
		if r.err != nil || r.id != "test" {
			t.Fatalf("waitForNode %q %v", r.id, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForNode did not stop")
	}
}

func findProbeRecorderFrame(recorder *probeBridgeRecorder, match func(bridgeMessage) bool) bridgeMessage {
	for _, message := range recorder.probeResults() {
		if match(message) {
			return message
		}
	}
	return nil
}

func waitProbeRecorderFrame(t *testing.T, recorder *probeBridgeRecorder, description string, match func(bridgeMessage) bool) bridgeMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if found := findProbeRecorderFrame(recorder, match); found != nil {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the host", description)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
