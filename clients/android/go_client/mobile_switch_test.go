package main

// Real switch orchestration at the seam boundary: the real bridge intake, controller
// switch handling, admission/rollback fences, host vpn_result ack and old-child
// teardown run unchanged; only the DTLS/worker tunnel is a synthetic stub.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

type switchTunnelStub struct {
	mu      sync.Mutex
	events  []string
	active  int
	started chan string
}

func newSwitchTunnelStub(t *testing.T) *switchTunnelStub {
	t.Helper()
	return &switchTunnelStub{started: make(chan string, 8)}
}

func (s *switchTunnelStub) start(_ context.Context, plan managedTunnelPlan) (managedTunnelRuntime, error) {
	s.mu.Lock()
	s.events = append(s.events, "start:"+plan.Node.NodeID)
	s.active++
	s.mu.Unlock()
	plan.Authenticated(&wlbs.VPNOK{AccessExpiresAt: plan.Node.Access.ExpiresAt, ServerTime: time.Now().UTC().Format(time.RFC3339)})
	running := true
	var stopOnce sync.Once
	runtime := managedTunnelRuntime{
		Config: "[Interface]\nAddress = 10.0.0.2/32\n[Peer]\nAllowedIPs = 0.0.0.0/0\nEndpoint = 192.0.2.1:1\n",
		Stop: func() {
			stopOnce.Do(func() {
				s.mu.Lock()
				s.events = append(s.events, "stop:"+plan.Node.NodeID)
				if s.active > 0 {
					s.active--
				}
				s.mu.Unlock()
			})
		},
		Restart: func() {
			s.mu.Lock()
			if !running {
				running = true
				s.events = append(s.events, "restart:"+plan.Node.NodeID)
				s.active++
			}
			s.mu.Unlock()
		},
		Running: func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return running
		},
	}
	select {
	case s.started <- plan.Node.NodeID:
	default:
	}
	return runtime, nil
}

func installSwitchTunnelStub(t *testing.T, stub *switchTunnelStub) {
	t.Helper()
	previous := managedTunnelStart
	managedTunnelStart = stub.start
	t.Cleanup(func() { managedTunnelStart = previous })
}

func (s *switchTunnelStub) eventsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *switchTunnelStub) indexOf(event string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, candidate := range s.events {
		if candidate == event {
			return index
		}
	}
	return -1
}

func (s *switchTunnelStub) count(event string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, candidate := range s.events {
		if candidate == event {
			total++
		}
	}
	return total
}

