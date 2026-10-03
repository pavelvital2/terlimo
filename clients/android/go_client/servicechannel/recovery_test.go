package servicechannel

// Shared producer fixture uses a fixed TEST-only Ed25519 key. These source tests
// perform actual TR1 verification, Store admission, persistence and restart; they
// do not authenticate a live service or claim the external runtime writer fence.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const recoveryFixtureSHA = "252c43104f5debad67a63489111dff6dc55801e5e2fc7d253d7c09e4009f1d1f"

type recoveryFixture struct {
	TestOnly               bool   `json:"test_only"`
	PublicKeyB64           string `json:"public_key_b64"`
	ExpectedSeed           Seed   `json:"expected_seed"`
	PayloadUTF8            string `json:"payload_utf8"`
	Code                   string `json:"code"`
	WhitespacePayloadCode  string `json:"whitespace_payload_code"`
	DuplicateKeyCode       string `json:"duplicate_key_code"`
	UnknownKeyCode         string `json:"unknown_key_code"`
	MissingStreamCode      string `json:"missing_stream_code"`
	NullStreamCode         string `json:"null_stream_code"`
	FutureVersionCode      string `json:"future_version_code"`
	ForeignEnvironmentCode string `json:"foreign_environment_code"`
	ForeignSignatureCode   string `json:"foreign_signature_code"`
}

