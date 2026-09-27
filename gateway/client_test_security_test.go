package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
	"net"
	"os"
	"testing"
	"time"
	"wg-turn-client/internal/wlwire"
)

func managedFixture(t *testing.T) (accessIdentity, ClientTestGrant, *ecdsa.PrivateKey) {
	t.Helper()
	t.Setenv("WL_TEST_ENABLED", "1")
	oldNodeID := clientTestNodeID
	clientTestNodeID = "test-1"
	t.Cleanup(func() { clientTestNodeID = oldNodeID })
	oldCap, oldMbps := configuredAccessRuntimeLimits()
	configureAccessRuntime(2, oldMbps)
	t.Cleanup(func() { configureAccessRuntime(oldCap, oldMbps) })
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	g := ClientTestGrant{GrantID: "test-grant", RegistrationID: "22222222-2222-4222-8222-222222222222", NodeID: "test-1", PublicKey: wlwire.Encode(spki), Generation: "1", LeaseSeq: "1"}
	i := accessIdentity{id: "test-access", password: "only-in-memory-test"}
	old := db
	db = &Database{Passwords: map[string]*PasswordEntry{i.password: {DeviceID: g.RegistrationID, ExpiresAt: time.Now().Add(time.Minute).Unix(), ClientTest: &g}}, Devices: map[string]*ClientDevice{}}
	t.Cleanup(func() { clientTestCloseGrant(g.GrantID); dbMutex.Lock(); db = old; dbMutex.Unlock() })
	return i, g, key
}
func TestManagedPoPDTLSReplayAndIdleExpiry(t *testing.T) {
	t.Run("finite", func(t *testing.T) { testManagedPoPDTLSReplayAndIdleExpiry(t, false) })
	t.Run("no_expiry", func(t *testing.T) { testManagedPoPDTLSReplayAndIdleExpiry(t, true) })
}

