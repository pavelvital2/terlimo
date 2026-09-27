package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
)

// refClientWGAllowed is a verbatim copy of the pre-change predicate, used to prove the new
// decision helper preserves the exact boolean for every branch.
func refClientWGAllowed(identity accessIdentity, packet []byte, keys *wgKeys) bool {
	own, _, managed := clientTestGrantFor(identity)
	if !managed {
		return true
	}
	if len(packet) < 4 {
		return !managed
	}
	kind := binary.LittleEndian.Uint32(packet[:4])
	if kind == 1 {
		peer, e := clientWGPeer(packet, keys)
		if e != nil {
			return !managed
		}
		dbMutex.Lock()
		defer dbMutex.Unlock()
		matched := ""
		for _, p := range db.Passwords {
			if p == nil || p.ClientTest == nil {
				continue
			}
			d := db.Devices[p.ClientTest.RegistrationID]
			if d == nil {
				continue
			}
			pk, e := base64.StdEncoding.DecodeString(d.PubKey)
			if e == nil && subtleCompare(pk, peer) == 1 {
				matched = p.ClientTest.GrantID
				break
			}
		}
		if managed {
			return matched == own.GrantID && !own.Revoked
		}
		return matched == ""
	}
	if kind == 4 && len(packet) >= 16 {
		index := binary.LittleEndian.Uint32(packet[4:8])
		clientWGIndices.Lock()
		defer clientWGIndices.Unlock()
		entry, exists := clientWGIndices.byIndex[index]
		if exists && time.Now().After(entry.expires) {
			delete(clientWGIndices.byIndex, index)
			exists = false
		}
		if managed {
			return exists && entry.grant == own.GrantID
		}
		return !exists
	}
	return !managed
}

func j01Keys() (*wgKeys, []byte) {
	priv := make([]byte, 32)
	_, _ = rand.Read(priv)
	pub, _ := curve25519.X25519(priv, curve25519.Basepoint)
	return &wgKeys{serverPrivate: base64.StdEncoding.EncodeToString(priv), serverPublic: base64.StdEncoding.EncodeToString(pub)}, pub
}

func j01InitPacket(serverPub, clientPub []byte) []byte {
	ephemeral := make([]byte, 32)
	_, _ = rand.Read(ephemeral)
	epub, _ := curve25519.X25519(ephemeral, curve25519.Basepoint)
	packet := make([]byte, 148)
	binary.LittleEndian.PutUint32(packet, 1)
	copy(packet[8:], epub)
	h := blake2s.Sum256(append(device.InitialHash[:], serverPub...))
	h = blake2s.Sum256(append(h[:], epub...))
	var ck, key [32]byte
	device.KDF1(&ck, device.InitialChainKey[:], epub)
	shared, _ := curve25519.X25519(ephemeral, serverPub)
	device.KDF2(&ck, &key, ck[:], shared)
	a, _ := chacha20poly1305.New(key[:])
	copy(packet[40:], a.Seal(nil, make([]byte, 12), clientPub, h[:]))
	return packet
}

func j01Data(idx uint32) []byte {
	p := make([]byte, 32)
	binary.LittleEndian.PutUint32(p, 4)
	binary.LittleEndian.PutUint32(p[4:], idx)
	return p
}

func j01ClearIndices() {
	clientWGIndices.Lock()
	clientWGIndices.byIndex = map[uint32]clientWGIndex{}
	clientWGIndices.Unlock()
}

type j01Case struct {
	name       string
	packet     []byte
	wantAllow  bool
	wantClass  wgPacketClass
	wantReason wgRejectReason
}

