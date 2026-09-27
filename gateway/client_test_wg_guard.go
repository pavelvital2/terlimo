package main

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
	"sync"
	"time"
)

// Only managed receiver indices are tracked. Existing unrelated legacy packets retain their path.
var clientWGIndices = struct {
	sync.Mutex
	byIndex map[uint32]clientWGIndex
}{byIndex: map[uint32]clientWGIndex{}}

type clientWGIndex struct {
	grant   string
	expires time.Time
}

// Bounded, non-sensitive classifier for a managed WG rejection. Only fixed enums; never
// payload, keys, grant ids, receiver indices, addresses or exception text. No age bucket is
// reported because clientWGIndex stores no creation timestamp (only expires), so an exact
// age cannot be derived.
type wgPacketClass string

const (
	wgClassShort wgPacketClass = "short"
	wgClassInit  wgPacketClass = "init"
	wgClassData  wgPacketClass = "data"
	wgClassOther wgPacketClass = "other"
)

type wgRejectReason string

const (
	wgReasonDecrypt        wgRejectReason = "decrypt"
	wgReasonNoMatch        wgRejectReason = "no_match"
	wgReasonDifferentGrant wgRejectReason = "different_grant"
	wgReasonRevoked        wgRejectReason = "revoked"
	wgReasonIndexMissing   wgRejectReason = "index_missing"
	wgReasonIndexExpired   wgRejectReason = "index_expired"
	wgReasonUnsupported    wgRejectReason = "unsupported"
)

// wgMismatchError keeps the existing WG_GRANT_MISMATCH class for managedErrorClass and adds
// only the bounded classifier enums.
type wgMismatchError struct {
	packetClass wgPacketClass
	reason      wgRejectReason
}

func (e *wgMismatchError) Error() string { return "WG_GRANT_MISMATCH" }

func clientWGPeer(packet []byte, keys *wgKeys) ([]byte, error) {
	if len(packet) != 148 || binary.LittleEndian.Uint32(packet[:4]) != 1 {
		return nil, errors.New("WG_HANDSHAKE_REQUIRED")
	}
	priv, e := base64.StdEncoding.DecodeString(keys.serverPrivate)
	if e != nil {
		return nil, e
	}
	pub, e := base64.StdEncoding.DecodeString(keys.serverPublic)
	if e != nil {
		return nil, e
	}
	// Same Noise IK static-key decoding as wireguard/device.ConsumeMessageInitiation.
	h := blake2s.Sum256(append(device.InitialHash[:], pub...))
	h = blake2s.Sum256(append(h[:], packet[8:40]...))
	var ck, key [32]byte
	device.KDF1(&ck, device.InitialChainKey[:], packet[8:40])
	shared, e := curve25519.X25519(priv, packet[8:40])
	if e != nil {
		return nil, e
	}
	device.KDF2(&ck, &key, ck[:], shared)
	a, e := chacha20poly1305.New(key[:])
	if e != nil {
		return nil, e
	}
	return a.Open(nil, make([]byte, 12), packet[40:88], h[:])
}

// clientWGAllowed keeps the original boolean API; it delegates to the decision helper so the
// allow/reject predicate, locks, TTL deletion and fail-closed behaviour are unchanged.
func clientWGAllowed(identity accessIdentity, packet []byte, keys *wgKeys) bool {
	allowed, _, _ := clientWGDecision(identity, packet, keys)
	return allowed
}

