package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wg-turn-client/internal/wlwire"
)

// A live newer owner always protects its peer, regardless of map order, and a distinct new
// grant is only allowed after every previous owner is revoked AND removal-confirmed.
func TestClientTestRegistrationOwnershipIsDeterministicAndFailClosed(t *testing.T) {
	old := db
	db = &Database{Passwords: map[string]*PasswordEntry{
		"hour-pass": {DeviceID: "reg-1", ClientTest: &ClientTestGrant{RegistrationID: "reg-1", GrantID: "hour-grant", Revoked: true}},
		"paid-pass": {DeviceID: "reg-1", ClientTest: &ClientTestGrant{RegistrationID: "reg-1", GrantID: "paid-grant"}},
	}}
	t.Cleanup(func() { db = old; clientTestPeerRemoved = map[string]bool{} })

	if !clientTestActiveOwnerDifferent("reg-1", "hour-grant") {
		t.Fatal("live paid owner must block removal by a stale hour revoke")
	}
	if clientTestActiveOwnerDifferent("reg-1", "paid-grant") {
		t.Fatal("the live owner itself must be removable")
	}
	delete(db.Passwords, "paid-pass")
	clientTestPeerRemoved["hour-grant"] = false
	if clientTestRegistrationClearForNewGrant("reg-1") {
		t.Fatal("unconfirmed runtime removal must fail closed")
	}
	clientTestPeerRemoved["hour-grant"] = true
	if !clientTestRegistrationClearForNewGrant("reg-1") {
		t.Fatal("all owners revoked and removal-confirmed must permit a new distinct grant")
	}
	clientTestPeerRemoved = map[string]bool{}
	if clientTestRegistrationClearForNewGrant("reg-1") {
		t.Fatal("lost removal confirmation after restart must fail closed")
	}
}

func realWG(t *testing.T) *nodeBindingWG {
	t.Helper()
	return &nodeBindingWG{}
}

// Real admin path with a real fake WG device: hour provision->revoke (IpcSet remove), paid
// provision on the same fingerprint, replay of the exact saved old admin raw must not remove
// the paid peer.
func TestHourToPaidAdminPathOwnershipAndRestart(t *testing.T) {
	identity, hour, _ := managedFixture(t)
	t.Setenv("WL_TEST_WG_ISOLATION_CONFIRMED", "1")
	dir := t.TempDir()
	registration := hour.RegistrationID
	db.Devices[registration] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, 32))}
	wg := realWG(t)

	hourRevoke := hour
	hourRevoke.Generation, hourRevoke.Revoked, hourRevoke.OperationID = "2", true, "hour-revoke-1"
	rawHourRevoke, _ := json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: hourRevoke})
	if _, err := clientTestAdmin(dir, rawHourRevoke, wg); err != nil {
		t.Fatalf("hour revoke: %v", err)
	}
	if !clientTestPeerRemoved[hour.GrantID] {
		t.Fatal("confirmed hour revoke must mark its peer removed")
	}
	if len(wg.calls) == 0 || !strings.Contains(wg.calls[len(wg.calls)-1], "remove=true") {
		t.Fatalf("confirmed hour revoke must issue a real WG remove: %v", wg.calls)
	}
	removalsAfterHour := len(wg.calls)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	paid := ClientTestGrant{
		GrantID: "paid-grant", RegistrationID: registration, NodeID: "test-1",
		PublicKey: wlwire.Encode(spki), Generation: "1", LeaseSeq: "1", OperationID: "paid-prov-1",
	}
	rawPaid, _ := json.Marshal(clientTestCommand{
		Operation: "grant_provision", Password: "paid-pass", Grant: paid,
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if _, err := clientTestAdmin(dir, rawPaid, wg); err != nil {
		t.Fatalf("paid provision after confirmed hour revoke: %v", err)
	}

	// A delayed/reconcile hour revoke (new generation from the backend) arriving after the
	// paid grant must be a successful idempotent no-op: no WG removal, runtime_applied true.
	hourLate := hour
	hourLate.Generation, hourLate.Revoked, hourLate.OperationID = "3", true, "hour-revoke-2-late"
	rawLate, _ := json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: hourLate})
	result, err := clientTestAdmin(dir, rawLate, wg)
	if err != nil {
		t.Fatalf("late hour revoke must be an idempotent no-op, got %v", err)
	}
	if result["runtime_applied"] != true {
		t.Fatalf("runtime_applied lost on idempotent no-op: %v", result)
	}
	if _, ok := db.Passwords["paid-pass"]; !ok {
		t.Fatal("paid entry disappeared after late hour revoke")
	}
	if clientTestPeerRemoved["paid-grant"] {
		t.Fatal("paid peer marked removed by a late hour revoke")
	}
	if len(wg.calls) != removalsAfterHour {
		t.Fatalf("late hour revoke issued extra WG calls: %v", wg.calls)
	}
}