func loadRecoveryFixture(t *testing.T) (recoveryFixture, ed25519.PublicKey) {
	t.Helper()
	raw, err := os.ReadFile("testdata/recovery-v1/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != recoveryFixtureSHA {
		t.Fatal("shared recovery producer fixture changed")
	}
	var fixture recoveryFixture
	if err := json.Unmarshal(raw, &fixture); err != nil || !fixture.TestOnly {
		t.Fatalf("fixture: %v", err)
	}
	key, err := DecodeRecoveryVerifyKey(fixture.PublicKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, key
}

func recoveryTestCode(raw []byte) string {
	seed := sha256.Sum256([]byte("TERLIMO RECOVERY V1 TEST ONLY"))
	key := ed25519.NewKeyFromSeed(seed[:])
	signature := ed25519.Sign(key, append([]byte(recoveryDomain), raw...))
	return "TR1." + base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func verifyRecoveryTestSeed(t *testing.T, seed Seed, key ed25519.PublicKey) Seed {
	t.Helper()
	verified, err := VerifyRecoveryCode(recoveryTestCode(seedJSON(t, seed)), key, seed.Environment)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func TestRecoverySharedProducerExactPayload(t *testing.T) {
	fixture, key := loadRecoveryFixture(t)
	if recoveryTestCode([]byte(fixture.PayloadUTF8)) != fixture.Code {
		t.Fatal("Python→Go exact signature mismatch")
	}
	for _, code := range []string{fixture.Code, " \t\n" + fixture.Code + "\r\n ", fixture.WhitespacePayloadCode} {
		seed, err := VerifyRecoveryCode(code, key, "test")
		if err != nil || recoverySeedDigest(seed) != recoverySeedDigest(fixture.ExpectedSeed) {
			t.Fatalf("verified fixture changed: %+v %v", seed, err)
		}
		raw := seedJSON(t, seed)
		if bytes.Contains(raw, []byte("recoveryProof")) {
			t.Fatal("private admission metadata leaked into seed wire")
		}
	}
	// The same JSON fields with different raw bytes need their own signature.
	parts := strings.Split(fixture.WhitespacePayloadCode, ".")
	parts[1] = strings.Split(fixture.Code, ".")[1]
	if _, err := VerifyRecoveryCode(strings.Join(parts, "."), key, "test"); !errors.Is(err, ErrRecoverySignature) {
		t.Fatalf("payload was reserialized before verify: %v", err)
	}
}

func TestRecoveryParserCorruptForeignAndBounds(t *testing.T) {
	fixture, key := loadRecoveryFixture(t)
	parts := strings.Split(fixture.Code, ".")
	cases := []struct {
		name, code string
		want       error
	}{
		{"duplicate key", fixture.DuplicateKeyCode, ErrRecoveryInvalid},
		{"unknown key", fixture.UnknownKeyCode, ErrRecoveryInvalid},
		{"case alias extra key", recoveryTestCode([]byte(strings.TrimSuffix(fixture.PayloadUTF8, "}") + `,"Version":1}`)), ErrRecoveryInvalid},
		{"missing zero field", fixture.MissingStreamCode, ErrRecoveryInvalid},
		{"null zero field", fixture.NullStreamCode, ErrRecoveryInvalid},
		{"future seed version", fixture.FutureVersionCode, ErrRecoveryInvalid},
		{"foreign environment", fixture.ForeignEnvironmentCode, ErrRecoveryEnvironment},
		{"foreign signer", fixture.ForeignSignatureCode, ErrRecoverySignature},
		{"empty", "", ErrRecoveryInvalid},
		{"truncated", strings.Join(parts[:2], "."), ErrRecoveryInvalid},
		{"extra segment", fixture.Code + ".x", ErrRecoveryInvalid},
		{"wrong code version", "TR2." + parts[1] + "." + parts[2], ErrRecoveryInvalid},
		{"internal whitespace", "TR1. " + parts[1] + "." + parts[2], ErrRecoveryInvalid},
		{"internal unicode whitespace", "TR1.\u200b" + parts[1] + "." + parts[2], ErrRecoveryInvalid},
		{"padded payload", "TR1." + parts[1] + "=." + parts[2], ErrRecoveryInvalid},
		{"padded signature", fixture.Code + "=", ErrRecoveryInvalid},
		{"oversized", strings.Repeat("x", recoveryCodeMax+1), ErrRecoveryInvalid},
		{"invalid utf8 payload", recoveryTestCode([]byte{'{', '"', 0xff, '"', ':', '0', '}'}), ErrRecoveryInvalid},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := VerifyRecoveryCode(test.code, key, "test"); !errors.Is(err, test.want) {
				t.Fatalf("want %v, got %v", test.want, err)
			}
		})
	}
	if _, err := VerifyRecoveryCode(fixture.Code, nil, "test"); !errors.Is(err, ErrRecoveryUnavailable) {
		t.Fatalf("missing key accepted: %v", err)
	}
	if _, err := VerifyRecoveryCode(fixture.Code, key, "staging"); !errors.Is(err, ErrRecoveryEnvironment) {
		t.Fatalf("unknown trusted env accepted: %v", err)
	}
	for _, encoded := range []string{"", fixture.PublicKeyB64 + "=", " " + fixture.PublicKeyB64, strings.Repeat("A", 42)} {
		if _, err := DecodeRecoveryVerifyKey(encoded); !errors.Is(err, ErrRecoveryUnavailable) {
			t.Fatalf("bad trusted key accepted: %v", err)
		}
	}
	boundary := fixture.ExpectedSeed
	boundary.VKHashes = []string{strings.Repeat("a", seedMaxHashLen), "b", "c", "d"}
	verifyRecoveryTestSeed(t, boundary, key)
	boundary.VKHashes = append(boundary.VKHashes, "e")
	if _, err := VerifyRecoveryCode(recoveryTestCode(seedJSON(t, boundary)), key, "test"); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("hash count exceeded: %v", err)
	}
	boundary.VKHashes = []string{strings.Repeat("a", seedMaxHashLen+1)}
	if _, err := VerifyRecoveryCode(recoveryTestCode(seedJSON(t, boundary)), key, "test"); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("hash length exceeded: %v", err)
	}
}

