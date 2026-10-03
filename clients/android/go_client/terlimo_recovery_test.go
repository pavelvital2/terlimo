package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"testing"

	"wg-turn-client/servicechannel"
)

// Exercise the actual native start→signature validation→candidate transport wiring.
// Failed candidate I/O must never reach the durable namespace writer.
func TestRecoveryMobileCandidateWiring(t *testing.T) {
	var fixture struct {
		PublicKey string              `json:"public_key_b64"`
		Code      string              `json:"code"`
		Foreign   string              `json:"foreign_signature_code"`
		Seed      servicechannel.Seed `json:"expected_seed"`
	}
	raw, err := os.ReadFile("servicechannel/testdata/recovery-v1/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	old := fixture.Seed
	old.Revision = "1"
	old.PeerIP = "192.0.2.9"
	old.VKHashes = []string{"old-test-hash"}
	builtin, _ := json.Marshal(old)
	start := managedStart{MobileBaseURL: "https://installed-origin.invalid", MobileEnvironment: "test",
		ServiceSeed: string(builtin), RecoveryVerifyKeyB64: fixture.PublicKey, RecoveryCode: fixture.Code}
	writes := 0
	persist := func(context.Context, []byte) error { writes++; return nil }
	for _, code := range []string{"TR1.truncated", fixture.Foreign} {
		rejected := start
		rejected.RecoveryCode = code
		if _, _, err := newMobileTransportAndStore(rejected, persist); err == nil {
			t.Fatal("corrupted code accepted by native wiring")
		}
	}
	missingKey := start
	missingKey.RecoveryVerifyKeyB64 = ""
	if _, _, err := newMobileTransportAndStore(missingKey, persist); !errors.Is(err, servicechannel.ErrRecoveryUnavailable) {
		t.Fatal("untrusted signer admitted")
	}
	transport, durable, err := newMobileTransportAndStore(start, persist)
	if err != nil {
		t.Fatal(err)
	}
	recovery, ok := transport.(*mobileRecoveryTransport)
	if !ok {
		t.Fatal("recovery fell back to ordinary HTTP")
	}
	defer recovery.Close()
	if recovery.Base.String() != start.MobileBaseURL {
		t.Fatal("signed peer changed logical account origin")
	}
	before, _ := durable.State()
	got, _, _ := durable.Current()
	if got.PeerIP != old.PeerIP {
		t.Fatal("parse promoted candidate")
	}
	candidate, _, _ := recovery.Seeds.Current()
	if candidate.PeerIP != fixture.Seed.PeerIP || candidate.VKHashes[0] != fixture.Seed.VKHashes[0] {
		t.Fatal("transport not using signed candidate")
	}
	calls := 0
	recovery.Channel.Establishment = servicechannel.EstablishFunc(func(_ context.Context, seed servicechannel.Seed) (net.Conn, func(), error) {
		calls++
		if seed.PeerIP != candidate.PeerIP {
			t.Fatal("dial selected old peer")
		}
		return nil, nil, errors.New("controlled candidate failure")
	})
	request, _ := http.NewRequest(http.MethodGet, start.MobileBaseURL+"/api/mobile/v1/me", nil)
	if _, err = recovery.Do(request); err == nil {
		t.Fatal("candidate failure was hidden")
	}
	after, _ := durable.State()
	if calls != 1 || writes != 0 || !bytes.Equal(before, after) {
		t.Fatal("candidate failure changed last-good durability")
	}
}
