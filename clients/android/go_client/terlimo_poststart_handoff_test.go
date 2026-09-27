package main

// Post-start handoff tests: the pre-admission explicit after-consent Connect must
// carry the SAME gateway through the existing post-start selection, admission and VPN
// gates with no second tap, no second hour and no fallback. The tests drive the real
// mobile feed fixture, the real bridge reader/flow and the in-process bootstrap
// listener; only the VPN apply is stubbed.

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/onboarding"
	"wg-turn-client/servicechannel"
)

type preAdmissionHarness struct {
	fixture    *mobileFeedFixture
	backend    *preAdmissionBackend
	listener   *preAdmissionListener
	host       *feedHost
	controller *managedController
	stub       *mobileVPNStub
	cancel     context.CancelFunc
	errCh      chan error
}

func newPreAdmissionHarness(t *testing.T, options mobileFeedOptions, gatewayCount int, probeIDs ...string) *preAdmissionHarness {
	t.Helper()
	fixture := newMobileFeedFixture(t, gatewayCount, options)
	backend := &preAdmissionBackend{fixture: fixture, lastKey: make(chan string, 4), startedCh: make(chan struct{}, 1)}
	server := httptest.NewServer(backend.handler())
	t.Cleanup(server.Close)
	start := mobileFeedStart(t, fixture, server, probeIDs...)
	listener := newPreAdmissionListener(t)
	previousDial := onboardingBootstrapDial
	onboardingBootstrapDial = func(*servicechannel.Store) onboarding.BootstrapDialer { return listener.dialer() }
	t.Cleanup(func() { onboardingBootstrapDial = previousDial })
	controller, _, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	backend.host = host
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)
	return &preAdmissionHarness{fixture: fixture, backend: backend, listener: listener, host: host,
		controller: controller, stub: stub, cancel: cancel, errCh: errCh}
}

// awaitStart consumes exactly one onboarding start RPC from the in-process gateway.
func (h *preAdmissionHarness) awaitStart(t *testing.T) {
	t.Helper()
	select {
	case <-h.listener.received:
	case <-time.After(5 * time.Second):
		t.Fatal("explicit connect never started the hour")
	}
}

// publishActiveHour makes the server-confirmed hour and the given verified gateways
// visible exactly like the next /me + /gateways cycle after onboarding:started.
func (h *preAdmissionHarness) publishActiveHour(t *testing.T, revision string, gatewayIndexes ...int) {
	t.Helper()
	now := time.Now().UTC()
	h.fixture.setMe(mobileFeedMeBody(mobileFeedOptions{dataAccess: "onboarding_hour", deadline: time.Hour, revision: revision}, now))
	gateways := make([]map[string]any, 0, len(gatewayIndexes))
	for _, index := range gatewayIndexes {
		gateways = append(gateways, mobileFeedGatewayBody(h.fixture.fingerprint, index, h.fixture.validUntil, mobileFeedOptions{}))
	}
	h.fixture.setCatalog(revision, gateways)
	h.controller.mobile.runner.Trigger("manual")
}

func (h *preAdmissionHarness) awaitAttemptResult(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("attempt did not end")
		return nil
	}
}

func waitCatalogSelection(t *testing.T, host *feedHost, selected string) {
	t.Helper()
	feedWaitFor(t, func() bool {
		message, ok := host.find("catalog")
		return ok && message["selected_node_id"] == selected
	})
}

// Explicit pre-admission Connect on a non-first gateway: the pending gateway is
// consumed exactly once, chooseNode binds that exact id, the catalog publishes it and
// the existing VPN apply receives it. The hour intent/start runs exactly once.
func TestPreAdmissionExplicitConnectCarriesGatewayIntoPostStartVPN(t *testing.T) {
	h := newPreAdmissionHarness(t, mobileFeedOptions{dataAccess: "none"}, 3, "gw-0", "gw-1", "gw-2")
	feedWaitFor(t, func() bool { me, _ := h.fixture.calls(); return me > 0 })

	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-1"})
	h.awaitStart(t)
	h.publishActiveHour(t, "9", 0, 1, 2)

	waitCatalogSelection(t, h.host, "gw-1")
	index := h.stub.waitStarted(t)
	if count := h.stub.count(); count != 1 {
		t.Fatalf("exactly one vpn start expected: %d", count)
	}
	if node := h.stub.sample(index).node.NodeID; node != "gw-1" {
		t.Fatalf("post-start VPN must use the explicit gateway: %q", node)
	}
	if got := h.controller.selectionID(); got != "gw-1" {
		t.Fatalf("explicit selection must be durable: %q", got)
	}
	if got := atomic.LoadInt32(&h.backend.intents); got != 1 {
		t.Fatalf("exactly one explicit intent: %d", got)
	}
	if got := atomic.LoadInt32(&h.listener.starts); got != 1 {
		t.Fatalf("exactly one explicit start: %d", got)
	}
	if got := h.controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("pending explicit gateway must already be consumed: %q", got)
	}
	decision := h.controller.mobile.lastDecision()
	if decision == nil || !decision.Admitted || decision.Grant == nil || decision.Grant.NodeID != "gw-1" {
		t.Fatalf("admission decision wrong: %+v", decision)
	}
	h.cancel()
	if err := h.awaitAttemptResult(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected attempt result: %v", err)
	}
}