func TestRecoveryStageCommitPersistRestart(t *testing.T) {
	fixture, key := loadRecoveryFixture(t)
	candidate, err := VerifyRecoveryCode(fixture.Code, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	type ownerContextKey struct{}
	ctx := context.WithValue(context.Background(), ownerContextKey{}, "current-attempt")
	var durable []byte
	persists := 0
	store := NewStore(Config{Environment: "test", Persist: func(writeCtx context.Context, payload []byte) error {
		persists++
		// Update(SourceUser) uses its existing background context. Recovery must
		// pass the caller context into the externally guarded persistence seam.
		if persists > 1 && writeCtx.Value(ownerContextKey{}) != "current-attempt" {
			t.Fatal("recovery dropped current-owner context")
		}
		durable = append([]byte(nil), payload...)
		return nil
	}})
	builtin := validSeed()
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	user := builtin
	user.VKHashes = []string{"old-user-override"}
	if err := store.Update(SourceUser, seedJSON(t, user)); err != nil {
		t.Fatal(err)
	}
	oldDurable := append([]byte(nil), durable...)
	noop, err := store.ValidateRecovery(candidate)
	if err != nil || noop {
		t.Fatalf("admission: %v %v", noop, err)
	}
	stage, err := store.StageRecovery(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if staged, source, ok := stage.Current(); !ok || source != SourceBuiltin || recoverySeedDigest(staged) != recoverySeedDigest(candidate) {
		t.Fatalf("candidate endpoint not staged: %+v %v", staged, source)
	}
	if current, source, _ := store.Current(); source != SourceUser || current.PeerIP != builtin.PeerIP || current.VKHashes[0] != "old-user-override" {
		t.Fatalf("staging changed last-good: %+v %v", current, source)
	}
	if persists != 1 || !bytes.Equal(durable, oldDurable) {
		t.Fatal("stage/validation wrote durable state")
	}
	if err := stage.CommitRecovery(ctx, candidate); !errors.Is(err, ErrRecoveryPersist) && err != nil {
		t.Fatalf("unexpected staged commit: %v", err)
	}
	// Discarding this stage (candidate network failure/cancel) never promotes it.
	if !bytes.Equal(durable, oldDurable) {
		t.Fatal("discarded stage changed last-good")
	}
	if err := store.CommitRecovery(ctx, candidate); err != nil {
		t.Fatal(err)
	}
	if persists != 2 {
		t.Fatalf("wanted one atomic recovery save, got %d", persists-1)
	}
	var envelope stateEnvelope
	if err := json.Unmarshal(durable, &envelope); err != nil || envelope.Namespace != StateNamespace || envelope.User != nil || envelope.Cached == nil || recoverySeedDigest(*envelope.Cached) != recoverySeedDigest(candidate) {
		t.Fatalf("cached+clear user not atomic: %s %v", durable, err)
	}
	current, source, ok := store.Current()
	if !ok || source != SourceCached || recoverySeedDigest(current) != recoverySeedDigest(candidate) {
		t.Fatalf("accepted endpoint/hash changed: %+v %v", current, source)
	}
	restarted := NewStore(Config{Environment: "test"})
	if err := restarted.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadState(durable); err != nil {
		t.Fatal(err)
	}
	if current, source, _ := restarted.Current(); source != SourceCached || recoverySeedDigest(current) != recoverySeedDigest(candidate) {
		t.Fatalf("restart lost endpoint/hashes: %+v %v", current, source)
	}
	if noop, err := restarted.ValidateRecovery(candidate); err != nil || !noop {
		t.Fatalf("equal identical after restart: %v %v", noop, err)
	}
	beforeRestartState, _ := restarted.State()
	for _, invalidBlob := range [][]byte{
		[]byte(`{"namespace":"service_seed_v1","namespace":"service_seed_v1"}`),
		[]byte(`{"namespace":"other","version":1}`),
		[]byte(`{"namespace":"service_seed_v1","version":1,"environment":"production"}`),
	} {
		if err := restarted.LoadState(invalidBlob); err == nil {
			t.Fatal("corrupt/foreign durable state accepted")
		}
		after, _ := restarted.State()
		if !bytes.Equal(beforeRestartState, after) {
			t.Fatal("corrupt/foreign restart blob modified trusted seed")
		}
	}
}

func TestRecoveryFailedSaveCancelledAndOwnerWriteRetainOld(t *testing.T) {
	fixture, key := loadRecoveryFixture(t)
	candidate, err := VerifyRecoveryCode(fixture.Code, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	saveFailure := errors.New("synthetic atomic-save failure")
	var durable []byte
	var saveErr error
	persists := 0
	store := NewStore(Config{Environment: "test", Persist: func(ctx context.Context, payload []byte) error {
		persists++
		if saveErr != nil {
			return saveErr
		}
		durable = append([]byte(nil), payload...)
		return nil
	}})
	builtin := validSeed()
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	user := builtin
	user.VKHashes = []string{"last-good-user-hash"}
	if err := store.Update(SourceUser, seedJSON(t, user)); err != nil {
		t.Fatal(err)
	}
	oldDurable := append([]byte(nil), durable...)
	oldState, _ := store.State()
	assertOld := func() {
		t.Helper()
		state, _ := store.State()
		if !bytes.Equal(state, oldState) || !bytes.Equal(durable, oldDurable) {
			t.Fatal("failed/cancelled operation overwrote last-good")
		}
	}
	saveErr = saveFailure
	if err := store.CommitRecovery(context.Background(), candidate); !errors.Is(err, ErrRecoveryPersist) || !errors.Is(err, saveFailure) {
		t.Fatalf("save failure misclassified: %v", err)
	}
	assertOld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := persists
	if err := store.CommitRecovery(ctx, candidate); !errors.Is(err, ErrRecoveryCancelled) {
		t.Fatalf("cancelled commit: %v", err)
	}
	if persists != before {
		t.Fatal("cancelled before write reached writer")
	}
	assertOld()
	// External writer rejection is distinct from Store/context locking. A current
	// attempt guard must reject ownership lost before its actual durable write.
	saveErr = ErrRecoveryCancelled
	if err := store.CommitRecovery(context.Background(), candidate); !errors.Is(err, ErrRecoveryCancelled) {
		t.Fatalf("external owner rejection: %v", err)
	}
	assertOld()
	saveErr = nil
	if err := store.CommitRecovery(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(durable, oldDurable) {
		t.Fatal("successful retry did not save candidate")
	}
}

func TestRecoveryTrustedAdmissionMonotonicAndNoop(t *testing.T) {
	fixture, key := loadRecoveryFixture(t)
	store := NewStore(Config{Environment: "test"})
	builtin := validSeed()
	if err := store.Update(SourceBuiltin, seedJSON(t, builtin)); err != nil {
		t.Fatal(err)
	}
	same := verifyRecoveryTestSeed(t, builtin, key)
	if noop, err := store.ValidateRecovery(same); err != nil || !noop {
		t.Fatalf("equal identical: %v %v", noop, err)
	}
	if err := store.CommitRecovery(context.Background(), same); err != nil {
		t.Fatalf("no-op required writer: %v", err)
	}
	modified := cloneSeed(same)
	modified.PeerIP = "192.0.2.200"
	if _, err := store.ValidateRecovery(modified); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("changed verified fields accepted: %v", err)
	}
	unsigned, err := ParseSeed(seedJSON(t, fixture.ExpectedSeed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateRecovery(unsigned); !errors.Is(err, ErrRecoveryInvalid) {
		t.Fatalf("unsigned parsed seed became recovery authority: %v", err)
	}
	for _, revision := range []string{"0", "1"} {
		different := fixture.ExpectedSeed
		different.Revision = revision
		verified := verifyRecoveryTestSeed(t, different, key)
		if _, err := store.ValidateRecovery(verified); !errors.Is(err, ErrRecoveryStale) || !errors.Is(err, ErrSeedStale) {
			t.Fatalf("stale/conflicting seed accepted: %v", err)
		}
		if err := store.CommitRecovery(context.Background(), verified); !errors.Is(err, ErrRecoveryStale) {
			t.Fatalf("stale/conflicting commit accepted: %v", err)
		}
	}
	foreignStore := NewStore(Config{Environment: "production"})
	if _, err := foreignStore.ValidateRecovery(same); !errors.Is(err, ErrRecoveryEnvironment) {
		t.Fatalf("store environment bypass: %v", err)
	}
	// Signature admission does not relax the existing SourceUser endpoint guard.
	if err := store.Update(SourceUser, seedJSON(t, fixture.ExpectedSeed)); !errors.Is(err, ErrSeedEndpoint) {
		t.Fatalf("SourceUser endpoint protection relaxed: %v", err)
	}
	current, source, _ := store.Current()
	if source != SourceBuiltin || recoverySeedDigest(current) != recoverySeedDigest(builtin) {
		t.Fatal("rejected admission modified old seed")
	}
	// No-op compares trusted fields, preserves the active user override and saves nothing.
	var saves int
	store.persist = func(_ context.Context, _ []byte) error { saves++; return nil }
	user := builtin
	user.VKHashes = []string{"keep-user-override"}
	if err := store.Update(SourceUser, seedJSON(t, user)); err != nil {
		t.Fatal(err)
	}
	before, _ := store.State()
	if err := store.CommitRecovery(context.Background(), same); err != nil {
		t.Fatal(err)
	}
	after, _ := store.State()
	if saves != 1 || !bytes.Equal(before, after) {
		t.Fatal("equal-identical cleared override or saved")
	}
}
