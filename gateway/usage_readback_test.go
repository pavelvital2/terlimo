package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type fakeUsageEngine struct {
	raw string
	err error
}

func (f *fakeUsageEngine) IpcSet(string) error { return nil }
func (f *fakeUsageEngine) Close()              {}
func (f *fakeUsageEngine) PeerUsage() ([]peerCounters, error) {
	if f.err != nil {
		return nil, f.err
	}
	return parseUAPIPeerUsage(f.raw)
}

func testKeyHex(fill byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = fill
	}
	return hex.EncodeToString(raw)
}

func TestParseUAPIPeerUsage(t *testing.T) {
	keyA, keyB := testKeyHex(0x11), testKeyHex(0x22)
	raw := strings.Join([]string{
		"private_key=" + keyB,
		"listen_port=56002",
		"public_key=" + keyA,
		"rx_bytes=1000",
		"tx_bytes=2000",
		"last_handshake_time_sec=1700000000",
		"public_key=" + keyB,
		"rx_bytes=7",
		"tx_bytes=9",
		"last_handshake_time_sec=1700000001",
	}, "\n")
	peers, err := parseUAPIPeerUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 || peers[0].PublicKeyHex != keyA || peers[0].RxBytes != 1000 ||
		peers[0].TxBytes != 2000 || peers[0].LastHandshake != 1700000000 {
		t.Fatalf("unexpected peers: %+v", peers)
	}
	if peers[1].PublicKeyHex != keyB || peers[1].RxBytes != 7 || peers[1].TxBytes != 9 {
		t.Fatalf("unexpected second peer: %+v", peers[1])
	}
}

func TestParseKernelWGDump(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x33
	}
	b64 := base64.StdEncoding.EncodeToString(key)
	dump := fmt.Sprintf("privkey\tpubkey\t56001\n%s\t(none)\t1.2.3.4:5\t10.67.67.2/32\t1700000000\t123\t456\t25\n", b64)
	peers, err := parseKernelWGDump(dump)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].PublicKeyHex != testKeyHex(0x33) ||
		peers[0].RxBytes != 123 || peers[0].TxBytes != 456 || peers[0].LastHandshake != 1700000000 {
		t.Fatalf("unexpected kernel peers: %+v", peers)
	}
}

func TestUsageOperationReturnsKnownPeerCountersOnly(t *testing.T) {
	t.Setenv("WL_TEST_ENABLED", "1")
	oldNodeID := clientTestNodeID
	clientTestNodeID = "usage-node"
	t.Cleanup(func() { clientTestNodeID = oldNodeID })

	known, unknown := testKeyHex(0x44), testKeyHex(0x55)
	raw := strings.Join([]string{
		"public_key=" + known,
		"rx_bytes=11",
		"tx_bytes=22",
		"last_handshake_time_sec=1700000002",
		"public_key=" + unknown,
		"rx_bytes=99",
		"tx_bytes=99",
		"last_handshake_time_sec=1700000003",
	}, "\n")
	router := &engineRouter{
		keys:    &wgKeys{},
		managed: &fakeUsageEngine{raw: raw},
		peers: map[string]enginePeer{
			known: {managed: true, ip: "10.67.67.2", registration: "reg-1"},
		},
	}
	command := clientTestCommand{Operation: "usage"}
	payload, _ := json.Marshal(command)
	result, err := clientTestAdmin(t.TempDir(), payload, router)
	if err != nil {
		t.Fatal(err)
	}
	if result["counters_supported"] != true || result["boot_id"] == "" {
		t.Fatalf("missing support/boot metadata: %+v", result)
	}
	startedAt, ok := result["boot_started_at"].(int64)
	if !ok || startedAt <= 0 {
		t.Fatalf("missing boot_started_at ordering key: %+v", result)
	}
	peers, ok := result["peers"].([]peerUsageEntry)
	if !ok || len(peers) != 1 {
		t.Fatalf("unexpected peers payload: %#v", result["peers"])
	}
	if peers[0].RegistrationID != "reg-1" || peers[0].RxBytes != 11 || peers[0].TxBytes != 22 || !peers[0].Managed {
		t.Fatalf("unexpected peer entry: %+v", peers[0])
	}
	firstSequence, _ := result["sequence"].(int64)
	again, err := clientTestAdmin(t.TempDir(), payload, router)
	if err != nil {
		t.Fatal(err)
	}
	secondSequence, _ := again["sequence"].(int64)
	if secondSequence <= firstSequence {
		t.Fatalf("sequence must increase: %d -> %d", firstSequence, secondSequence)
	}
	if again["boot_started_at"] != startedAt {
		t.Fatalf("boot_started_at must be stable within a process: %v -> %v", startedAt, again["boot_started_at"])
	}
}

func TestEngineRegistrationMatchesGrantIdentity(t *testing.T) {
	fingerprint := testKeyHex(0x77)
	publicKey := make([]byte, 32)
	for i := range publicKey {
		publicKey[i] = 0x88
	}
	pubB64 := base64.StdEncoding.EncodeToString(publicKey)
	loaded := &Database{
		Devices: map[string]*ClientDevice{
			fingerprint: {DeviceID: fingerprint, IP: "10.67.67.2", PubKey: pubB64},
		},
		Passwords: map[string]*PasswordEntry{
			"fixture-password": {
				DeviceID:   fingerprint,
				ExpiresAt:  4_000_000_000,
				ClientTest: &ClientTestGrant{GrantID: "grant-1", RegistrationID: fingerprint, NodeID: "usage-node"},
			},
		},
	}
	router, err := validateEngineState(loaded)
	if err != nil {
		t.Fatal(err)
	}
	hexKey := hex.EncodeToString(publicKey)
	peer, known := router.peers[hexKey]
	if !known || peer.registration != fingerprint || !peer.managed {
		t.Fatalf("registration mapping mismatch: %+v known=%v", peer, known)
	}
	// Real readback path: counters observed for that peer map to the grant identity.
	router.managed = &fakeUsageEngine{raw: strings.Join([]string{
		"public_key=" + hexKey,
		"rx_bytes=5",
		"tx_bytes=6",
		"last_handshake_time_sec=1700000000",
	}, "\n")}
	entries, supported, err := router.PeerUsage()
	if err != nil || !supported || len(entries) != 1 || entries[0].RegistrationID != fingerprint {
		t.Fatalf("PeerUsage mapping mismatch: %+v supported=%v err=%v", entries, supported, err)
	}
}

func TestPeerUsageErrorsAreExplicit(t *testing.T) {
	router := &engineRouter{keys: &wgKeys{}, managed: &fakeUsageEngine{err: fmt.Errorf("engine down")}}
	if _, _, err := router.PeerUsage(); err == nil {
		t.Fatal("expected explicit engine error")
	}
	router = &engineRouter{keys: &wgKeys{}}
	peers, supported, err := router.PeerUsage()
	if err != nil || supported || len(peers) != 0 {
		t.Fatalf("unsupported engine must be explicit: %v %v %v", peers, supported, err)
	}
}
