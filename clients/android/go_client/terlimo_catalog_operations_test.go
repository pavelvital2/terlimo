package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

const cycleA = "11111111-1111-4111-8111-111111111111"
const cycleB = "22222222-2222-4222-8222-222222222222"

func TestCatalogOperationReplacementKeepsOldIdentityAndParentAlive(t *testing.T) {
	var ops catalogOperations
	ops.initialize(cycleA)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, finishFirst := ops.begin(parent, false)
	deadline, ok := first.Deadline()
	if !ok || time.Until(deadline) > 65*time.Second || time.Until(deadline) < 64*time.Second {
		t.Fatal("missing finite whole-operation budget")
	}
	if ops.request(cycleA) || !ops.request(cycleB) || ops.request(cycleB) {
		t.Fatal("wrong duplicate/replacement handling")
	}
	if first.Err() != context.Canceled || catalogCycleFromContext(first) != cycleA || parent.Err() != nil {
		t.Fatal("replacement relabeled old result or canceled VPN parent")
	}
	if !finishFirst() {
		t.Fatal("replacement wake lost")
	}
	second, finishSecond := ops.begin(parent, true)
	if catalogCycleFromContext(second) != cycleB || second.Err() != nil {
		t.Fatal("wrong replacement context")
	}
	ops.cancel(cycleA)
	if second.Err() != nil {
		t.Fatal("stale cancel killed newer operation")
	}
	ops.cancel(cycleB)
	if second.Err() != context.Canceled || parent.Err() != nil {
		t.Fatal("operation cancellation touched parent")
	}
	if finishSecond() {
		t.Fatal("unexpected pending operation")
	}
	background, finishBackground := ops.begin(parent, false)
	if catalogCycleFromContext(background) != "" || background.Err() != nil {
		t.Fatal("background inherited completed operation")
	}
	finishBackground()
}

func TestCatalogManualBrowseDedupStillCompletesNewOperation(t *testing.T) {
	host := &feedHost{}
	m := &managedMobile{bridge: newManagedBridge(host, "attempt", func() {})}
	m.catalogOps.initialize(cycleA)
	browse := accountaccess.BrowseCatalogResponse{Gateways: []accountaccess.BrowseGateway{{GatewayID: "g", Name: "Gateway"}}}
	_, finish := m.catalogOps.begin(context.Background(), false)
	m.onBrowse(browse)
	finish()
	if !m.catalogOps.request(cycleB) {
		t.Fatal("new operation rejected")
	}
	_, finish = m.catalogOps.begin(context.Background(), true)
	m.onBrowse(browse)
	finish()
	if len(host.messages) != 2 || host.messages[0]["catalog_cycle"] != cycleA || host.messages[1]["catalog_cycle"] != cycleB {
		t.Fatalf("catalog correlation/dedup: %v", host.messages)
	}
	_, finish = m.catalogOps.begin(context.Background(), false)
	m.onBrowse(browse)
	finish()
	if len(host.messages) != 2 {
		t.Fatal("background unchanged list lost existing dedup")
	}
}

func TestCatalogCanceledBrowseNeverPublishes(t *testing.T) {
	host := &feedHost{}
	m := &managedMobile{bridge: newManagedBridge(host, "attempt", func() {})}
	m.catalogOps.initialize(cycleA)
	_, finish := m.catalogOps.begin(context.Background(), false)
	defer finish()
	m.catalogOps.cancel(cycleA)
	m.onBrowse(accountaccess.BrowseCatalogResponse{})
	if len(host.messages) != 0 {
		t.Fatal("canceled operation published list")
	}
}

func TestCatalogCompletedPublicationSuppressesFurtherProgress(t *testing.T) {
	var ops catalogOperations
	ops.initialize(cycleA)
	ctx, finish := ops.begin(context.Background(), false)
	defer finish()
	host := &feedHost{}
	bridge := newManagedBridge(host, "attempt", func() {})
	if err := bridge.sendContext(ctx, bridgeMessage{"type": "catalog"}); err != nil {
		t.Fatal(err)
	}
	if !catalogProgressComplete(ctx) {
		t.Fatal("publication did not close progress")
	}
}

func TestCatalogCancelBeforeManualDispatchDoesNotStartNewPoll(t *testing.T) {
	var ops catalogOperations
	_, finish := ops.begin(context.Background(), false)
	finish()
	ops.request(cycleA)
	ops.cancel(cycleA)
	ctx, finish := ops.begin(context.Background(), true)
	defer finish()
	if ctx.Err() != context.Canceled {
		t.Fatal("canceled queued refresh became a live background poll")
	}
}

func TestCatalogBridgeRoutesOnlyMatchingAttemptAndValidCycle(t *testing.T) {
	b := newManagedBridge(io.Discard, "attempt", func() {})
	var requests, cancels []string
	b.setCatalogRefreshHooks(func(id string) { requests = append(requests, id) }, func(id string) { cancels = append(cancels, id) })
	input := fmt.Sprintf(`{"v":1,"attempt_id":"old","type":"refresh_manual","catalog_cycle":%q}
{"v":1,"attempt_id":"attempt","type":"refresh_manual","catalog_cycle":"bad"}
{"v":1,"attempt_id":"attempt","type":"refresh_manual","catalog_cycle":%q}
{"v":1,"attempt_id":"attempt","type":"cancel_catalog","catalog_cycle":%q}
`, cycleA, cycleA, cycleA)
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(input)))
	if len(requests) != 1 || requests[0] != cycleA || len(cancels) != 1 || cancels[0] != cycleA {
		t.Fatalf("requests=%v cancels=%v", requests, cancels)
	}
}