func testManagedPoPDTLSReplayAndIdleExpiry(t *testing.T, noExpiry bool) {
	identity, g, key := managedFixture(t)
	if noExpiry {
		db.Passwords[identity.password].ExpiresAt = 0
	}
	cert, e := selfsign.GenerateSelfSigned()
	if e != nil {
		t.Fatal(e)
	}
	listener, e := dtls.Listen("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &dtls.Config{Certificates: []tls.Certificate{cert}, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret})
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	secured := make(chan net.Conn, 1)
	errs := make(chan error, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			errs <- e
			return
		}
		dc := c.(*dtls.Conn)
		if e = dc.HandshakeContext(ctx); e != nil {
			errs <- e
			return
		}
		mc, e := clientTestAuthenticate(ctx, dc, identity, g, nil)
		if e != nil {
			errs <- e
			return
		}
		secured <- mc
		b := make([]byte, 1600)
		_, _ = mc.Read(b)
	}()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	client, e := dtls.Dial("udp4", listener.Addr().(*net.UDPAddr), &dtls.Config{InsecureSkipVerify: true, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		c, e := x509.ParseCertificate(raw[0])
		if e != nil {
			return e
		}
		h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
		if h != pin {
			return wlwire.ErrProof
		}
		return nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	if e = client.HandshakeContext(ctx); e != nil {
		t.Fatal(e)
	}
	send := func(id wlwire.ID, v any) {
		b, _ := json.Marshal(v)
		frames, _ := wlwire.Frames(id, false, b)
		for _, f := range frames {
			if _, e := client.Write(f); e != nil {
				t.Fatal(e)
			}
		}
	}
	recv := func() map[string]any {
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 2048)
		n, e := client.Read(b)
		if e != nil {
			t.Fatal(e)
		}
		var a wlwire.Assembler
		_, body, e := a.Add(b[:n], time.Now())
		if e != nil {
			t.Fatal(e)
		}
		var v map[string]any
		if e = json.Unmarshal(body, &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	aid := wlwire.ID{1}
	bid := wlwire.ID{2}
	begin := map[string]any{"v": 1, "op": "VPN_AUTH_BEGIN"}
	send(aid, begin)
	challenge := recv()
	accessExpiry, hasAccessExpiry := challenge["access_expires_at"]
	if !hasAccessExpiry {
		t.Fatal("challenge omitted mandatory access_expires_at")
	}
	if noExpiry && (challenge["v"] != float64(2) || accessExpiry != nil) {
		t.Fatalf("no-expiry grant did not reach v2/null challenge: %#v", challenge)
	}
	if !noExpiry && (challenge["v"] != float64(1) || accessExpiry == nil) {
		t.Fatalf("finite grant did not retain v1/finite challenge: %#v", challenge)
	}
	send(aid, begin)
	duplicate := recv()
	if duplicate["nonce"] != challenge["nonce"] {
		t.Fatal("challenge duplicate changed")
	}
	state, _ := client.ConnectionState()
	exporter, _ := state.ExportKeyingMaterial("EXPORTER-WL-VPN-POP-1", nil, 32)
	cid, _ := wlwire.Decode(challenge["challenge_id"].(string), 16)
	nonce, _ := wlwire.Decode(challenge["nonce"].(string), 32)
	payload, _ := json.Marshal(clientAuthPayload{Op: "vpn_auth", Node: g.NodeID, Grant: g.GrantID, Registration: g.RegistrationID, Generation: "1", Seq: "1", Session: "fixture-session-0001", Worker: "0", Mode: "getconf"})
	tr := wlwire.Transcript("WL-VPN-POP-1", exporter, cid, nonce, payload)
	h := sha256.Sum256(tr)
	sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
	auth := clientAuthBody{V: 1, Op: "VPN_AUTH", Challenge: wlwire.Encode(cid), Payload: wlwire.Encode(payload), Proof: wlwire.Encode(sig)}
	send(bid, auth)
	ok := recv()
	if ok["op"] != "VPN_AUTH_OK" {
		t.Fatal(ok)
	}
	var mc net.Conn
	select {
	case mc = <-secured:
	case e = <-errs:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal("auth timeout")
	}
	defer mc.Close()
	// Android may create a different DER signature when retrying exact payload bytes.
	sig, _ = ecdsa.SignASN1(rand.Reader, key, h[:])
	auth.Proof = wlwire.Encode(sig)
	send(bid, auth)
	again := recv()
	if again["op"] != "VPN_AUTH_OK" {
		t.Fatal("lost ACK replay")
	}
	dbMutex.Lock()
	db.Passwords[identity.password].ExpiresAt = time.Now().Unix()
	dbMutex.Unlock()
	deadline := time.Now().Add(time.Second)
	for clientTestWorkerCount(g.GrantID) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if clientTestWorkerCount(g.GrantID) != 0 {
		t.Fatal("idle worker not cancelled")
	}
}
func TestManagedWGIdentityAndCrossAccess(t *testing.T) {
	identity, g, _ := managedFixture(t)
	priv := make([]byte, 32)
	_, _ = rand.Read(priv)
	pub, _ := curve25519.X25519(priv, curve25519.Basepoint)
	keys := &wgKeys{serverPrivate: base64.StdEncoding.EncodeToString(priv), serverPublic: base64.StdEncoding.EncodeToString(pub)}
	clientPriv := make([]byte, 32)
	_, _ = rand.Read(clientPriv)
	clientPub, _ := curve25519.X25519(clientPriv, curve25519.Basepoint)
	db.Devices[g.RegistrationID] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(clientPub)}
	ephemeral := make([]byte, 32)
	_, _ = rand.Read(ephemeral)
	epub, _ := curve25519.X25519(ephemeral, curve25519.Basepoint)
	packet := make([]byte, 148)
	binary.LittleEndian.PutUint32(packet, 1)
	copy(packet[8:], epub)
	h := blake2s.Sum256(append(device.InitialHash[:], pub...))
	h = blake2s.Sum256(append(h[:], epub...))
	var ck, key [32]byte
	device.KDF1(&ck, device.InitialChainKey[:], epub)
	shared, _ := curve25519.X25519(ephemeral, pub)
	device.KDF2(&ck, &key, ck[:], shared)
	a, _ := chacha20poly1305.New(key[:])
	copy(packet[40:], a.Seal(nil, make([]byte, 12), clientPub, h[:]))
	if !clientWGAllowed(identity, packet, keys) {
		t.Fatal("own WG handshake denied")
	}
	// Legacy now has a different engine/key and cannot select the managed endpoint.
	// Its index namespace must not be blocked by an unrelated managed index.
	response := make([]byte, 92)
	binary.LittleEndian.PutUint32(response, 2)
	binary.LittleEndian.PutUint32(response[4:], 1001)
	if !clientWGRemember(identity, response) {
		t.Fatal("index")
	}
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data, 4)
	binary.LittleEndian.PutUint32(data[4:], 1001)
	if !clientWGAllowed(identity, data, keys) || !clientWGAllowed(accessIdentity{id: "legacy", password: "other"}, data, keys) {
		t.Fatal("cross-access index")
	}
}

func TestManagedAdminFenceAndNoLegacyMutation(t *testing.T) {
	identity, g, _ := managedFixture(t)
	dir := t.TempDir()
	g.OperationID = "refresh-1"
	command := clientTestCommand{Operation: "refresh_lease", Password: identity.password, Grant: g, ExpectedSeq: "1", ExpiresAt: time.Now().Add(2 * time.Minute).Unix()}
	command.Grant.LeaseSeq = "2"
	raw, _ := json.Marshal(command)
	result, e := clientTestAdmin(dir, raw, nil)
	if e != nil {
		t.Fatal(e)
	}
	if result["lease_seq"] != "2" {
		t.Fatal(result)
	}
	if _, e = clientTestAdmin(dir, raw, nil); e != nil {
		t.Fatal("idempotent retry", e)
	}
	revoke := command
	revoke.Operation = "grant_revoke"
	revoke.Grant.Generation = "2"
	revoke.Grant.Revoked = true
	revoke.Grant.OperationID = "revoke-1"
	rawRevoke, _ := json.Marshal(revoke)
	if _, e = clientTestAdmin(dir, rawRevoke, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = clientTestAdmin(dir, raw, nil); e == nil {
		t.Fatal("stale refresh reactivated revoked grant")
	}
	if !clientTestProtectedLegacyCommand([]string{"activate", "--password", identity.password}) {
		t.Fatal("legacy mutation allowed")
	}
	if clientTestDeviceOwnedLocked("unrelated", g.RegistrationID) || !clientTestDeviceOwnedLocked(identity.password, g.RegistrationID) || !clientTestDeviceOwnedLocked("unrelated", "legacy-device") {
		t.Fatal("GETCONF device ownership boundary")
	}
	if clientTestProtectedLegacyCommand([]string{"activate", "--password", "unrelated"}) {
		t.Fatal("unrelated changed")
	}
}

func TestBootstrapIdentityDoesNotEnterVPNState(t *testing.T) {
	_, _, _ = managedFixture(t)
	b := ClientTestBootstrap{CredentialID: "fixture", Secret: "synthetic-bootstrap-secret-only", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	db.ClientBootstrap = map[string]ClientTestBootstrap{"fixture": b}
	identity := accessIdentity{id: "bootstrap-fixture", password: b.Secret}
	if _, ok := clientTestBootstrapFor(identity); !ok {
		t.Fatal("bootstrap identity")
	}
	if currentAccessIdentityState(identity) != accessIdentityUnknown {
		t.Fatal("bootstrap became ordinary access")
	}
	if _, _, ok := clientTestGrantFor(identity); ok {
		t.Fatal("bootstrap became grant")
	}
}

type failingClientWG struct{ fail bool }

func (d *failingClientWG) Close() {}
func (d *failingClientWG) IpcSet(string) error {
	if d.fail {
		return errors.New("fixture")
	}
	return nil
}
func TestManagedRevokeReadbackRequiresPeerRemoval(t *testing.T) {
	identity, g, _ := managedFixture(t)
	db.Devices[g.RegistrationID] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	g.Generation, g.OperationID, g.Revoked = "2", "revoke-remove", true
	raw, _ := json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: g, ExpiresAt: db.Passwords[identity.password].ExpiresAt})
	dir := t.TempDir()
	wg := &failingClientWG{fail: true}
	if _, err := clientTestAdmin(dir, raw, wg); err == nil {
		t.Fatal("failed removal acknowledged")
	}
	if clientTestReadback(db.Passwords[identity.password])["runtime_applied"] != false {
		t.Fatal("false runtime readback")
	}
	wg.fail = false
	result, err := clientTestAdmin(dir, raw, wg)
	if err != nil || result["runtime_applied"] != true {
		t.Fatal("retry failed", err)
	}
}

func TestManagedRevokeDominatesLostRefreshAndDelayedProvision(t *testing.T) {
	identity, g, _ := managedFixture(t)
	dir := t.TempDir()
	db.Passwords[identity.password].ClientTest.LeaseSeq = "2"
	g.LeaseSeq, g.Generation, g.OperationID, g.Revoked = "1", "2", "revoke-lost-refresh", true
	raw, _ := json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: g})
	result, err := clientTestAdmin(dir, raw, nil)
	if err != nil || result["lease_seq"] != "2" {
		t.Fatal("revoke lost refresh", err)
	}
	delete(db.Passwords, identity.password)
	g.OperationID = "revoke-before-provision"
	raw, _ = json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: g})
	if _, err = clientTestAdmin(dir, raw, nil); err != nil {
		t.Fatal("tombstone", err)
	}
	t.Setenv("WL_TEST_WG_ISOLATION_CONFIRMED", "1")
	g.Generation, g.Revoked, g.OperationID = "1", false, "delayed-provision"
	raw, _ = json.Marshal(clientTestCommand{Operation: "grant_provision", Password: identity.password, Grant: g, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if _, err = clientTestAdmin(dir, raw, nil); err == nil {
		t.Fatal("delayed provision resurrected revoked grant")
	}
}

func TestManagedExpiryWithoutWorkers(t *testing.T) {
	identity, g, _ := managedFixture(t)
	db.Passwords[identity.password].ExpiresAt = time.Now().Unix()
	db.Devices[g.RegistrationID] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	delete(clientTestPeerRemoved, g.GrantID)
	wg := &failingClientWG{fail: true}
	clientTestExpirySweep(wg)
	if clientTestPeerRemoved[g.GrantID] {
		t.Fatal("failed expiry removal acknowledged")
	}
	wg.fail = false
	clientTestExpirySweep(wg)
	if !clientTestPeerRemoved[g.GrantID] {
		t.Fatal("expiry removal not retried")
	}
}

func TestManagedWorkerSchema(t *testing.T) {
	for _, tt := range []struct {
		worker, mode string
		cap          int
		want         bool
	}{
		{"0", "getconf", 2, true}, {"1", "data", 2, true},
		{"worker-0", "getconf", 2, false}, {"00", "getconf", 2, false},
		{"+1", "data", 2, false}, {"-1", "data", 2, false},
		{"0", "data", 2, false}, {"1", "getconf", 2, false},
		{"2", "data", 2, false}, {"0", "getconf", 0, false},
	} {
		if clientTestWorkerValid(tt.worker, tt.mode, tt.cap) != tt.want {
			t.Fatalf("worker %q mode %s cap %d", tt.worker, tt.mode, tt.cap)
		}
	}
}

func TestSignedSchemaFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/wl_schema_signed_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Payload clientAuthPayload `json:"vpn_payload"`
		Signed  struct {
			PublicKey  string   `json:"public_key_spki"`
			Payload    string   `json:"payload_b64"`
			Transcript string   `json:"transcript_hex"`
			Proof      string   `json:"proof_b64"`
			Frames     []string `json:"auth_frames_hex"`
		} `json:"signed_vpn"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !validTransportSession(fixture.Payload.Session) || !clientTestWorkerValid(fixture.Payload.Worker, fixture.Payload.Mode, 1) {
		t.Fatal("positive schema")
	}
	key, _ := decodeClientB64(fixture.Signed.PublicKey)
	tr, _ := hex.DecodeString(fixture.Signed.Transcript)
	sig, _ := decodeClientB64(fixture.Signed.Proof)
	if err = wlwire.Verify(key, tr, sig); err != nil {
		t.Fatal(err)
	}
	var assembly wlwire.Assembler
	var body []byte
	for _, encoded := range fixture.Signed.Frames {
		packet, _ := hex.DecodeString(encoded)
		_, body, err = assembly.Add(packet, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	var auth clientAuthBody
	if wlwire.StrictJSON(body, &auth) != nil || auth.Payload != fixture.Signed.Payload || auth.Proof != fixture.Signed.Proof {
		t.Fatal("full AUTH envelope")
	}
}
