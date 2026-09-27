package servicechannel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseSeedValidAndStrict(t *testing.T) {
	raw := seedJSON(t, validSeed())
	seed, err := ParseSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if seed.PeerIP != "192.0.2.7" || seed.DTLSPort != 56000 || seed.StreamID != 0 || len(seed.VKHashes) != 1 {
		t.Fatalf("seed changed: %+v", seed)
	}
	if seed.ServiceClassifier != "synthetic-public-classifier" || seed.Revision != "1" || seed.Environment != "test" {
		t.Fatalf("classifier/revision/environment changed: %+v", seed)
	}

	cases := []struct {
		name string
		mut  func(seed Seed) []byte
	}{
		{"missing version", func(seed Seed) []byte { seed.Version = 0; raw, _ := json.Marshal(seed); return raw }},
		{"future version", func(seed Seed) []byte { seed.Version = 2; raw, _ := json.Marshal(seed); return raw }},
		{"bad peer ip", func(seed Seed) []byte { seed.PeerIP = "not-an-ip"; raw, _ := json.Marshal(seed); return raw }},
		{"port zero", func(seed Seed) []byte { seed.DTLSPort = 0; raw, _ := json.Marshal(seed); return raw }},
		{"port high", func(seed Seed) []byte { seed.DTLSPort = 65536; raw, _ := json.Marshal(seed); return raw }},
		{"short pin", func(seed Seed) []byte { seed.DTLSSPKISHA256 = "AAAA"; raw, _ := json.Marshal(seed); return raw }},
		{"padded pin", func(seed Seed) []byte {
			seed.DTLSSPKISHA256 = validSeed().DTLSSPKISHA256 + "="
			raw, _ := json.Marshal(seed)
			return raw
		}},
		{"missing classifier", func(seed Seed) []byte { seed.ServiceClassifier = ""; raw, _ := json.Marshal(seed); return raw }},
		{"classifier with space", func(seed Seed) []byte {
			seed.ServiceClassifier = "public service"
			raw, _ := json.Marshal(seed)
			return raw
		}},
		{"classifier with control", func(seed Seed) []byte {
			seed.ServiceClassifier = "public\x01"
			raw, _ := json.Marshal(seed)
			return raw
		}},
		{"oversized classifier", func(seed Seed) []byte {
			seed.ServiceClassifier = strings.Repeat("a", seedMaxClassLen+1)
			raw, _ := json.Marshal(seed)
			return raw
		}},
		{"invalid revision", func(seed Seed) []byte { seed.Revision = "01"; raw, _ := json.Marshal(seed); return raw }},
		{"huge revision", func(seed Seed) []byte {
			seed.Revision = strings.Repeat("9", seedMaxRevLen+1)
			raw, _ := json.Marshal(seed)
			return raw
		}},
		{"foreign environment", func(seed Seed) []byte { seed.Environment = "staging"; raw, _ := json.Marshal(seed); return raw }},
		{"no hashes", func(seed Seed) []byte { seed.VKHashes = nil; raw, _ := json.Marshal(seed); return raw }},
		{"five hashes", func(seed Seed) []byte {
			seed.VKHashes = []string{"1", "2", "3", "4", "5"}
			raw, _ := json.Marshal(seed)
			return raw
		}},
		{"blank hash", func(seed Seed) []byte { seed.VKHashes = []string{""}; raw, _ := json.Marshal(seed); return raw }},
		{"hash with space", func(seed Seed) []byte { seed.VKHashes = []string{"a b"}; raw, _ := json.Marshal(seed); return raw }},
		{"negative stream", func(seed Seed) []byte { seed.StreamID = -1; raw, _ := json.Marshal(seed); return raw }},
		{"empty document", func(Seed) []byte { return nil }},
		{"oversized document", func(Seed) []byte { return bytes.Repeat([]byte{0x20}, seedMaxBytes+1) }},
		{"unknown secret field", func(seed Seed) []byte {
			return []byte(`{"version":1,"revision":"1","environment":"test","peer_ip":"192.0.2.7","dtls_port":56000,"dtls_spki_sha256":"` +
				validSeed().DTLSSPKISHA256 + `","service_classifier":"public","vk_hashes":["h"],"stream_id":0,"psk":"secret"}`)
		}},
		{"duplicate field", func(Seed) []byte {
			return []byte(`{"version":1,"version":1,"revision":"1","environment":"test","peer_ip":"192.0.2.7","dtls_port":56000,"dtls_spki_sha256":"` +
				validSeed().DTLSSPKISHA256 + `","service_classifier":"public","vk_hashes":["h"],"stream_id":0}`)
		}},
	}
	for _, testCase := range cases {
		if _, err := ParseSeed(testCase.mut(validSeed())); !errors.Is(err, ErrSeedInvalid) {
			t.Fatalf("%s: expected ErrSeedInvalid, got %v", testCase.name, err)
		}
	}
	if _, err := ParseSeed([]byte(`{"version":1,"revision":"1","environment":"test","peer_ip":"192.0.2.7","dtls_port":56000,"dtls_spki_sha256":"` +
		validSeed().DTLSSPKISHA256 + `","service_classifier":"public","vk_hashes":["h"],"stream_id":0} trailing`)); !errors.Is(err, ErrSeedInvalid) {
		t.Fatalf("trailing data accepted: %v", err)
	}
}