func TestWGDecisionEquivalenceAndEnumMapping(t *testing.T) {
	j01ClearIndices()
	defer j01ClearIndices()
	legacy := accessIdentity{id: "legacy", password: "s1j01-legacy-pw"}

	// Short packet branch (needs a managed identity).
	t.Run("short", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, _ := j01Keys()
		packet := []byte{1, 2}
		assertDecision(t, identity, packet, keys, false, wgClassShort, wgReasonUnsupported)
	})

	// Legacy (unmanaged) always allowed.
	t.Run("legacy", func(t *testing.T) {
		managedFixture(t) // ensure a non-nil db; the legacy password is absent from it
		keys, _ := j01Keys()
		packet := j01Data(4242)
		if !clientWGAllowed(legacy, packet, keys) || !refClientWGAllowed(legacy, packet, keys) {
			t.Fatal("legacy must be allowed")
		}
		if allowed, _, _ := clientWGDecision(legacy, packet, keys); !allowed {
			t.Fatal("legacy decision must allow")
		}
	})

	t.Run("init_decrypt", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, _ := j01Keys()
		packet := make([]byte, 148)
		binary.LittleEndian.PutUint32(packet, 1)
		_, _ = rand.Read(packet[8:])
		assertDecision(t, identity, packet, keys, false, wgClassInit, wgReasonDecrypt)
	})

	t.Run("init_no_match", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, pub := j01Keys()
		clientPriv := make([]byte, 32)
		_, _ = rand.Read(clientPriv)
		clientPub, _ := curve25519.X25519(clientPriv, curve25519.Basepoint)
		packet := j01InitPacket(pub, clientPub) // no device registered
		assertDecision(t, identity, packet, keys, false, wgClassInit, wgReasonNoMatch)
	})

	t.Run("init_own_allowed", func(t *testing.T) {
		identity, g, _ := managedFixture(t)
		keys, pub := j01Keys()
		clientPriv := make([]byte, 32)
		_, _ = rand.Read(clientPriv)
		clientPub, _ := curve25519.X25519(clientPriv, curve25519.Basepoint)
		dbMutex.Lock()
		db.Devices[g.RegistrationID] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(clientPub)}
		dbMutex.Unlock()
		packet := j01InitPacket(pub, clientPub)
		assertDecision(t, identity, packet, keys, true, wgClassInit, "")
	})

	t.Run("init_revoked_own", func(t *testing.T) {
		identity, g, _ := managedFixture(t)
		keys, pub := j01Keys()
		clientPriv := make([]byte, 32)
		_, _ = rand.Read(clientPriv)
		clientPub, _ := curve25519.X25519(clientPriv, curve25519.Basepoint)
		dbMutex.Lock()
		db.Devices[g.RegistrationID] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(clientPub)}
		db.Passwords[identity.password].ClientTest.Revoked = true
		dbMutex.Unlock()
		packet := j01InitPacket(pub, clientPub)
		assertDecision(t, identity, packet, keys, false, wgClassInit, wgReasonRevoked)
	})

	t.Run("init_different_grant", func(t *testing.T) {
		identity, g, _ := managedFixture(t)
		keys, pub := j01Keys()
		clientPriv := make([]byte, 32)
		_, _ = rand.Read(clientPriv)
		clientPub, _ := curve25519.X25519(clientPriv, curve25519.Basepoint)
		// own device key does NOT match the initiator; a second grant's device does.
		other := make([]byte, 32)
		_, _ = rand.Read(other)
		otherPub, _ := curve25519.X25519(other, curve25519.Basepoint)
		dbMutex.Lock()
		db.Devices[g.RegistrationID] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(otherPub)}
		db.Passwords["s1j01-other-pw"] = &PasswordEntry{DeviceID: "s1j01-other-reg", ClientTest: &ClientTestGrant{GrantID: "s1j01-other-grant", RegistrationID: "s1j01-other-reg", NodeID: clientTestNodeID, PublicKey: g.PublicKey, Generation: "1", LeaseSeq: "1"}}
		db.Devices["s1j01-other-reg"] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(clientPub)}
		dbMutex.Unlock()
		packet := j01InitPacket(pub, clientPub)
		assertDecision(t, identity, packet, keys, false, wgClassInit, wgReasonDifferentGrant)
	})

	t.Run("data_malformed", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, _ := j01Keys()
		packet := make([]byte, 8)
		binary.LittleEndian.PutUint32(packet, 4)
		assertDecision(t, identity, packet, keys, false, wgClassData, wgReasonUnsupported)
	})

	t.Run("data_index_missing", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, _ := j01Keys()
		assertDecision(t, identity, j01Data(77001), keys, false, wgClassData, wgReasonIndexMissing)
	})

	t.Run("data_index_own", func(t *testing.T) {
		identity, g, _ := managedFixture(t)
		keys, _ := j01Keys()
		clientWGIndices.Lock()
		clientWGIndices.byIndex[77002] = clientWGIndex{grant: g.GrantID, expires: time.Now().Add(time.Minute)}
		clientWGIndices.Unlock()
		assertDecision(t, identity, j01Data(77002), keys, true, wgClassData, "")
	})

	t.Run("data_index_other_grant", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, _ := j01Keys()
		clientWGIndices.Lock()
		clientWGIndices.byIndex[77003] = clientWGIndex{grant: "s1j01-other-grant", expires: time.Now().Add(time.Minute)}
		clientWGIndices.Unlock()
		assertDecision(t, identity, j01Data(77003), keys, false, wgClassData, wgReasonDifferentGrant)
	})

	t.Run("data_index_expired_deletes", func(t *testing.T) {
		identity, g, _ := managedFixture(t)
		keys, _ := j01Keys()
		clientWGIndices.Lock()
		clientWGIndices.byIndex[77004] = clientWGIndex{grant: g.GrantID, expires: time.Now().Add(-time.Second)}
		clientWGIndices.Unlock()
		assertDecision(t, identity, j01Data(77004), keys, false, wgClassData, wgReasonIndexExpired)
		clientWGIndices.Lock()
		_, still := clientWGIndices.byIndex[77004]
		clientWGIndices.Unlock()
		if still {
			t.Fatal("expired index was not deleted")
		}
	})

	t.Run("other_kind", func(t *testing.T) {
		identity, _, _ := managedFixture(t)
		keys, _ := j01Keys()
		packet := make([]byte, 32)
		binary.LittleEndian.PutUint32(packet, 7)
		assertDecision(t, identity, packet, keys, false, wgClassOther, wgReasonUnsupported)
	})
}