// clientWGDecision reproduces the exact clientWGAllowed boolean and additionally returns a
// bounded packet class and reject reason. On allow the reason is empty.
func clientWGDecision(identity accessIdentity, packet []byte, keys *wgKeys) (bool, wgPacketClass, wgRejectReason) {
	own, _, managed := clientTestGrantFor(identity)
	// Engine separation makes managed receiver indices unrelated to legacy indices.
	if !managed {
		return true, classifyAllowedPacket(packet), ""
	}
	if len(packet) < 4 {
		return false, wgClassShort, wgReasonUnsupported
	}
	kind := binary.LittleEndian.Uint32(packet[:4])
	if kind == 1 {
		peer, e := clientWGPeer(packet, keys)
		if e != nil {
			return false, wgClassInit, wgReasonDecrypt
		}
		dbMutex.Lock()
		defer dbMutex.Unlock()
		matched := clientWGMatchedGrantLocked(peer, own.GrantID)
		if matched == own.GrantID {
			if !own.Revoked {
				return true, wgClassInit, ""
			}
			// Own peer matched but the authoritative grant is revoked.
			return false, wgClassInit, wgReasonRevoked
		}
		if matched == "" {
			return false, wgClassInit, wgReasonNoMatch
		}
		// Peer matched a different grant.
		return false, wgClassInit, wgReasonDifferentGrant
	}
	if kind == 4 && len(packet) >= 16 {
		index := binary.LittleEndian.Uint32(packet[4:8])
		clientWGIndices.Lock()
		defer clientWGIndices.Unlock()
		entry, exists := clientWGIndices.byIndex[index]
		expired := false
		if exists && time.Now().After(entry.expires) {
			delete(clientWGIndices.byIndex, index)
			exists = false
			expired = true
		}
		if exists {
			if entry.grant == own.GrantID {
				return true, wgClassData, ""
			}
			return false, wgClassData, wgReasonDifferentGrant
		}
		if expired {
			return false, wgClassData, wgReasonIndexExpired
		}
		return false, wgClassData, wgReasonIndexMissing
	}
	if kind == 4 {
		// Data-class packet too short for a receiver index.
		return false, wgClassData, wgReasonUnsupported
	}
	// Managed clients initiate WG handshakes; unsolicited responses/cookies cannot create index state.
	return false, wgClassOther, wgReasonUnsupported
}

// classifyAllowedPacket is only used for allowed (legacy) packets where no reason is reported.
func classifyAllowedPacket(packet []byte) wgPacketClass {
	if len(packet) < 4 {
		return wgClassShort
	}
	switch binary.LittleEndian.Uint32(packet[:4]) {
	case 1:
		return wgClassInit
	case 4:
		return wgClassData
	default:
		return wgClassOther
	}
}

// clientWGMatchedGrantLocked maps a decoded handshake static key to the owning grant of a
// NON-TOMBSTONE record. Revoked/deactivated duplicates never own a key, so map iteration
// order cannot flip a live handshake into different_grant.
//
// Expiry is intentionally not evaluated here: a tombstone is a permanent duplicate of the
// same registration, while an expired-but-unswept entry either equals the connection's own
// grant (then the authoritative clientTestCurrentGrantActive lease check rejects the write)
// or differs from it (then different_grant rejects). Broadening the filter is unnecessary.
// Must be called with dbMutex held.
func clientWGMatchedGrantLocked(peer []byte, ownGrantID string) string {
	tombstoneOwn := false
	for _, p := range db.Passwords {
		if p == nil || p.ClientTest == nil {
			continue
		}
		d := db.Devices[p.ClientTest.RegistrationID]
		if d == nil {
			continue
		}
		pk, e := base64.StdEncoding.DecodeString(d.PubKey)
		if e != nil || subtle.ConstantTimeCompare(pk, peer) != 1 {
			continue
		}
		if p.ClientTest.Revoked || p.IsDeactivated {
			// A tombstone never owns a key. Only the connection's own tombstone is surfaced
			// so the caller can report the existing revoked classification instead of no_match.
			if p.ClientTest.GrantID == ownGrantID {
				tombstoneOwn = true
			}
			continue
		}
		return p.ClientTest.GrantID
	}
	if tombstoneOwn {
		return ownGrantID
	}
	return ""
}

func clientTestWriteWG(identity accessIdentity, generation string, packet []byte, keys *wgKeys, write func([]byte) error) error {
	if !clientTestCurrentGrantActive(identity, generation, time.Now().Unix()) {
		return errors.New("LEASE_EXPIRED")
	}
	if allowed, class, reason := clientWGDecision(identity, packet, keys); !allowed {
		return &wgMismatchError{packetClass: class, reason: reason}
	}
	return write(packet)
}
func clientWGRemember(identity accessIdentity, packet []byte) bool {
	g, _, managed := clientTestGrantFor(identity)
	if !managed {
		return true
	}
	if len(packet) == 92 && binary.LittleEndian.Uint32(packet[:4]) == 2 {
		index := binary.LittleEndian.Uint32(packet[4:8])
		clientWGIndices.Lock()
		defer clientWGIndices.Unlock()
		for k, v := range clientWGIndices.byIndex {
			if time.Now().After(v.expires) {
				delete(clientWGIndices.byIndex, k)
			}
		}
		if old, ok := clientWGIndices.byIndex[index]; ok && old.grant != g.GrantID {
			return false
		}
		if len(clientWGIndices.byIndex) >= 4096 {
			return false
		}
		clientWGIndices.byIndex[index] = clientWGIndex{g.GrantID, time.Now().Add(210 * time.Second)}
	}
	return true
}
