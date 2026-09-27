package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"wg-turn-client/internal/wlwire"
)

var clientTestBootID = newClientTestBootID()

var clientTestBootStartedAt = time.Now().Unix()

var clientTestUsageSequence int64

func newClientTestBootID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("boot-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

// ClientTestGrant is opt-in metadata. Absence retains the upstream legacy contract.
type ClientTestGrant struct {
	GrantID         string `json:"grant_id"`
	RegistrationID  string `json:"registration_id"`
	NodeID          string `json:"node_id"`
	PublicKey       string `json:"public_key_spki"`
	Generation      string `json:"generation"`
	LeaseSeq        string `json:"lease_seq"`
	Revoked         bool   `json:"revoked"`
	OperationID     string `json:"operation_id"`
	OperationDigest string `json:"operation_digest"`
}
type ClientTestBootstrap struct {
	CredentialID string `json:"credential_id"`
	Secret       string `json:"secret"`
	ExpiresAt    int64  `json:"expires_at"`
	Revoked      bool   `json:"revoked"`
}
type clientTestCommand struct {
	Operation   string              `json:"operation"`
	Password    string              `json:"password"`
	Grant       ClientTestGrant     `json:"grant"`
	Bootstrap   ClientTestBootstrap `json:"bootstrap"`
	ExpiresAt   int64               `json:"expires_at"`
	ExpectedSeq string              `json:"expected_seq"`
}

// Managed TEST access uses zero as the explicit non-expiring value. Revocation,
// generation and seat ownership remain independent admission fences.
func clientTestExpiryActive(expiresAt, nowUnix int64) bool {
	return expiresAt == 0 || expiresAt > nowUnix
}

func clientVersion(s string) (uint64, error) {
	n, e := strconv.ParseUint(s, 10, 64)
	if e != nil || n == 0 || strconv.FormatUint(n, 10) != s {
		return 0, errors.New("BAD_VERSION")
	}
	return n, nil
}

// Read once at process initialization: admin requests cannot change node ownership.
var clientTestNodeID = os.Getenv("WL_TEST_NODE_ID")

// clientTestVKHashes exposes only the node-level normalized vk hash list (profile), never the
// whole admin profile and never secrets. Empty means "no hashes configured".
func clientTestVKHashes() []string {
	raw := strings.TrimSpace(db.AdminProfile.VkHashes)
	hashes := []string{}
	if raw == "" {
		return hashes
	}
	for _, item := range strings.Split(raw, ",") {
		if hash := strings.TrimSpace(item); hash != "" {
			hashes = append(hashes, hash)
		}
	}
	return hashes
}

func validateClientTestNodeID() error {
	if clientTestNodeID == "" || strings.TrimSpace(clientTestNodeID) != clientTestNodeID || len(clientTestNodeID) > 128 {
		return errors.New("NODE_ID_UNCONFIGURED")
	}
	return nil
}

// Reject mixed/restored foreign managed state before startup can publish keys
// or persist configuration; tombstones are retained, never silently rewritten.
func validateClientTestNodeState(loaded *Database) error {
	if err := validateClientTestNodeID(); err != nil {
		return err
	}
	for _, entry := range loaded.Passwords {
		if entry != nil && entry.ClientTest != nil && entry.ClientTest.NodeID != clientTestNodeID {
			return errors.New("NODE_ID_MISMATCH")
		}
	}
	return nil
}

// Called under dbMutex by the existing authenticated admin socket only.
func clientTestAdmin(configDir string, raw []byte, wgDev wgDevice) (map[string]any, error) {
	if os.Getenv("WL_TEST_ENABLED") != "1" {
		return nil, errors.New("TEST_DISABLED")
	}
	var c clientTestCommand
	if wlwire.StrictJSON(raw, &c) != nil {
		return nil, errors.New("BAD_MESSAGE")
	}
	if c.Operation != "bootstrap_provision" && c.Operation != "bootstrap_revoke" {
		if err := validateClientTestNodeID(); err != nil {
			return nil, err
		}
	}
	if c.Operation == "engine_status" {
		r, ok := wgDev.(*engineRouter)
		if !ok {
			return nil, errors.New("MANAGED_ENGINE_UNAVAILABLE")
		}
		return map[string]any{"node_id": clientTestNodeID, "managed_ready": r.managed != nil && r.keys != nil, "legacy_engine": r.legacy != nil, "managed_endpoint": managedEndpoint, "max_workers": configuredAccessWorkerLimit(), "issuance_gate": os.Getenv("WL_TEST_WG_ISOLATION_CONFIRMED") == "1", "vk_hashes": clientTestVKHashes()}, nil
	}
	if c.Operation == "usage" {
		r, ok := wgDev.(*engineRouter)
		if !ok {
			return nil, errors.New("MANAGED_ENGINE_UNAVAILABLE")
		}
		peers, supported, err := r.PeerUsage()
		if err != nil {
			return nil, errors.New("USAGE_UNAVAILABLE")
		}
		return map[string]any{
			"node_id":            clientTestNodeID,
			"boot_id":            clientTestBootID,
			"boot_started_at":    clientTestBootStartedAt,
			"sequence":           atomic.AddInt64(&clientTestUsageSequence, 1),
			"observed_at":        time.Now().Unix(),
			"counters_supported": supported,
			"peers":              peers,
		}, nil
	}
	if c.Operation == "bootstrap_provision" || c.Operation == "bootstrap_revoke" {
		b := c.Bootstrap
		if b.CredentialID == "" || len(b.CredentialID) > 128 {
			return nil, errors.New("BAD_MESSAGE")
		}
		if db.ClientBootstrap == nil {
			db.ClientBootstrap = map[string]ClientTestBootstrap{}
		}
		old, exists := db.ClientBootstrap[b.CredentialID]
		if c.Operation == "bootstrap_revoke" {
			if exists {
				b = old
				b.Revoked = true
			} else {
				// Durable tombstone for an unknown credential: a delayed
				// provision of the same credential_id must never resurrect it,
				// even after a worker/DB session loss. No plaintext secret is
				// stored. Tombstones are never GC'd until a proven safe replay
				// bound exists; they also never enter wrap keys (Revoked).
				b = ClientTestBootstrap{CredentialID: b.CredentialID, Revoked: true}
			}
		} else {
			if len(b.Secret) < 32 || !clientTestExpiryActive(b.ExpiresAt, time.Now().Unix()) || b.Revoked {
				return nil, errors.New("BAD_MESSAGE")
			}
			if exists && (old.Secret != b.Secret || old.Revoked) {
				return nil, errors.New("CONFLICT")
			}
			// Fail closed before persisting: a secret colliding with the public service
			// classifier would otherwise be saved while its wrap key is refused.
			if serverWrapKeys.ServiceCollision(b.Secret) {
				return nil, errors.New("CONFLICT")
			}
		}
		db.ClientBootstrap[b.CredentialID] = b
		if e := saveAdminDB(configDir, db); e != nil {
			if exists {
				db.ClientBootstrap[b.CredentialID] = old
			} else {
				delete(db.ClientBootstrap, b.CredentialID)
			}
			return nil, errors.New("STORAGE_FAILED")
		}
		if e := refreshWrapKeysFromDBLocked(); e != nil {
			return nil, e
		}
		return map[string]any{"credential_id": b.CredentialID, "revoked": b.Revoked, "expires_at": b.ExpiresAt, "scope": "bootstrap"}, nil
	}
	if c.Password == "" || c.Password == db.MainPassword {
		return nil, errors.New("BAD_MESSAGE")
	}
	// Reads may omit the node in the existing wire shape. All mutations must
	// explicitly target this node, including exact retries and revoke tombstones.
	if (c.Operation != "grant_get" || c.Grant.NodeID != "") && c.Grant.NodeID != clientTestNodeID {
		return nil, errors.New("NODE_ID_MISMATCH")
	}
	entry := db.Passwords[c.Password]
	if entry != nil && entry.ClientTest != nil && entry.ClientTest.NodeID != clientTestNodeID {
		return nil, errors.New("NODE_ID_MISMATCH")
	}
	if c.Operation == "grant_get" {
		if entry == nil || entry.ClientTest == nil {
			return nil, errors.New("NOT_FOUND")
		}
		return clientTestReadback(entry), nil
	}
	if c.Grant.GrantID == "" || c.Grant.RegistrationID == "" || c.Grant.NodeID == "" || c.Grant.OperationID == "" {
		return nil, errors.New("BAD_MESSAGE")
	}
	gen, e := clientVersion(c.Grant.Generation)
	if e != nil {
		return nil, e
	}
	seq, e := clientVersion(c.Grant.LeaseSeq)
	if e != nil {
		return nil, e
	}
	key, e := decodeClientB64(c.Grant.PublicKey)
	if e != nil {
		return nil, e
	}
	if _, e = wlwire.PublicKey(key); e != nil {
		return nil, e
	}
	// No managed VPN may be enabled before the separately authorised TEST isolation readback.
	if c.Operation == "grant_provision" && os.Getenv("WL_TEST_WG_ISOLATION_CONFIRMED") != "1" {
		return nil, errors.New("WG_ISOLATION_UNCONFIRMED")
	}
	if c.Operation == "grant_provision" && configuredAccessWorkerLimit() < 1 {
		return nil, errors.New("WORKER_POLICY_UNCONFIRMED")
	}
	digest := sha256.Sum256(raw)
	dh := hex.EncodeToString(digest[:])
	if entry != nil && entry.ClientTest != nil && entry.ClientTest.OperationID == c.Grant.OperationID {
		if entry.ClientTest.OperationDigest != dh {
			return nil, errors.New("IDEMPOTENCY_CONFLICT")
		}
		if err := clientTestApplyRuntime(entry, c.Password, wgDev); err != nil {
			return nil, err
		}
		return clientTestReadback(entry), nil
	}
	if c.Operation == "grant_provision" || (c.Operation == "grant_revoke" && entry == nil) {
		if c.Operation == "grant_provision" && (entry != nil || gen != 1 || seq != 1 || c.Grant.Revoked || !clientTestExpiryActive(c.ExpiresAt, time.Now().Unix())) {
			return nil, errors.New("CONFLICT")
		}
		if c.Operation == "grant_revoke" && (gen != 2 || !c.Grant.Revoked) {
			return nil, errors.New("CONFLICT")
		}
		if c.Operation == "grant_revoke" && clientTestUnprovenOwner(c.Grant.RegistrationID, c.Grant.GrantID) {
			// An unknown revoke must never claim a legacy/unknown or live foreign owner.
			return nil, errors.New("CONFLICT")
		}
		for _, p := range db.Passwords {
			if p != nil && p.ClientTest != nil && p.ClientTest.GrantID == c.Grant.GrantID {
				// The same grant identity is never provisioned twice.
				return nil, errors.New("CONFLICT")
			}
		}
		if c.Operation == "grant_provision" && !clientTestRegistrationClearForNewGrant(c.Grant.RegistrationID) {
			// Distinct new grant on an installation fingerprint whose previous owner is not yet
			// confirmed-revoked/removed (including after restart when the volatile confirmation
			// was lost): fail closed and let the reconcile confirm first.
			if len(clientTestRegistrationOwners(c.Grant.RegistrationID)) > 0 {
				return nil, errors.New("REGISTRATION_BUSY")
			}
		}
		if len(db.Passwords) >= db.MaxPasswords && db.MaxPasswords > 0 {
			return nil, errors.New("CAPACITY_FULL")
		}
		entry = &PasswordEntry{DeviceID: c.Grant.RegistrationID, ExpiresAt: c.ExpiresAt, ClientTest: &c.Grant, IsDeactivated: c.Grant.Revoked}
	} else {
		if entry == nil || entry.ClientTest == nil {
			return nil, errors.New("NOT_FOUND")
		}
		g := entry.ClientTest
		if g.GrantID != c.Grant.GrantID || g.RegistrationID != c.Grant.RegistrationID || g.PublicKey != c.Grant.PublicKey || g.NodeID != c.Grant.NodeID {
			return nil, errors.New("GRANT_CONFLICT")
		}
		oldGen, _ := clientVersion(g.Generation)
		oldSeq, _ := clientVersion(g.LeaseSeq)
		switch c.Operation {
		case "refresh_lease":
			if g.Revoked || gen != oldGen || seq != oldSeq+1 || c.ExpectedSeq != g.LeaseSeq || c.Grant.Revoked || !clientTestExpiryActive(c.ExpiresAt, time.Now().Unix()) {
				return nil, errors.New("LEASE_CONFLICT")
			}
		case "grant_revoke":
			if gen != oldGen+1 || seq > oldSeq || !c.Grant.Revoked {
				return nil, errors.New("LEASE_CONFLICT")
			}
			// Revoke dominates a refresh whose ACK was lost: fence generation and
			// retain the node's actual sequence/expiry instead of trusting stale DB readback.
			c.Grant.LeaseSeq = g.LeaseSeq
			c.ExpiresAt = entry.ExpiresAt
		default:
			return nil, errors.New("BAD_OPERATION")
		}
		copyEntry := *entry
		entry = &copyEntry
		entry.ClientTest = &c.Grant
		entry.ExpiresAt = c.ExpiresAt
		entry.IsDeactivated = c.Grant.Revoked
	}
	entry.ClientTest.OperationDigest = dh
	previous := db.Passwords[c.Password]
	db.Passwords[c.Password] = entry
	if e = saveAdminDB(configDir, db); e != nil {
		if previous == nil {
			delete(db.Passwords, c.Password)
		} else {
			db.Passwords[c.Password] = previous
		}
		return nil, errors.New("STORAGE_FAILED")
	}
	if e = clientTestApplyRuntime(entry, c.Password, wgDev); e != nil {
		return nil, e
	}
	return clientTestReadback(entry), nil
}

// Protected by dbMutex, just like the authoritative grants. Absence after restart
// means peer removal is unconfirmed until an idempotent revoke is reconciled.
var clientTestPeerRemoved = map[string]bool{}

// clientTestRegistrationOwners returns every stored owner of one registration identity. The
// map iteration order is irrelevant: callers examine all owners, never the first match.
func clientTestRegistrationOwners(registrationID string) []*PasswordEntry {
	owners := make([]*PasswordEntry, 0, 2)
	for _, p := range db.Passwords {
		if p != nil && p.DeviceID == registrationID {
			owners = append(owners, p)
		}
	}
	return owners
}

// clientTestActiveOwnerDifferent reports whether a live grant other than this one currently
// owns the registration (an active/expiry-active, non-revoked entry with a different grant id).
func clientTestActiveOwnerDifferent(registrationID, grantID string) bool {
	for _, p := range clientTestRegistrationOwners(registrationID) {
		if p.ClientTest == nil || p.ClientTest.Revoked || p.ClientTest.GrantID == grantID {
			continue
		}
		if clientTestExpiryActive(p.ExpiresAt, time.Now().Unix()) {
			return true
		}
	}
	return false
}

// clientTestRegistrationClearForNewGrant allows a new distinct grant on a registration only
// when every previous owner is revoked AND its runtime peer removal is confirmed. The removal
// confirmation is volatile: after a restart it is unset, so a new grant fails closed until the
// reconcile re-confirms the revoke.
func clientTestRegistrationClearForNewGrant(registrationID string) bool {
	owners := clientTestRegistrationOwners(registrationID)
	if len(owners) == 0 {
		return true
	}
	for _, p := range owners {
		if p.ClientTest == nil || !p.ClientTest.Revoked {
			return false
		}
		if !clientTestPeerRemoved[p.ClientTest.GrantID] {
			return false
		}
	}
	return true
}

// clientTestLegacyOwner reports an owner entry with no grant identity (legacy/unknown). Such a
// peer must never be removed on an unproven claim.
func clientTestLegacyOwner(registrationID string) bool {
	for _, p := range clientTestRegistrationOwners(registrationID) {
		if p.ClientTest == nil {
			return true
		}
	}
	return false
}

// clientTestUnprovenOwner is used only for an unknown revoke (no entry for the presented
// credential): it fails closed for a legacy/unknown entry or a different live grant.
func clientTestUnprovenOwner(registrationID, grantID string) bool {
	for _, p := range clientTestRegistrationOwners(registrationID) {
		if p.ClientTest == nil {
			return true
		}
		if p.ClientTest.GrantID != grantID && !p.ClientTest.Revoked {
			return true
		}
	}
	return false
}

func clientTestRemovePeer(wgDev wgDevice, g ClientTestGrant) error {
	if clientTestLegacyOwner(g.RegistrationID) {
		// Legacy/unknown owner: never remove its peer without a proven grant right.
		clientTestPeerRemoved[g.GrantID] = false
		return errors.New("PEER_OWNER_UNPROVEN")
	}
	if clientTestActiveOwnerDifferent(g.RegistrationID, g.GrantID) {
		// A known newer/live grant now owns the registration: the old revoke is an idempotent
		// no-op (runtime_applied stays true), and no WG peer is removed.
		clientTestPeerRemoved[g.GrantID] = true
		return nil
	}
	dev := db.Devices[g.RegistrationID]
	if dev != nil && dev.PubKey != "" {
		pubHex, err := b64ToHex(dev.PubKey)
		if err != nil || wgDev == nil {
			return errors.New("WG_REMOVE_UNCONFIRMED")
		}
		if err = wgDev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", pubHex)); err != nil {
			clientTestPeerRemoved[g.GrantID] = false
			return errors.New("WG_REMOVE_UNCONFIRMED")
		}
	}
	clientTestPeerRemoved[g.GrantID] = true
	return nil
}
func clientTestApplyRuntime(entry *PasswordEntry, password string, wgDev wgDevice) error {
	if entry.IsDeactivated || !clientTestExpiryActive(entry.ExpiresAt, time.Now().Unix()) {
		serverWrapKeys.RemovePassword(password)
		clientTestCloseGrant(entry.ClientTest.GrantID)
		return clientTestRemovePeer(wgDev, *entry.ClientTest)
	}
	delete(clientTestPeerRemoved, entry.ClientTest.GrantID)
	return serverWrapKeys.AddPassword(password)
}