// TestStoreSlotsSeparateAndUserClearRestoresTrusted proves that builtin, cached and
// user are separate slots: a cached update moves the trusted endpoint, the user
// override replaces only the hash list, and clearing it restores the last-good
// cached hash set.
func TestStoreSlotsSeparateAndUserClearRestoresTrusted(t *testing.T) {
	store := NewStore(Config{Environment: "test"})
	if _, _, ok := store.Current(); ok {
		t.Fatal("empty store reported a seed")
	}
	builtin := validSeed()
	builtin.VKHashes = []string{"builtin-hash"}
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	trusted, source, ok := store.Current()
	if !ok || source != SourceBuiltin || trusted.VKHashes[0] != "builtin-hash" {
		t.Fatalf("builtin seed not installed: %+v %v", trusted, source)
	}

	// A cached update may replace the trusted endpoint atomically.
	cached := builtin
	cached.Revision = "2"
	cached.PeerIP = "198.51.100.8"
	cached.VKHashes = []string{"cached-hash"}
	if err := store.Update(SourceCached, seedJSON(t, cached)); err != nil {
		t.Fatal(err)
	}
	trusted, source, _ = store.Current()
	if source != SourceCached || trusted.PeerIP != "198.51.100.8" || trusted.VKHashes[0] != "cached-hash" {
		t.Fatalf("cached seed not applied: %+v %v", trusted, source)
	}
	if store.builtin == nil || store.cached == nil {
		t.Fatal("builtin/cached slots were not kept separate")
	}

	// A user update may replace the hash list only, bound to the trusted endpoint.
	userSeed := trusted
	userSeed.VKHashes = []string{"user-hash"}
	if err := store.Update(SourceUser, seedJSON(t, userSeed)); err != nil {
		t.Fatal(err)
	}
	effective, source, _ := store.Current()
	if source != SourceUser || effective.VKHashes[0] != "user-hash" {
		t.Fatalf("user hash update not applied: %+v %v", effective, source)
	}
	if effective.PeerIP != trusted.PeerIP || effective.DTLSSPKISHA256 != trusted.DTLSSPKISHA256 ||
		effective.ServiceClassifier != trusted.ServiceClassifier {
		t.Fatal("user hash update changed the trusted endpoint")
	}

	// Clearing the user override restores the last-good cached hash set.
	if err := store.ClearUser(); err != nil {
		t.Fatal(err)
	}
	restored, source, _ := store.Current()
	if source != SourceCached || restored.VKHashes[0] != "cached-hash" || restored.PeerIP != "198.51.100.8" {
		t.Fatalf("user clear did not restore the last-good trusted hashes: %+v %v", restored, source)
	}
	if err := store.ClearUser(); err != nil {
		t.Fatal("clearing without an override must be a no-op")
	}
}

