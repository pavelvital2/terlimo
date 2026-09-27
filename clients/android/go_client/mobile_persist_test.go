package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

// Real-bridge persistence: BridgeReceiptStore writes through bridge.persist with the
// accountaccess namespace, waits for persist_result, never touches wlbs fields, and a
// failed/restarted host keeps the same-operation identity.

type persistHost struct {
	mu        sync.Mutex
	fail      bool
	blobs     map[string]string
	persisted []string
}

func (h *persistHost) run(t *testing.T, lines *bufio.Scanner, reply io.Writer) {
	t.Helper()
	for lines.Scan() {
		var message map[string]any
		if json.Unmarshal(lines.Bytes(), &message) != nil {
			continue
		}
		if message["type"] != "persist" {
			continue
		}
		namespace, _ := message["namespace"].(string)
		state, _ := message["state_b64"].(string)
		requestID, _ := message["request_id"].(string)
		h.mu.Lock()
		h.persisted = append(h.persisted, namespace)
		fail := h.fail
		if namespace != "" {
			h.blobs[namespace] = state
		}
		h.mu.Unlock()
		replyMessage := map[string]any{"v": 1, "attempt_id": "attempt",
			"type": "persist_result", "request_id": requestID}
		if fail {
			replyMessage["error"] = "PERSIST_FAILED"
		}
		raw, _ := json.Marshal(replyMessage)
		_, _ = reply.Write(append(raw, '\n'))
	}
}

func TestMobileBridgePersistNamespaceRoundTripAndWriteFailure(t *testing.T) {
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newManagedBridge(outWriter, "attempt", cancel)
	go bridge.read(ctx, bufio.NewScanner(inReader))
	host := &persistHost{blobs: map[string]string{}}
	go host.run(t, bufio.NewScanner(outReader), inWriter)

	store, err := accountaccess.NewBridgeReceiptStore(nil, func(ctx context.Context, payload []byte) error {
		return bridge.persistNamespace(ctx, accountaccess.ReceiptNamespace, payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	first := accountaccess.Receipt{
		Subject:    accountaccess.Subject{AccountRef: "acc-1", InstallationID: "inst-1"},
		Key:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BodyDigest: "one",
		Request:    accountaccess.AccessSyncRequest{CatalogRevision: "7", BindingRevision: "1"},
	}
	if err := store.Save(first); err != nil {
		t.Fatalf("persist through the real bridge failed: %v", err)
	}
	host.mu.Lock()
	captured := host.blobs[accountaccess.ReceiptNamespace]
	namespaces := append([]string(nil), host.persisted...)
	host.mu.Unlock()
	if captured == "" || len(namespaces) != 1 || namespaces[0] != accountaccess.ReceiptNamespace {
		t.Fatalf("namespace persistence wrong: %q %v", captured, namespaces)
	}
	if strings.Contains(captured, "subscription_ref") || strings.Contains(captured, "credential_id") {
		t.Fatal("accountaccess receipt must not carry wlbs business fields")
	}

	// Host write failure: the durable ack is missing, so memory must not advance.
	host.mu.Lock()
	host.fail = true
	host.mu.Unlock()
	second := first
	second.Key = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := store.Save(second); err == nil {
		t.Fatal("missing persist_result ack must surface an error")
	}
	loaded, err := store.Load()
	if err != nil || loaded == nil || loaded.Key != first.Key {
		t.Fatalf("failed write must not advance the receipt: %+v %v", loaded, err)
	}

	// Restart: the host blob decodes into the same operation identity, not wlbs state.
	raw, err := base64.RawURLEncoding.DecodeString(captured)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := accountaccess.NewBridgeReceiptStore(raw, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := restarted.Load()
	if err != nil || reloaded == nil || reloaded.Key != first.Key || reloaded.Request.CatalogRevision != "7" {
		t.Fatalf("restart reload wrong: %+v %v", reloaded, err)
	}
}

// TestMobileServiceSeedPersistsThroughBridgeNamespace proves the service seed store
// uses the existing persist/AtomicFile namespace path end to end: cached + user state
// is acknowledged by the host under service_seed_v1, never under the accountaccess
// receipt namespace, and a fresh store reloads it after restart.
func TestMobileServiceSeedPersistsThroughBridgeNamespace(t *testing.T) {
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newManagedBridge(outWriter, "attempt", cancel)
	go bridge.read(ctx, bufio.NewScanner(inReader))
	host := &persistHost{blobs: map[string]string{}}
	go host.run(t, bufio.NewScanner(outReader), inWriter)

	persist := func(ctx context.Context, payload []byte) error {
		return bridge.persistNamespace(ctx, servicechannel.StateNamespace, payload)
	}
	store := servicechannel.NewStore(servicechannel.Config{Environment: "test", Persist: persist})
	builtin := servicechannel.Seed{
		Version:           1,
		Revision:          "1",
		Environment:       "test",
		PeerIP:            "192.0.2.7",
		DTLSPort:          56000,
		DTLSSPKISHA256:    base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		ServiceClassifier: "bridge-namespace-classifier",
		VKHashes:          []string{"builtin-hash"},
	}
	raw, err := json.Marshal(builtin)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(servicechannel.SourceBuiltin, raw); err != nil {
		t.Fatal(err)
	}
	cached := builtin
	cached.Revision = "2"
	cached.PeerIP = "198.51.100.8"
	cached.VKHashes = []string{"cached-hash"}
	raw, err = json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(servicechannel.SourceCached, raw); err != nil {
		t.Fatal(err)
	}
	userSeed := cached
	userSeed.VKHashes = []string{"user-hash"}
	raw, err = json.Marshal(userSeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(servicechannel.SourceUser, raw); err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	captured := host.blobs[servicechannel.StateNamespace]
	namespaces := append([]string(nil), host.persisted...)
	host.mu.Unlock()
	if captured == "" || len(namespaces) != 2 {
		t.Fatalf("service seed state was not persisted twice: %q %v", captured, namespaces)
	}
	for _, namespace := range namespaces {
		if namespace != servicechannel.StateNamespace {
			t.Fatalf("service seed state used a foreign namespace: %v", namespaces)
		}
	}

	blob, err := base64.RawURLEncoding.DecodeString(captured)
	if err != nil {
		t.Fatal(err)
	}
	restarted := servicechannel.NewStore(servicechannel.Config{Environment: "test"})
	if err := restarted.Update(servicechannel.SourceBuiltin, mustSeedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadState(blob); err != nil {
		t.Fatal(err)
	}
	effective, source, ok := restarted.Current()
	if !ok || source != servicechannel.SourceUser || effective.PeerIP != "198.51.100.8" || effective.VKHashes[0] != "user-hash" {
		t.Fatalf("bridge-persisted restart wrong: %+v %v %v", effective, source, ok)
	}
}

func mustSeedJSON(t *testing.T, seed servicechannel.Seed) []byte {
	t.Helper()
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
