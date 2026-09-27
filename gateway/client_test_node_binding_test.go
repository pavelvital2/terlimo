package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type nodeBindingWG struct{ calls []string }

func (w *nodeBindingWG) Close() {}
func (w *nodeBindingWG) IpcSet(command string) error {
	w.calls = append(w.calls, command)
	return nil
}

func useClientTestNodeID(t *testing.T, nodeID string) {
	t.Helper()
	previous := clientTestNodeID
	clientTestNodeID = nodeID
	t.Cleanup(func() { clientTestNodeID = previous })
}

func isolatedNodeBindingRuntime(t *testing.T) {
	t.Helper()
	previous := serverWrapKeys
	serverWrapKeys = newWrapKeyStore()
	t.Cleanup(func() { serverWrapKeys = previous })
}

func nodeBindingCommand(t *testing.T, command clientTestCommand) []byte {
	t.Helper()
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClientTestNodeBindingRejectsForeignCommandsBeforeEffects(t *testing.T) {
	identity, grant, _ := managedFixture(t)
	useClientTestNodeID(t, "node-a")
	isolatedNodeBindingRuntime(t)
	delete(db.Passwords, identity.password)
	dir := t.TempDir()
	wg := &nodeBindingWG{}
	grant.NodeID = "node-b"
	grant.OperationID = "foreign-provision"
	provision := nodeBindingCommand(t, clientTestCommand{Operation: "grant_provision", Password: identity.password, Grant: grant, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	revoke := grant
	revoke.Generation, revoke.Revoked, revoke.OperationID = "2", true, "foreign-revoke"

	for _, raw := range [][]byte{
		provision,
		provision, // exact retry
		nodeBindingCommand(t, clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: revoke}),
		nodeBindingCommand(t, clientTestCommand{Operation: "grant_get", Password: identity.password, Grant: ClientTestGrant{NodeID: "node-b"}}),
	} {
		if _, err := clientTestAdmin(dir, raw, wg); err == nil || err.Error() != "NODE_ID_MISMATCH" {
			t.Fatalf("foreign command error = %v", err)
		}
	}
	if len(db.Passwords) != 0 || len(wg.calls) != 0 || serverWrapKeys.Count() != 0 {
		t.Fatalf("foreign command had effects: passwords=%d wg=%d keys=%d", len(db.Passwords), len(wg.calls), serverWrapKeys.Count())
	}
	if _, err := os.Stat(filepath.Join(dir, "passwords.json")); !os.IsNotExist(err) {
		t.Fatalf("foreign command persisted a file: %v", err)
	}
}

func TestClientTestNodeBindingRejectsForeignPersistedStateBeforeInit(t *testing.T) {
	_, grant, _ := managedFixture(t)
	useClientTestNodeID(t, "node-a")
	isolatedNodeBindingRuntime(t)
	t.Setenv("WL_TEST_ENABLED", "1")
	oldDB, oldDBFile := db, dbFile
	t.Cleanup(func() { db, dbFile = oldDB, oldDBFile })

	dir := t.TempDir()
	grant.NodeID, grant.Generation, grant.Revoked = "node-b", "2", true
	stored := testDatabase("owner-secret", 0)
	stored.Passwords["foreign-tombstone"] = &PasswordEntry{DeviceID: grant.RegistrationID, ClientTest: &grant, IsDeactivated: true}
	path := filepath.Join(dir, "passwords.json")
	if err := persistDatabaseFile(path, stored); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := initDB(dir, "", "", "", ""); err == nil || err.Error() != "NODE_ID_MISMATCH" {
		t.Fatalf("foreign persisted state init error = %v", err)
	}
	if db != oldDB {
		t.Fatal("initDB replaced the active database after node mismatch")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("initDB rewrote foreign persisted state")
	}

	legacyDir := t.TempDir()
	legacy := testDatabase("owner-secret", 0)
	legacy.Passwords["legacy"] = &PasswordEntry{DeviceID: "legacy-device"}
	if err := persistDatabaseFile(filepath.Join(legacyDir, "passwords.json"), legacy); err != nil {
		t.Fatal(err)
	}
	if err := initDB(legacyDir, "", "", "", ""); err != nil {
		t.Fatalf("legacy persisted entry rejected: %v", err)
	}
}

func TestClientTestNodeBindingProvisionAndRevokeRetriesWithFences(t *testing.T) {
	identity, grant, _ := managedFixture(t)
	useClientTestNodeID(t, "node-a")
	isolatedNodeBindingRuntime(t)
	t.Setenv("WL_TEST_WG_ISOLATION_CONFIRMED", "1")
	delete(db.Passwords, identity.password)
	dir := t.TempDir()
	grant.NodeID, grant.OperationID = "node-a", "provision-1"
	provision := nodeBindingCommand(t, clientTestCommand{Operation: "grant_provision", Password: identity.password, Grant: grant, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if _, err := clientTestAdmin(dir, provision, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := clientTestAdmin(dir, provision, nil); err != nil {
		t.Fatalf("exact provision retry: %v", err)
	}
	for _, requestedNode := range []string{"", "node-a"} {
		readback, err := clientTestAdmin(dir, nodeBindingCommand(t, clientTestCommand{Operation: "grant_get", Password: identity.password, Grant: ClientTestGrant{NodeID: requestedNode}}), nil)
		if err != nil || readback["node_id"] != "node-a" {
			t.Fatalf("grant_get node %q = %#v, %v", requestedNode, readback, err)
		}
	}

	staleRefresh := grant
	staleRefresh.LeaseSeq, staleRefresh.OperationID = "2", "refresh-stale"
	if _, err := clientTestAdmin(dir, nodeBindingCommand(t, clientTestCommand{Operation: "refresh_lease", Password: identity.password, Grant: staleRefresh, ExpectedSeq: "0", ExpiresAt: time.Now().Add(time.Minute).Unix()}), nil); err == nil || err.Error() != "LEASE_CONFLICT" {
		t.Fatalf("stale sequence error = %v", err)
	}
	staleRevoke := grant
	staleRevoke.Revoked, staleRevoke.OperationID = true, "revoke-stale-generation"
	if _, err := clientTestAdmin(dir, nodeBindingCommand(t, clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: staleRevoke}), nil); err == nil || err.Error() != "LEASE_CONFLICT" {
		t.Fatalf("stale generation error = %v", err)
	}

	revoke := grant
	revoke.Generation, revoke.Revoked, revoke.OperationID = "2", true, "revoke-1"
	rawRevoke := nodeBindingCommand(t, clientTestCommand{Operation: "grant_revoke", Password: identity.password, Grant: revoke})
	if _, err := clientTestAdmin(dir, rawRevoke, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := clientTestAdmin(dir, rawRevoke, nil); err != nil {
		t.Fatalf("exact revoke retry: %v", err)
	}
	stored := db.Passwords[identity.password].ClientTest
	if !stored.Revoked || stored.Generation != "2" || stored.LeaseSeq != "1" || stored.NodeID != "node-a" {
		t.Fatalf("unexpected stored revoke: %#v", stored)
	}
}

func TestClientTestNodeBindingRejectsForeignPersistedGrantAndEmptyNode(t *testing.T) {
	identity, grant, _ := managedFixture(t)
	useClientTestNodeID(t, "node-a")
	isolatedNodeBindingRuntime(t)
	grant.NodeID = "node-b"
	db.Passwords[identity.password].ClientTest = &grant
	dir := t.TempDir()
	for _, command := range []clientTestCommand{
		{Operation: "grant_get", Password: identity.password},
		{Operation: "grant_get", Password: identity.password, Grant: ClientTestGrant{NodeID: "node-a"}},
		{Operation: "grant_revoke", Password: identity.password, Grant: ClientTestGrant{NodeID: "node-a"}},
	} {
		if _, err := clientTestAdmin(dir, nodeBindingCommand(t, command), nil); err == nil || err.Error() != "NODE_ID_MISMATCH" {
			t.Fatalf("foreign persisted grant error = %v", err)
		}
	}
	for _, nodeID := range []string{"", " node-a", "node-a ", string(make([]byte, 129))} {
		useClientTestNodeID(t, nodeID)
		if err := validateClientTestNodeID(); err == nil || err.Error() != "NODE_ID_UNCONFIGURED" {
			t.Fatalf("node %q validation = %v", nodeID, err)
		}
		for _, command := range []clientTestCommand{
			{Operation: "grant_get", Password: identity.password},
			{Operation: "grant_provision", Password: identity.password, Grant: ClientTestGrant{NodeID: "node-a"}},
		} {
			if _, err := clientTestAdmin(dir, nodeBindingCommand(t, command), nil); err == nil || err.Error() != "NODE_ID_UNCONFIGURED" {
				t.Fatalf("node %q admin error = %v", nodeID, err)
			}
		}
	}
}

func TestClientTestEngineStatusReportsOnlyNormalizedVKHashes(t *testing.T) {
	_, _, _ = managedFixture(t)
	useClientTestNodeID(t, "node-vk")
	previous := db.AdminProfile
	t.Cleanup(func() { db.AdminProfile = previous })
	db.AdminProfile.VkHashes = "hash-a, hash-b,,hash-a "
	result, err := clientTestAdmin(t.TempDir(), nodeBindingCommand(t, clientTestCommand{Operation: "engine_status"}), &engineRouter{keys: &wgKeys{}, managed: &nodeBindingWG{}})
	if err != nil {
		t.Fatalf("engine status: %v", err)
	}
	hashes, ok := result["vk_hashes"].([]string)
	if !ok || len(hashes) != 3 || hashes[0] != "hash-a" || hashes[1] != "hash-b" || hashes[2] != "hash-a" {
		t.Fatalf("vk_hashes = %#v, %v", result["vk_hashes"], err)
	}
	if _, leaked := result["admin_profile"]; leaked {
		t.Fatalf("engine status must not expose the admin profile: %#v", result)
	}
	db.AdminProfile.VkHashes = ""
	result, err = clientTestAdmin(t.TempDir(), nodeBindingCommand(t, clientTestCommand{Operation: "engine_status"}), &engineRouter{keys: &wgKeys{}, managed: &nodeBindingWG{}})
	if err != nil {
		t.Fatalf("engine status empty: %v", err)
	}
	if hashes, ok := result["vk_hashes"].([]string); !ok || len(hashes) != 0 {
		t.Fatalf("empty profile should report an empty list: %#v", result["vk_hashes"])
	}
}

func TestClientTestNodeBindingEngineStatusReportsConfiguredNode(t *testing.T) {
	_, _, _ = managedFixture(t)
	useClientTestNodeID(t, "node-a")
	result, err := clientTestAdmin(t.TempDir(), nodeBindingCommand(t, clientTestCommand{Operation: "engine_status"}), &engineRouter{keys: &wgKeys{}, managed: &nodeBindingWG{}})
	if err != nil || result["node_id"] != "node-a" {
		t.Fatalf("engine status = %#v, %v", result, err)
	}
}