// TestStoreUserOverrideBoundToTrustedEndpoint proves that a user override created for
// one trusted endpoint never follows a cached endpoint move; the trusted hash set is
// used instead until the override is re-bound.
func TestStoreUserOverrideBoundToTrustedEndpoint(t *testing.T) {
	store := NewStore(Config{Environment: "test"})
	first := validSeed()
	first.VKHashes = []string{"first-hash"}
	if err := store.Update(SourceBuiltin, seedJSON(t, first)); err != nil {
		t.Fatal(err)
	}
	override := first
	override.VKHashes = []string{"user-hash"}
	if err := store.Update(SourceUser, seedJSON(t, override)); err != nil {
		t.Fatal(err)
	}
	if effective, source, _ := store.Current(); source != SourceUser || effective.VKHashes[0] != "user-hash" {
		t.Fatalf("user override not active: %+v %v", effective, source)
	}

	moved := first
	moved.Revision = "2"
	moved.PeerIP = "203.0.113.9"
	moved.VKHashes = []string{"moved-hash"}
	if err := store.Update(SourceCached, seedJSON(t, moved)); err != nil {
		t.Fatal(err)
	}
	effective, source, _ := store.Current()
	if source != SourceCached || effective.PeerIP != "203.0.113.9" || effective.VKHashes[0] != "moved-hash" {
		t.Fatalf("stale user override leaked onto the moved endpoint: %+v %v", effective, source)
	}

	// The override stays stored and becomes active again when re-bound to the new node.
	if err := store.Update(SourceUser, seedJSON(t, moved)); err != nil {
		t.Fatal(err)
	}
	if effective, source, _ = store.Current(); source != SourceUser || effective.VKHashes[0] != "moved-hash" {
		t.Fatalf("re-bound user override not applied: %+v %v", effective, source)
	}
}

// TestStoreRevisionOrderingRejectsStaleReplay proves out-of-order and replayed
// trusted updates are rejected while the last-good seed survives.
func TestStoreRevisionOrderingRejectsStaleReplay(t *testing.T) {
	store := NewStore(Config{Environment: "test"})
	builtin := validSeed()
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	newer := builtin
	newer.Revision = "5"
	newer.VKHashes = []string{"newer-hash"}
	if err := store.Update(SourceCached, seedJSON(t, newer)); err != nil {
		t.Fatal(err)
	}
	lastGood, _, _ := store.Current()

	for name, stale := range map[string]Seed{
		"lower":  func() Seed { s := builtin; s.Revision = "4"; return s }(),
		"equal":  func() Seed { s := newer; return s }(),
		"no rev": func() Seed { s := builtin; s.Revision = ""; return s }(),
	} {
		if err := store.Update(SourceCached, seedJSON(t, stale)); !errors.Is(err, ErrSeedStale) {
			t.Fatalf("%s cached update not rejected as stale: %v", name, err)
		}
	}
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); !errors.Is(err, ErrSeedStale) {
		t.Fatalf("stale builtin update not rejected: %v", err)
	}
	after, source, _ := store.Current()
	if source != SourceCached || after.VKHashes[0] != "newer-hash" || after.Revision != "5" {
		t.Fatalf("stale update replaced the last-good seed: %+v %v", after, source)
	}
	if after.PeerIP != lastGood.PeerIP {
		t.Fatal("stale update moved the endpoint")
	}

	// A newer builtin supersedes the older cached slot and keeps the endpoint ordering.
	rebuild := builtin
	rebuild.Revision = "9"
	rebuild.PeerIP = "198.51.100.77"
	if err := store.Update(SourceBuiltin, seedJSON(t, rebuild)); err != nil {
		t.Fatal(err)
	}
	after, source, _ = store.Current()
	if source != SourceBuiltin || after.PeerIP != "198.51.100.77" || after.Revision != "9" {
		t.Fatalf("newer builtin not effective: %+v %v", after, source)
	}
}

func TestStoreEnvironmentBinding(t *testing.T) {
	store := NewStore(Config{Environment: "test"})
	builtin := validSeed()
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	foreign := builtin
	foreign.Revision = "2"
	foreign.Environment = "production"
	if err := store.Update(SourceCached, seedJSON(t, foreign)); !errors.Is(err, ErrSeedBinding) {
		t.Fatalf("foreign-environment cached seed accepted: %v", err)
	}
	foreignUser := builtin
	foreignUser.Environment = "production"
	if err := store.Update(SourceUser, seedJSON(t, foreignUser)); !errors.Is(err, ErrSeedBinding) {
		t.Fatalf("foreign-environment user seed accepted: %v", err)
	}
	// An unbound environment keeps the last-good seed untouched too.
	if _, source, _ := store.Current(); source != SourceBuiltin {
		t.Fatal("binding failure replaced the last-good seed")
	}
}

