package main

// Focused tests for the owner-contract display branch and the gateway_key binding in
// the host bridge: the explicit actions carry the optional gateway_key, the browse
// catalog reaches the host as a metadata-only `catalog` event with catalog_mode=browse
// and never touches the credential store, and the distinct intent conflict surfaces as
// the fixed ONBOARDING_INTENT_CONFLICT token.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wg-turn-client/onboarding"
)

func TestBridgeExplicitActionsCarryGatewayKey(t *testing.T) {
	b := newManagedBridge(io.Discard, "attempt", func() {})
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"attempt","type":"explicit_connect","gateway_key":"gw-b"}`+"\n")))
	select {
	case key := <-b.explicit:
		if key != "gw-b" {
			t.Fatalf("explicit_connect gateway_key = %q", key)
		}
	default:
		t.Fatal("explicit_connect must reach the pre-admission channel")
	}
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"attempt","type":"select_node","node_id":"gw-c","gateway_key":"gw-c","explicit_connect":true}`+"\n")))
	select {
	case id := <-b.selection:
		if id != "gw-c" || b.selectionGatewayKey() != "gw-c" || !b.explicitSelection() {
			t.Fatalf("selection pairing wrong: id=%q key=%q explicit=%v", id, b.selectionGatewayKey(), b.explicitSelection())
		}
	default:
		t.Fatal("select_node must reach the selection channel")
	}
	// A legacy select without gateway_key keeps the empty pairing: the caller falls
	// back to the selected id.
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"attempt","type":"select_node","node_id":"gw-d","explicit_connect":true}`+"\n")))
	<-b.selection
	if b.selectionGatewayKey() != "" {
		t.Fatalf("absent gateway_key must stay empty: %q", b.selectionGatewayKey())
	}
}

func feedBrowseBody(count int) []byte {
	nodes := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		nodes = append(nodes, map[string]any{
			"gateway_id": fmt.Sprintf("gw-synth-%d", index), "name": fmt.Sprintf("Synthetic %d", index),
			"region": "test", "country_code": "XX"})
	}
	raw, err := json.Marshal(map[string]any{
		"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
		"schema_version": "1.0", "status": "ok", "catalog_mode": "browse",
		"valid_until": "2026-09-21T12:10:00Z", "issued_at": "2026-09-21T11:55:00Z",
		"gateways": nodes})
	if err != nil {
		panic(err)
	}
	return raw
}

type browseFeedBackend struct {
	fixture *mobileFeedFixture
	body    []byte
}

func (b *browseFeedBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mobile/v1/gateways", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(b.body)
	})
	mux.Handle("/", b.fixture.handler())
	return mux
}

// TestMobileBrowseCatalogPublishedWithoutCredentials proves the fresh-install open
// path: a session with no data right receives the display list over the existing
// `catalog` event with catalog_mode=browse and metadata-only nodes, while the
// credential store stays empty and no revision/selection/subscription is announced.
func TestMobileBrowseCatalogPublishedWithoutCredentials(t *testing.T) {
	fixture := newMobileFeedFixture(t, 0, mobileFeedOptions{dataAccess: "none"})
	backend := &browseFeedBackend{fixture: fixture, body: feedBrowseBody(3)}
	server := httptest.NewServer(backend.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server)
	controller, _, host, cancel, errCh := newMobileFeedRun(t, fixture, start)

	message := host.waitMessage(t, "catalog")
	if message["catalog_mode"] != "browse" {
		t.Fatalf("browse event must be explicit: %v", message)
	}
	nodes, ok := message["nodes"].([]any)
	if !ok || len(nodes) != 3 {
		t.Fatalf("browse nodes wrong: %v", message["nodes"])
	}
	for index, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("browse node shape wrong: %v", raw)
		}
		if node["node_id"] != fmt.Sprintf("gw-synth-%d", index) || node["name"] != fmt.Sprintf("Synthetic %d", index) ||
			node["country_code"] != "XX" || node["region"] != "test" {
			t.Fatalf("browse node metadata wrong: %v", node)
		}
		for _, forbidden := range []string{"access", "transport", "probe", "grant_id", "password"} {
			if _, present := node[forbidden]; present {
				t.Fatalf("browse node leaked %q: %v", forbidden, node)
			}
		}
	}
	for _, forbidden := range []string{"revision", "selected_node_id", "subscription_status", "subscription_expires_at", "slots_used", "slots_limit"} {
		if _, present := message[forbidden]; present {
			t.Fatalf("browse event leaked %q: %v", forbidden, message)
		}
	}
	if controller.store.Snapshot() != nil {
		t.Fatal("browse must never populate the credential store")
	}
	if controller.mobile == nil {
		t.Fatal("mobile feed missing")
	}
	controller.mobile.mu.Lock()
	retained := controller.mobile.browse
	controller.mobile.mu.Unlock()
	if retained == nil || len(retained.Gateways) != 3 {
		t.Fatalf("the display catalog must be retained in memory for the host session: %+v", retained)
	}
	cancel()
	<-errCh
}

func TestExplicitConnectIntentConflictEmitsFixedToken(t *testing.T) {
	// Local pre-emption: an unfinished intent paired with gw-a, a different explicit
	// choice arrives, and no network call is made.
	store := &onboarding.MemoryStore{}
	if err := store.Save(onboarding.KeyState{RequestKey: "rk-1", IntentID: "intent-1", GatewayKey: "gw-a"}); err != nil {
		t.Fatal(err)
	}
	called := false
	intents := &diagIntentClient{step: func(int, string) (onboarding.IntentPoll, *onboarding.APIError, error) {
		called = true
		return onboarding.IntentPoll{}, nil, nil
	}}
	flow, err := onboarding.NewFlow(store, intents, nil, onboarding.Options{})
	if err != nil {
		t.Fatal(err)
	}
	diag := captureOnboardingDiagnostics(t)
	_, conflict := (&managedMobile{explicit: flow}).explicitConnect(context.Background(), "gw-b")
	if !errors.Is(conflict, onboarding.ErrIntentConflict) || called {
		t.Fatalf("local conflict must pre-empt the network: %v called=%v", conflict, called)
	}
	want := []string{"onboarding: ENTRY", "onboarding: ONBOARDING_INTENT_CONFLICT"}
	if got := diagLines(diag); !equalLines(got, want) {
		t.Fatalf("local conflict diagnostics = %q, want %q", got, want)
	}

	// Server 409: the same fixed token, with the API error identity preserved.
	serverFlow, err := onboarding.NewFlow(&onboarding.MemoryStore{}, &diagIntentClient{
		step: func(int, string) (onboarding.IntentPoll, *onboarding.APIError, error) {
			return onboarding.IntentPoll{}, &onboarding.APIError{HTTPStatus: 409, Code: "ONBOARDING_INTENT_CONFLICT"}, nil
		}}, nil, onboarding.Options{})
	if err != nil {
		t.Fatal(err)
	}
	diag = captureOnboardingDiagnostics(t)
	_, serverConflict := (&managedMobile{explicit: serverFlow}).explicitConnect(context.Background(), "gw-b")
	var apiError *onboarding.APIError
	if !errors.As(serverConflict, &apiError) || apiError.Code != "ONBOARDING_INTENT_CONFLICT" {
		t.Fatalf("server conflict identity changed: %v", serverConflict)
	}
	if got := diagLines(diag); !equalLines(got, want) {
		t.Fatalf("server conflict diagnostics = %q, want %q", got, want)
	}
}