// Duplicate explicit_connect callbacks, including a different gateway after the hour
// already started, keep one pending and one hour; the post-start VPN still uses the
// first confirmed gateway.
func TestPreAdmissionDuplicateExplicitConnectKeepsSingleGateway(t *testing.T) {
	h := newPreAdmissionHarness(t, mobileFeedOptions{dataAccess: "none"}, 2, "gw-0", "gw-1")
	feedWaitFor(t, func() bool { me, _ := h.fixture.calls(); return me > 0 })

	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-1"})
	h.awaitStart(t)
	// A repeated tap (same and then a different key) must neither start a second
	// hour nor replace the confirmed pending gateway.
	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-1"})
	time.Sleep(50 * time.Millisecond)
	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-0"})
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&h.backend.intents); got != 1 {
		t.Fatalf("duplicate callbacks must not create a second intent: %d", got)
	}
	if got := atomic.LoadInt32(&h.listener.starts); got != 1 {
		t.Fatalf("duplicate callbacks must not create a second start: %d", got)
	}

	h.publishActiveHour(t, "9", 0, 1)
	waitCatalogSelection(t, h.host, "gw-1")
	index := h.stub.waitStarted(t)
	if count := h.stub.count(); count != 1 {
		t.Fatalf("exactly one vpn start expected: %d", count)
	}
	if node := h.stub.sample(index).node.NodeID; node != "gw-1" {
		t.Fatalf("duplicates must not fall back to another gateway: %q", node)
	}
	if got := h.controller.selectionID(); got != "gw-1" {
		t.Fatalf("selection wrong after duplicates: %q", got)
	}
	h.cancel()
	if err := h.awaitAttemptResult(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected attempt result: %v", err)
	}
}

// The confirmed explicit gateway absent from the verified catalog is a bounded refusal:
// no VPN, no alternative gateway, no second explicit onboarding.
func TestPreAdmissionExplicitConnectAbsentGatewayBoundedRefusal(t *testing.T) {
	h := newPreAdmissionHarness(t, mobileFeedOptions{dataAccess: "none"}, 2, "gw-0", "gw-1")
	feedWaitFor(t, func() bool { me, _ := h.fixture.calls(); return me > 0 })

	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-1"})
	h.awaitStart(t)
	// The hour is active, but the verified catalog only carries the other gateway.
	h.publishActiveHour(t, "9", 0)

	err := h.awaitAttemptResult(t)
	if managedCode(err) != "SELECTED_NODE_REMOVED" {
		t.Fatalf("absent gateway must refuse with SELECTED_NODE_REMOVED: %v", err)
	}
	if count := h.stub.count(); count != 0 {
		t.Fatalf("absent gateway must not start a VPN: %d", count)
	}
	if got := h.controller.selectionID(); got != "" {
		t.Fatalf("absent gateway must not leave a selection: %q", got)
	}
	if got := atomic.LoadInt32(&h.backend.intents); got != 1 {
		t.Fatalf("no second explicit onboarding allowed: %d", got)
	}
	if got := atomic.LoadInt32(&h.listener.starts); got != 1 {
		t.Fatalf("no second explicit start allowed: %d", got)
	}
	if got := h.controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("pending explicit gateway must be cleared: %q", got)
	}
}

// The confirmed explicit gateway present but not admitted by the verified pair is a
// bounded admission refusal: no VPN, no fallback, no second explicit onboarding.
func TestPreAdmissionExplicitConnectNotAdmittedBoundedRefusal(t *testing.T) {
	h := newPreAdmissionHarness(t, mobileFeedOptions{dataAccess: "none"}, 2, "gw-0", "gw-1")
	feedWaitFor(t, func() bool { me, _ := h.fixture.calls(); return me > 0 })

	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-1"})
	h.awaitStart(t)
	now := time.Now().UTC()
	h.fixture.setMe(mobileFeedMeBody(mobileFeedOptions{dataAccess: "onboarding_hour", deadline: time.Hour,
		revision: "9", bindingStatus: "deactivated"}, now))
	h.fixture.setCatalog("9", []map[string]any{
		mobileFeedGatewayBody(h.fixture.fingerprint, 0, h.fixture.validUntil, mobileFeedOptions{}),
		mobileFeedGatewayBody(h.fixture.fingerprint, 1, h.fixture.validUntil, mobileFeedOptions{}),
	})
	h.controller.mobile.runner.Trigger("manual")

	err := h.awaitAttemptResult(t)
	if managedCode(err) != "BINDING_DEACTIVATED" {
		t.Fatalf("not-admitted gateway must refuse with its bounded reason: %v", err)
	}
	if count := h.stub.count(); count != 0 {
		t.Fatalf("not-admitted gateway must not start a VPN: %d", count)
	}
	if got := h.controller.selectionID(); got != "" {
		t.Fatalf("not-admitted gateway must not leave a selection: %q", got)
	}
	if got := atomic.LoadInt32(&h.backend.intents); got != 1 {
		t.Fatalf("no second explicit onboarding allowed: %d", got)
	}
	if got := h.controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("pending explicit gateway must be cleared: %q", got)
	}
}