func (s *switchTunnelStub) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *switchTunnelStub) waitEvent(t *testing.T, event string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s.indexOf(event) >= 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel event %q never happened: %v", event, s.eventsSnapshot())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *switchTunnelStub) waitStart(t *testing.T, node string) { s.waitEvent(t, "start:"+node) }
func (s *switchTunnelStub) waitStop(t *testing.T, node string)  { s.waitEvent(t, "stop:"+node) }

func switchFrame(id, node, revision string) map[string]any {
	return map[string]any{"type": "switch_node", "switch_id": id, "node_id": node, "catalog_revision": revision}
}

func waitHostMessage(t *testing.T, host *feedHost, description string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		for _, message := range host.messages {
			if match(message) {
				host.mu.Unlock()
				return message
			}
		}
		host.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the host", description)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func findHostMessage(host *feedHost, match func(map[string]any) bool) (map[string]any, bool) {
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, message := range host.messages {
		if match(message) {
			return message, true
		}
	}
	return nil, false
}

func switchMobileTunnelSetup(t *testing.T, gatewayCount int) (*mobileFeedFixture, *managedController, *managedBridge, *feedHost, *switchTunnelStub, context.CancelFunc, chan error) {
	t.Helper()
	fixture := newMobileFeedFixture(t, gatewayCount, mobileFeedOptions{})
	server := httptest.NewServer(fixture.handler())
	t.Cleanup(server.Close)
	probes := make([]string, 0, gatewayCount)
	for index := 0; index < gatewayCount; index++ {
		probes = append(probes, fmt.Sprintf("gw-%d", index))
	}
	start := mobileFeedStart(t, fixture, server, probes...)
	controller, bridge, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := newSwitchTunnelStub(t)
	installSwitchTunnelStub(t, stub)
	host.waitMessage(t, "catalog")
	bridge.selection <- "gw-0"
	stub.waitStart(t, "gw-0")
	feedWaitFor(t, func() bool { return controller.selectionID() == "gw-0" })
	return fixture, controller, bridge, host, stub, cancel, errCh
}

func TestMobileSwitchRuntimeSelectionByIDBothDirections(t *testing.T) {
	_, controller, _, host, stub, cancel, errCh := switchMobileTunnelSetup(t, 4)
	revision := "7"
	steps := []struct {
		target string
	}{
		{"gw-3"},
		{"gw-1"},
		{"gw-0"},
	}
	previousOwner := "gw-0"
	for index, step := range steps {
		id := fmt.Sprintf("11111111-1111-1111-1111-%012d", index+1)
		host.send(t, switchFrame(id, step.target, revision))
		stub.waitStart(t, step.target)
		started := waitHostMessage(t, host, "target start "+step.target, func(message map[string]any) bool {
			return message["type"] == "vpn_config" && message["node_id"] == step.target && message["switch_id"] == id
		})
		result := waitHostMessage(t, host, "switch_result "+step.target, func(message map[string]any) bool {
			return message["type"] == "switch_result" && message["node_id"] == step.target && message["status"] == "ok" && message["switch_id"] == id
		})
		if result["switch_id"] != id || result["catalog_revision"] != revision || started["switch_id"] != id {
			t.Fatalf("switch frame identity must stay exact: %v %v", result, started)
		}
		feedWaitFor(t, func() bool { return controller.selectionID() == step.target })
		// The committed switch starts the target before the host ack releases the
		// previous child, then the previous child must be stopped and reaped.
		if stub.indexOf("start:"+step.target) > stub.indexOf("stop:"+previousOwner) {
			t.Fatalf("target %s must start before the previous child is stopped: %v", step.target, stub.eventsSnapshot())
		}
		feedWaitFor(t, func() bool { return stub.indexOf("stop:"+previousOwner) >= 0 })
		feedWaitFor(t, func() bool { return stub.activeCount() == 1 })
		feedWaitFor(t, func() bool {
			controller.mu.Lock()
			defer controller.mu.Unlock()
			return len(controller.mobileStops) == 1
		})
		previousOwner = step.target
	}
	// Selection by stable gateway_id: gw-3 is last, gw-1 is non-adjacent, gw-0 first.
	if controller.selectionID() != "gw-0" {
		t.Fatalf("selection by stable id failed: %q", controller.selectionID())
	}
	if stub.count("start:gw-2") != 0 {
		t.Fatal("unselected gateway must never start")
	}
	cancel()
	<-errCh
}

func TestMobileSwitchRuntimeReapsFailedTargetBeforeNextTarget(t *testing.T) {
	_, controller, _, host, stub, cancel, errCh := switchMobileTunnelSetup(t, 4)
	host.setVPNResult(func(request, response map[string]any) {
		if request["node_id"] == "gw-2" {
			response["ok"] = false
		}
	})
	failedID := "11111111-1111-1111-1111-111111111112"
	nextID := "11111111-1111-1111-1111-111111111113"
	host.send(t, switchFrame(failedID, "gw-2", "7"))
	stub.waitStart(t, "gw-2")
	// A newer selection arrives while the previous target is still in flight. The
	// serialized switch handling must reap the failed target before starting it.
	host.send(t, switchFrame(nextID, "gw-1", "7"))

	failed := waitHostMessage(t, host, "failed switch_result for gw-2", func(message map[string]any) bool {
		return message["type"] == "switch_result" && message["node_id"] == "gw-2" && message["status"] == "failed"
	})
	if failed["code"] != "VPN_PROBE_FAILED" || failed["rollback_allowed"] != true {
		t.Fatalf("failed target must carry the real code and the rollback fence: %v", failed)
	}
	stub.waitStop(t, "gw-2")
	stub.waitStart(t, "gw-1")
	if stub.indexOf("stop:gw-2") > stub.indexOf("start:gw-1") {
		t.Fatalf("previous target child must be stopped/reaped before the next target starts: %v", stub.eventsSnapshot())
	}
	waitHostMessage(t, host, "switch_result gw-1", func(message map[string]any) bool {
		return message["type"] == "switch_result" && message["node_id"] == "gw-1" && message["status"] == "ok"
	})
	feedWaitFor(t, func() bool { return controller.selectionID() == "gw-1" })
	feedWaitFor(t, func() bool { return stub.activeCount() == 1 })

	// A late callback of the previous target must not resurrect it: the request waiter
	// is gone and the live proof belongs to the new selection.
	config, ok := findHostMessage(host, func(message map[string]any) bool {
		return message["type"] == "vpn_config" && message["node_id"] == "gw-2"
	})
	if !ok {
		t.Fatal("failed target vpn_config was not observed")
	}
	host.send(t, map[string]any{"type": "vpn_result", "request_id": config["request_id"],
		"runtime_epoch": config["runtime_epoch"], "ok": true, "switch_id": config["switch_id"],
		"catalog_revision": config["catalog_revision"]})
	time.Sleep(100 * time.Millisecond)
	if controller.selectionID() != "gw-1" {
		t.Fatalf("a late callback of the previous target must not become the selection: %q", controller.selectionID())
	}
	if stub.count("start:gw-2") != 1 || stub.activeCount() != 1 {
		t.Fatalf("a late callback must not start the previous target: %v", stub.eventsSnapshot())
	}
	cancel()
	<-errCh
}

func TestMobileSwitchRuntimeUnavailableTargetRollsBackToActiveChild(t *testing.T) {
	fixture, controller, _, host, stub, cancel, errCh := switchMobileTunnelSetup(t, 4)
	gateways := fixture.gatewayBodies()
	if len(gateways) != 4 {
		t.Fatalf("fixture gateways: %d", len(gateways))
	}
	fixture.setCatalog("8", []map[string]any{gateways[0], gateways[1], gateways[3]})
	controller.mobile.runner.Trigger("wake")
	feedWaitFor(t, func() bool {
		snapshot := controller.store.Snapshot()
		return snapshot != nil && snapshot.Revision == "8"
	})
	host.send(t, switchFrame("11111111-1111-1111-1111-111111111114", "gw-2", "8"))
	failed := waitHostMessage(t, host, "failed switch_result for the removed target", func(message map[string]any) bool {
		return message["type"] == "switch_result" && message["node_id"] == "gw-2" && message["status"] == "failed"
	})
	if failed["code"] != "SELECTED_NODE_REMOVED" || failed["rollback_allowed"] != true {
		t.Fatalf("a removed target must fail with the rollback policy: %v", failed)
	}
	if stub.count("start:gw-2") != 0 {
		t.Fatalf("an unavailable target must never start: %v", stub.eventsSnapshot())
	}
	if controller.selectionID() != "gw-0" || stub.activeCount() != 1 {
		t.Fatalf("the active child must keep running after a refused target: sel=%q active=%d",
			controller.selectionID(), stub.activeCount())
	}
	if _, ok := findHostMessage(host, func(message map[string]any) bool {
		return message["type"] == "switch_result" && message["node_id"] == "gw-2" && message["status"] == "ok"
	}); ok {
		t.Fatal("a refused target must never be reported Connected")
	}
	cancel()
	<-errCh
}