func clientTestReadback(e *PasswordEntry) map[string]any {
	g := e.ClientTest
	k, _ := decodeClientB64(g.PublicKey)
	h := sha256.Sum256(k)
	return map[string]any{"grant_id": g.GrantID, "registration_id": g.RegistrationID, "node_id": g.NodeID, "key_fingerprint": hex.EncodeToString(h[:]), "generation": g.Generation, "lease_seq": g.LeaseSeq, "expires_at": e.ExpiresAt, "revoked": g.Revoked, "runtime_applied": (!g.Revoked && clientTestExpiryActive(e.ExpiresAt, time.Now().Unix())) || clientTestPeerRemoved[g.GrantID], "max_workers": configuredAccessWorkerLimit(), "active_workers": clientTestWorkerCount(g.GrantID)}
}
func clientTestBootstrapFor(identity accessIdentity) (ClientTestBootstrap, bool) {
	dbMutex.Lock()
	defer dbMutex.Unlock()
	for _, b := range db.ClientBootstrap {
		if b.Secret == identity.password {
			return b, true
		}
	}
	return ClientTestBootstrap{}, false
}
func clientTestGrantFor(identity accessIdentity) (ClientTestGrant, int64, bool) {
	dbMutex.Lock()
	defer dbMutex.Unlock()
	e := db.Passwords[identity.password]
	if e == nil || e.ClientTest == nil {
		return ClientTestGrant{}, 0, false
	}
	return *e.ClientTest, e.ExpiresAt, true
}