// Background refresh/ready alone (no explicit_connect) must never start the hour, a
// selection or a VPN: runSelected keeps waiting for the host selection.
func TestPreAdmissionBackgroundReadyAloneStartsNoHourOrVPN(t *testing.T) {
	h := newPreAdmissionHarness(t, mobileFeedOptions{dataAccess: "onboarding_hour"}, 2, "gw-0", "gw-1")
	h.host.waitMessage(t, "catalog")
	if got := atomic.LoadInt32(&h.backend.intents); got != 0 {
		t.Fatalf("background ready must not begin an explicit intent: %d", got)
	}
	if got := atomic.LoadInt32(&h.listener.starts); got != 0 {
		t.Fatalf("background ready must not start the hour: %d", got)
	}
	time.Sleep(100 * time.Millisecond)
	if count := h.stub.count(); count != 0 {
		t.Fatalf("background ready must not start a VPN: %d", count)
	}
	if got := h.controller.selectionID(); got != "" {
		t.Fatalf("background ready must not select a gateway: %q", got)
	}
	if got := h.controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("background ready must not arm a pending gateway: %q", got)
	}
	h.cancel()
	if err := h.awaitAttemptResult(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected attempt result: %v", err)
	}
}

// A cancel before the verified hour arrives leaves no selection and no VPN start, and
// the pending gateway never outlives the attempt.
func TestPreAdmissionExplicitConnectCancelBeforeReadyNoSelectionOrVPN(t *testing.T) {
	h := newPreAdmissionHarness(t, mobileFeedOptions{dataAccess: "none"}, 2, "gw-0", "gw-1")
	feedWaitFor(t, func() bool { me, _ := h.fixture.calls(); return me > 0 })

	h.host.send(t, map[string]any{"type": "explicit_connect", "gateway_key": "gw-1"})
	h.awaitStart(t)
	h.cancel()
	if err := h.awaitAttemptResult(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pre-admission attempt must report context.Canceled: %v", err)
	}
	if count := h.stub.count(); count != 0 {
		t.Fatalf("canceled attempt must not start a VPN: %d", count)
	}
	if got := h.controller.selectionID(); got != "" {
		t.Fatalf("canceled attempt must not leave a selection: %q", got)
	}
	if got := h.controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("attempt teardown must clear the pending gateway: %q", got)
	}
}

// Paid/trial regression: without an explicit pending, runSelected keeps the existing
// waitForNode selection path.
func TestPaidSelectionPathKeepsWaitForNodeWithoutPending(t *testing.T) {
	fixture := newMobileFeedFixture(t, 2, mobileFeedOptions{})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0", "gw-1")
	controller, bridge, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	host.waitMessage(t, "catalog")
	if got := controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("plain catalog must not arm an explicit gateway: %q", got)
	}
	bridge.selection <- "gw-1"
	index := stub.waitStarted(t)
	if node := stub.sample(index).node.NodeID; node != "gw-1" {
		t.Fatalf("waitForNode selection must reach the VPN: %q", node)
	}
	if got := controller.selectionID(); got != "gw-1" {
		t.Fatalf("selection wrong: %q", got)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected attempt result: %v", err)
	}
}

// Arm/consume boundary: empty keys never arm, the first armed value wins (a second
// callback never replaces it) and consumption happens exactly once.
func TestPendingExplicitGatewayArmConsumeSemantics(t *testing.T) {
	controller := &managedController{}
	controller.armPendingExplicitGateway("")
	if got := controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("empty key must not arm: %q", got)
	}
	controller.armPendingExplicitGateway("gw-a")
	controller.armPendingExplicitGateway("gw-b")
	if got := controller.consumePendingExplicitGateway(); got != "gw-a" {
		t.Fatalf("first armed gateway must win: %q", got)
	}
	if got := controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("consume must clear the pending gateway: %q", got)
	}
	controller.armPendingExplicitGateway("gw-c")
	controller.clearPendingExplicitGateway()
	if got := controller.consumePendingExplicitGateway(); got != "" {
		t.Fatalf("teardown clear must drop the pending gateway: %q", got)
	}
}