func subtleCompare(a, b []byte) int {
	if len(a) != len(b) {
		return 0
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	if v == 0 {
		return 1
	}
	return 0
}

func assertDecision(t *testing.T, identity accessIdentity, packet []byte, keys *wgKeys, wantAllow bool, wantClass wgPacketClass, wantReason wgRejectReason) {
	t.Helper()
	// Decision first: the expired branch mutates (deletes) the index, exactly as the
	// original predicate did; the reference is then evaluated on the same post-delete state.
	allowed, class, reason := clientWGDecision(identity, packet, keys)
	if class != wantClass || reason != wantReason {
		t.Fatalf("classifier got (%s,%s) want (%s,%s)", class, reason, wantClass, wantReason)
	}
	got := clientWGAllowed(identity, packet, keys)
	ref := refClientWGAllowed(identity, packet, keys)
	if ref != wantAllow || got != wantAllow || allowed != wantAllow {
		t.Fatalf("allow mismatch: ref=%v got=%v decision=%v want=%v", ref, got, allowed, wantAllow)
	}
}

func TestWGMismatchErrorClassUnchanged(t *testing.T) {
	err := &wgMismatchError{packetClass: wgClassData, reason: wgReasonIndexMissing}
	if got := managedErrorClass(err); got != "WG_GRANT_MISMATCH" {
		t.Fatalf("managedErrorClass=%s want WG_GRANT_MISMATCH", got)
	}
}

func TestWGFirstExitBoundedClassifierLogging(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	trace := newManagedConnTrace()
	trace.exit("proxy_wg_write", &wgMismatchError{packetClass: wgClassData, reason: wgReasonIndexMissing})
	trace.exit("later_wg_write", &wgMismatchError{packetClass: wgClassInit, reason: wgReasonDecrypt})
	trace.exit("later_close", net.ErrClosed)

	out := buf.String()
	if strings.Count(out, "[MANAGED_TRACE]") != 1 {
		t.Fatalf("expected a single bounded first-exit line, got %q", out)
	}
	for _, want := range []string{"stage=proxy_wg_write", "class=WG_GRANT_MISMATCH", "packet_class=data", "reject_reason=index_missing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	for _, bad := range []string{"later_wg_write", "later_close", "packet_class=init", "reject_reason=decrypt"} {
		if strings.Contains(out, bad) {
			t.Fatalf("unexpected %q in %q", bad, out)
		}
	}
}

func TestWGClassifierLogHasNoSensitiveContent(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	identity, g, _ := managedFixture(t)
	dbMutex.Lock()
	db.Passwords[identity.password].ClientTest.GrantID = "GRANT-SECRET-abc123"
	g.GrantID = "GRANT-SECRET-abc123"
	dbMutex.Unlock()
	g.GrantID = "GRANT-SECRET-abc123"
	keys, _ := j01Keys()
	err := clientTestWriteWG(identity, g.Generation, j01Data(88001), keys, func([]byte) error { return nil })
	if err == nil || managedErrorClass(err) != "WG_GRANT_MISMATCH" {
		t.Fatalf("expected WG_GRANT_MISMATCH, got %v", err)
	}
	trace := newManagedConnTrace()
	trace.exit("first_wg_write", err)

	out := buf.String()
	for _, secret := range []string{"GRANT-SECRET-abc123", identity.password, "88001", "192.0.2.9", "fixture-payload", "arbitrary-exception-text"} {
		if strings.Contains(out, secret) {
			t.Fatalf("sensitive content %q leaked in %q", secret, out)
		}
	}
	if !strings.Contains(out, "packet_class=data reject_reason=index_missing") {
		t.Fatalf("bounded classifier missing in %q", out)
	}
}