func TestStoreInvalidUpdateKeepsLastGood(t *testing.T) {
	store := NewStore(Config{Environment: "test"})
	builtin := validSeed()
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"version":99}`),
		[]byte(`not json`),
		[]byte(`{"version":1,"revision":"2","environment":"test","peer_ip":"192.0.2.7","dtls_port":56000,"dtls_spki_sha256":"` +
			validSeed().DTLSSPKISHA256 + `","service_classifier":"public","vk_hashes":["h"],"stream_id":0,"psk":"secret"}`),
	} {
		if err := store.Update(SourceCached, invalid); err == nil {
			t.Fatalf("invalid cached update accepted: %s", invalid)
		}
	}
	after, source, _ := store.Current()
	if source != SourceBuiltin || after.PeerIP != builtin.PeerIP {
		t.Fatalf("invalid update replaced the last-good seed: %+v %v", after, source)
	}
	if err := store.Update(Source("attacker"), seedJSON(t, builtin)); !errors.Is(err, ErrSeedSource) {
		t.Fatalf("unknown source accepted: %v", err)
	}
}

// TestStorePersistFailureKeepsLastGood proves the durable write is the commit point:
// a failed host persist never advances the in-memory slots.
func TestStorePersistFailureKeepsLastGood(t *testing.T) {
	persistErr := errors.New("PERSIST_FAILED")
	store := NewStore(Config{Environment: "test", Persist: func(context.Context, []byte) error { return persistErr }})
	builtin := validSeed()
	builtin.VKHashes = []string{"builtin-hash"}
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	cached := builtin
	cached.Revision = "2"
	cached.VKHashes = []string{"cached-hash"}
	if err := store.Update(SourceCached, seedJSON(t, cached)); !errors.Is(err, persistErr) {
		t.Fatalf("failed persist accepted: %v", err)
	}
	after, source, _ := store.Current()
	if source != SourceBuiltin || after.VKHashes[0] != "builtin-hash" {
		t.Fatalf("failed persist advanced the slots: %+v %v", after, source)
	}
}

// TestStoreDurableRestart proves cached + user survive a restart through the fake
// persist loader, and that LoadState refuses foreign or malformed blobs.
func TestStoreDurableRestart(t *testing.T) {
	var persisted []byte
	store := NewStore(Config{Environment: "test", Persist: func(_ context.Context, payload []byte) error {
		persisted = append([]byte(nil), payload...)
		return nil
	}})
	builtin := validSeed()
	builtin.VKHashes = []string{"builtin-hash"}
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	cached := builtin
	cached.Revision = "2"
	cached.PeerIP = "198.51.100.9"
	cached.VKHashes = []string{"cached-hash"}
	if err := store.Update(SourceCached, seedJSON(t, cached)); err != nil {
		t.Fatal(err)
	}
	userSeed := cached
	userSeed.VKHashes = []string{"user-hash"}
	if err := store.Update(SourceUser, seedJSON(t, userSeed)); err != nil {
		t.Fatal(err)
	}
	if len(persisted) == 0 {
		t.Fatal("durable update was not persisted")
	}

	restarted := NewStore(Config{Environment: "test"})
	if err := restarted.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadState(persisted); err != nil {
		t.Fatal(err)
	}
	effective, source, _ := restarted.Current()
	if source != SourceUser || effective.PeerIP != "198.51.100.9" || effective.VKHashes[0] != "user-hash" {
		t.Fatalf("durable restart lost the cached/user slots: %+v %v", effective, source)
	}
	if err := restarted.ClearUser(); err != nil {
		t.Fatal(err)
	}
	if effective, source, _ = restarted.Current(); source != SourceCached || effective.VKHashes[0] != "cached-hash" {
		t.Fatalf("durable user clear did not restore the cached hashes: %+v %v", effective, source)
	}

	// Foreign namespace, foreign environment and malformed blobs are rejected
	// without touching the last-good state.
	var envelope map[string]any
	if err := json.Unmarshal(persisted, &envelope); err != nil {
		t.Fatal(err)
	}
	foreign := map[string]any{}
	for key, value := range envelope {
		foreign[key] = value
	}
	foreign["namespace"] = "wlbs_state"
	foreignRaw, _ := json.Marshal(foreign)
	if err := restarted.LoadState(foreignRaw); !errors.Is(err, ErrSeedInvalid) {
		t.Fatalf("foreign namespace accepted: %v", err)
	}
	foreignEnv := map[string]any{}
	for key, value := range envelope {
		foreignEnv[key] = value
	}
	foreignEnv["environment"] = "production"
	foreignEnvRaw, _ := json.Marshal(foreignEnv)
	if err := restarted.LoadState(foreignEnvRaw); !errors.Is(err, ErrSeedBinding) {
		t.Fatalf("foreign environment state accepted: %v", err)
	}
	foreignCached := map[string]any{}
	for key, value := range envelope {
		foreignCached[key] = value
	}
	cachedEnvelope, _ := foreignCached["cached"].(map[string]any)
	cachedCopy := map[string]any{}
	for key, value := range cachedEnvelope {
		cachedCopy[key] = value
	}
	cachedCopy["environment"] = "production"
	foreignCached["cached"] = cachedCopy
	foreignCachedRaw, _ := json.Marshal(foreignCached)
	if err := restarted.LoadState(foreignCachedRaw); !errors.Is(err, ErrSeedBinding) {
		t.Fatalf("foreign cached environment accepted: %v", err)
	}
	if err := restarted.LoadState([]byte(`not json`)); !errors.Is(err, ErrSeedInvalid) {
		t.Fatalf("malformed state accepted: %v", err)
	}
	if effective, source, _ := restarted.Current(); source != SourceCached || effective.VKHashes[0] != "cached-hash" {
		t.Fatalf("rejected state advanced the slots: %+v %v", effective, source)
	}
}