// Restart proof isolated to a revoked owner only (no active paid to mask it): volatile removal
// confirmation reset -> new distinct grant blocked; confirmed reconcile -> allowed.
func TestHourRevokedOwnerRestartRequiresReconcileBeforeNewGrant(t *testing.T) {
	identity, hour, _ := managedFixture(t)
	t.Setenv("WL_TEST_WG_ISOLATION_CONFIRMED", "1")
	dir := t.TempDir()
	registration := hour.RegistrationID
	db.Devices[registration] = &ClientDevice{PubKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32))}
	wg := realWG(t)
	hourRevoke := hour
	hourRevoke.Generation, hourRevoke.Revoked, hourRevoke.OperationID = "2", true, "hour-revoke-2"
	raw, _ := json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: hourRevoke})
	if _, err := clientTestAdmin(dir, raw, wg); err != nil {
		t.Fatal(err)
	}
	// Restart: volatile confirmation gone, the revoked owner (tombstone) is still stored.
	clientTestPeerRemoved = map[string]bool{}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	next := ClientTestGrant{GrantID: "next-grant", RegistrationID: registration, NodeID: "test-1", PublicKey: wlwire.Encode(spki), Generation: "1", LeaseSeq: "1", OperationID: "next-prov"}
	rawNext, _ := json.Marshal(clientTestCommand{Operation: "grant_provision", Password: "next-pass", Grant: next, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if _, err := clientTestAdmin(dir, rawNext, wg); err == nil || err.Error() != "REGISTRATION_BUSY" {
		t.Fatalf("lost confirmation must block a new grant, got %v", err)
	}
	// Production reconcile: a new revoke generation re-confirms the removal (not a direct map set).
	reconcile := hour
	reconcile.Generation, reconcile.Revoked, reconcile.OperationID = "3", true, "hour-reconcile-3"
	rawReconcile, _ := json.Marshal(clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: reconcile})
	if _, err := clientTestAdmin(dir, rawReconcile, wg); err != nil {
		t.Fatalf("reconcile revoke failed: %v", err)
	}
	if !clientTestPeerRemoved[hour.GrantID] {
		t.Fatal("reconcile must re-confirm the runtime removal")
	}
	if _, err := clientTestAdmin(dir, rawNext, wg); err != nil {
		t.Fatalf("confirmed reconcile must allow the new grant: %v", err)
	}
}

// Legacy/unknown owner (ClientTest == nil) must never be removed by an unproven claim.
func TestLegacyOwnerPeerIsNotRemovedWithoutProvenGrant(t *testing.T) {
	old := db
	db = &Database{
		Passwords: map[string]*PasswordEntry{
			"legacy-pass": {DeviceID: "reg-legacy"}, // no ClientTest: unknown/legacy owner
		},
		Devices: map[string]*ClientDevice{
			"reg-legacy": {PubKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, 32))},
		},
	}
	t.Cleanup(func() { db = old; clientTestPeerRemoved = map[string]bool{} })
	wg := realWG(t)
	if err := clientTestRemovePeer(wg, ClientTestGrant{RegistrationID: "reg-legacy", GrantID: "other"}); err == nil {
		t.Fatal("unproven removal of a legacy owner must fail closed")
	}
	if len(wg.calls) != 0 {
		t.Fatalf("legacy peer must not be touched: %v", wg.calls)
	}
	if clientTestUnprovenOwner("reg-legacy", "other") != true {
		t.Fatal("legacy owner must be reported as unproven")
	}
}
