package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"wg-turn-client/accountaccess"
	"wg-turn-client/wlbs"
)

func TestMobileVerifiedCatalogReplacesBrowseDisplay(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {}), start: managedStart{MobileBaseURL: "https://mobile.invalid"}}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	controller.saved.SelectedNodeID = "gw-0"
	mobile := &managedMobile{controller: controller, bridge: controller.bridge, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	mobile.onBrowse(accountaccess.BrowseCatalogResponse{
		Gateways: []accountaccess.BrowseGateway{{GatewayID: "gw-0", Name: "STEP036"}},
	})
	if message, ok := host.find("catalog"); !ok || message["catalog_mode"] != "browse" {
		t.Fatalf("browse event did not reach real bridge: %v", message)
	}
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if !mobile.readySnapshot() || controller.store.Snapshot() == nil ||
		len(controller.store.Snapshot().Nodes) != 1 {
		t.Fatal("verified credential catalog did not reach native store")
	}
	message, ok := host.find("catalog")
	if !ok || message["catalog_mode"] != nil {
		t.Fatalf("later credential event did not replace browse: %v", message)
	}
	if nodes, ok := message["nodes"].([]any); !ok || len(nodes) != 1 {
		t.Fatalf("later credential node missing: %v", message)
	}
	if state, ok := host.find("state"); ok {
		t.Fatalf("background catalog refresh changed host phase: %v", state)
	}
}

func TestMobileVerifiedCatalogDoesNotAnnounceFailedProjection(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {})}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	catalog := feedDecodeCatalog(t, fixture)
	catalog.Gateways = nil
	mobile.onVerified(feedDecodeMe(t, fixture.me), catalog)
	if mobile.readySnapshot() || mobile.verifiedErr == nil {
		t.Fatal("failed projection marked mobile ready")
	}
	if _, ok := host.find("catalog"); ok {
		t.Fatal("failed projection announced a credential catalog")
	}
}

func TestMobileVerifiedCatalogSendFailureNotReady(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	writer := runnerTestWriter(func([]byte) (int, error) { return 0, io.ErrClosedPipe })
	controller := &managedController{bridge: newManagedBridge(writer, "attempt", func() {})}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if controller.store.Snapshot() == nil || mobile.readySnapshot() || mobile.verifiedErr == nil {
		t.Fatal("failed bridge send was reported as a ready host catalog")
	}
}

func TestMobileLaterPublishFailureKeepsLastGoodReadyButReportsError(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	host := &feedHost{}
	fail := false
	writer := runnerTestWriter(func(frame []byte) (int, error) {
		if fail {
			return 0, io.ErrClosedPipe
		}
		return host.Write(frame)
	})
	controller := &managedController{bridge: newManagedBridge(writer, "attempt", func() {})}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if !mobile.readySnapshot() || mobile.verifiedErr != nil {
		t.Fatal("initial catalog was not ready")
	}
	first, ok := host.find("catalog")
	if !ok {
		t.Fatal("initial host catalog missing")
	}
	fail = true
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	last, ok := host.find("catalog")
	if !ok || last["revision"] != first["revision"] || !mobile.readySnapshot() || mobile.verifiedErr == nil {
		t.Fatal("failed later publish lost last-good readiness or reported a new success")
	}
}

func TestMobileCancelledAttemptDoesNotPublishCatalog(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {})}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint,
		verifiedSig: make(chan struct{}, 1), runCtx: ctx}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if _, ok := host.find("catalog"); ok || mobile.verifiedErr == nil || mobile.readySnapshot() {
		t.Fatal("cancelled attempt published a catalog")
	}
}

func TestMobileCancelledDuringCatalogSendDoesNotBecomeReady(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := runnerTestWriter(func(frame []byte) (int, error) { cancel(); return len(frame), nil })
	controller := &managedController{bridge: newManagedBridge(writer, "old-attempt", func() {})}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1), runCtx: ctx}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if mobile.readySnapshot() || !errors.Is(mobile.verifiedErr, context.Canceled) {
		t.Fatal("cancellation during send announced readiness")
	}
}

func TestMobileVerifiedCatalogPreservesRuntimePhaseAndAttempt(t *testing.T) {
	for _, phase := range []string{"Connected", "KillSwitch"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
			host := &feedHost{}
			controller := &managedController{bridge: newManagedBridge(host, "current-attempt", func() {})}
			controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
			controller.saved.SelectedNodeID = "gw-0"
			controller.state(phase)
			mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
			controller.mobile = mobile
			mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
			catalog, ok := host.find("catalog")
			if !ok || catalog["attempt_id"] != "current-attempt" || catalog["selected_node_id"] != "gw-0" {
				t.Fatal("snapshot lost attempt/selection")
			}
			state, ok := host.find("state")
			if !ok || state["state"] != phase {
				t.Fatal("verified snapshot changed runtime phase")
			}
			if _, ok := host.find("vpn_config"); ok {
				t.Fatal("snapshot initiated VPN")
			}
		})
	}
}

func TestManagedCatalogPublisherNilSnapshotHasNoEvents(t *testing.T) {
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {})}
	if err := controller.publishCatalogContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.find("catalog"); ok {
		t.Fatal("nil snapshot published catalog")
	}
	if _, ok := host.find("state"); ok {
		t.Fatal("nil snapshot announced CatalogReady")
	}
}

func TestManagedCatalogPublisherStartupAndCancellation(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {})}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if err := controller.publishCatalogContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.find("catalog"); !ok {
		t.Fatal("startup did not publish real catalog")
	}
	state, ok := host.find("state")
	if !ok || state["state"] != "CatalogReady" {
		t.Fatal("nonempty startup lost CatalogReady")
	}
	before := len(host.messages)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.publishCatalogContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel not preserved: %v", err)
	}
	if len(host.messages) != before {
		t.Fatal("cancelled startup emitted events")
	}
	empty := &managedController{bridge: newManagedBridge(host, "empty-attempt", func() {})}
	if err := empty.publishCatalogContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty cancelled publication lost cancellation: %v", err)
	}
	if len(host.messages) != before {
		t.Fatal("empty cancelled startup emitted events")
	}
}
