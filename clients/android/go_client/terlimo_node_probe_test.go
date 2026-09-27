package main

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

func TestManagedProbeNodeRequiresExactFreshCachedNode(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerTwoNodes(now)
	node, err := managedProbeNode(cat, false, cat.SubscriptionRef, "second", now)
	if err != nil || node.NodeID != "second" {
		t.Fatal("exact second node not selected", err)
	}
	for _, tc := range []struct {
		catNil, pending bool
		id              string
	}{
		{pending: true, id: "second"}, {catNil: true, id: "second"}, {id: "missing"},
	} {
		candidate := cat
		if tc.catNil {
			candidate = nil
		}
		if _, err := managedProbeNode(candidate, tc.pending, cat.SubscriptionRef, tc.id, now); err == nil {
			t.Fatal("unsafe probe admitted", tc)
		}
	}
	if cat.Nodes[0].NodeID != "test" || cat.Nodes[1].NodeID != "second" {
		t.Fatal("catalog mutated")
	}
}

func TestManagedBridgeRoutesProbeAndCancellationWithoutSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := newManagedBridge(&strings.Builder{}, "attempt", cancel)
	raw := "{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"probe_node\",\"node_id\":\"second\"}\n" +
		"{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"cancel_probe\"}\n"
	b.read(ctx, bufio.NewScanner(strings.NewReader(raw)))
	select {
	case got := <-b.probe:
		if got.NodeID != "second" || got.HasProbeID {
			t.Fatal(got)
		}
	default:
		t.Fatal("probe missing")
	}
	select {
	case <-b.probeStop:
	default:
		t.Fatal("probe cancellation missing")
	}
	select {
	case <-b.selection:
		t.Fatal("probe became connect selection")
	default:
	}
}