// TestStoreStaleDurableCacheIgnored proves a durable cache older than the installed
// builtin revision is ignored (not an error): the new build wins.
func TestStoreStaleDurableCacheIgnored(t *testing.T) {
	var persisted []byte
	writer := NewStore(Config{Environment: "test", Persist: func(_ context.Context, payload []byte) error {
		persisted = append([]byte(nil), payload...)
		return nil
	}})
	old := validSeed()
	old.VKHashes = []string{"old-builtin-hash"}
	if err := writer.Update(SourceBuiltin, seedJSON(t, old)); err != nil {
		t.Fatal(err)
	}
	oldCached := old
	oldCached.Revision = "2"
	oldCached.PeerIP = "198.51.100.10"
	oldCached.VKHashes = []string{"old-cached-hash"}
	if err := writer.Update(SourceCached, seedJSON(t, oldCached)); err != nil {
		t.Fatal(err)
	}

	newBuild := validSeed()
	newBuild.Revision = "7"
	newBuild.PeerIP = "203.0.113.7"
	newBuild.VKHashes = []string{"new-builtin-hash"}
	restarted := NewStore(Config{Environment: "test"})
	if err := restarted.Update(SourceBuiltin, seedJSON(t, newBuild)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadState(persisted); err != nil {
		t.Fatal(err)
	}
	effective, source, _ := restarted.Current()
	if source != SourceBuiltin || effective.PeerIP != "203.0.113.7" || effective.VKHashes[0] != "new-builtin-hash" {
		t.Fatalf("stale durable cache overrode the new builtin: %+v %v", effective, source)
	}
}

func TestStoreUserSeedWithoutTrustedEndpoint(t *testing.T) {
	store := NewStore(Config{Environment: "test"})
	if err := store.Update(SourceUser, seedJSON(t, validSeed())); !errors.Is(err, ErrSeedEndpoint) {
		t.Fatalf("user-only seed accepted: %v", err)
	}
	if err := store.LoadState(nil); err != nil {
		t.Fatalf("empty state rejected: %v", err)
	}
	if _, _, ok := store.Current(); ok {
		t.Fatal("user-only seed installed")
	}
}

func TestSeedStringDoesNotLeakPath(t *testing.T) {
	seed, err := ParseSeed(seedJSON(t, validSeed()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seed.PeerIP, "/") || len(seed.VKHashes) == 0 {
		t.Fatalf("unexpected seed: %+v", seed)
	}
}