// Check the authoritative managed grant immediately before a packet leaves the
// server. Legacy connections have no managed generation and retain their path.
func clientTestCurrentGrantActive(identity accessIdentity, generation string, nowUnix int64) bool {
	if generation == "" {
		return true
	}
	g, expiry, found := clientTestGrantFor(identity)
	return found && !g.Revoked && g.Generation == generation && clientTestExpiryActive(expiry, nowUnix)
}
func decodeClientB64(s string) ([]byte, error) { return wlwire.Decode(s, len(s)*6/8) }
func clientTestProtectedLegacyCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "list", "details", "backup-status", "backup-list":
		return false
	}
	for _, a := range args[1:] {
		if e := db.Passwords[strings.TrimSpace(a)]; e != nil && e.ClientTest != nil {
			return true
		}
	}
	return false
}

// Prevent a legacy/main GETCONF from claiming a managed registration UUID.
// Called with dbMutex held before any binding or device metadata mutation.
func clientTestDeviceOwnedLocked(password, deviceID string) bool {
	// Revoked/deactivated entries are durable tombstones for the same registration:
	// they must never decide ownership, or map iteration order would randomly deny
	// the active re-provisioned grant. Active owners win; tombstones only preserve
	// the revoked-only boundary (owner matches, unrelated never does).
	activeMatch, activeOther, tombstoneOwner, matched := false, false, false, false
	for owner, entry := range db.Passwords {
		if entry == nil || entry.ClientTest == nil || entry.ClientTest.RegistrationID != deviceID {
			continue
		}
		matched = true
		if entry.ClientTest.Revoked || entry.IsDeactivated {
			if owner == password {
				tombstoneOwner = true
			}
			continue
		}
		if owner == password {
			activeMatch = true
		} else {
			activeOther = true
		}
	}
	if activeMatch {
		return true
	}
	if activeOther {
		return false
	}
	if matched {
		return tombstoneOwner
	}
	return true
}
