package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bootstrapTombstoneFixture(t *testing.T) string {
	t.Helper()
	_, _, _ = managedFixture(t)
	db.ClientBootstrap = map[string]ClientTestBootstrap{}
	return t.TempDir()
}

func bootstrapRaw(t *testing.T, operation, credentialID, secret string, expiresAt int64) []byte {
	t.Helper()
	raw, err := json.Marshal(clientTestCommand{Operation: operation, Bootstrap: ClientTestBootstrap{CredentialID: credentialID, Secret: secret, ExpiresAt: expiresAt}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClientTestBootstrapUnknownRevokeTombstoneIsDurableAndIdempotent(t *testing.T) {
	dir := bootstrapTombstoneFixture(t)
	raw := bootstrapRaw(t, "bootstrap_revoke", "tombstone-1", strings.Repeat("r", 40), 0)
	result, err := clientTestAdmin(dir, raw, nil)
	if err != nil {
		t.Fatal("unknown revoke must be acknowledged only after a durable save", err)
	}
	if result["revoked"] != true || result["credential_id"] != "tombstone-1" {
		t.Fatal(result)
	}
	if _, err = clientTestAdmin(dir, raw, nil); err != nil {
		t.Fatal("repeated unknown revoke must stay idempotent", err)
	}
	loaded, err := loadDatabaseFile(filepath.Join(dir, "passwords.json"))
	if err != nil {
		t.Fatal(err)
	}
	tomb, ok := loaded.ClientBootstrap["tombstone-1"]
	if !ok || !tomb.Revoked {
		t.Fatal("tombstone not persisted", loaded.ClientBootstrap)
	}
	if tomb.Secret != "" {
		t.Fatal("tombstone must not store a plaintext secret")
	}
	if len(loaded.ClientBootstrap) != 1 {
		t.Fatal("repeated revoke created duplicate entries", loaded.ClientBootstrap)
	}
}

func TestClientTestBootstrapTombstoneBlocksLateProvision(t *testing.T) {
	dir := bootstrapTombstoneFixture(t)
	if _, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_revoke", "tombstone-2", "", 0), nil); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute).Unix()
	for name, secret := range map[string]string{"same_as_revoke_request": strings.Repeat("r", 40), "different": strings.Repeat("s", 40)} {
		_, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_provision", "tombstone-2", secret, future), nil)
		if err == nil || err.Error() != "CONFLICT" {
			t.Fatalf("%s secret must not resurrect a tombstoned credential: %v", name, err)
		}
	}
	loaded, err := loadDatabaseFile(filepath.Join(dir, "passwords.json"))
	if err != nil {
		t.Fatal(err)
	}
	if tomb := loaded.ClientBootstrap["tombstone-2"]; !tomb.Revoked || tomb.Secret != "" {
		t.Fatal("tombstone was overwritten by a provision attempt", tomb)
	}
	other, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_provision", "fresh-1", strings.Repeat("t", 40), future), nil)
	if err != nil {
		t.Fatal("unrelated credential must not be blocked by a tombstone", err)
	}
	if other["revoked"] != false {
		t.Fatal(other)
	}
}

func TestClientTestBootstrapTombstoneSurvivesReload(t *testing.T) {
	dir := bootstrapTombstoneFixture(t)
	raw := bootstrapRaw(t, "bootstrap_revoke", "tombstone-3", "", 0)
	if _, err := clientTestAdmin(dir, raw, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDatabaseFile(filepath.Join(dir, "passwords.json"))
	if err != nil {
		t.Fatal(err)
	}
	dbMutex.Lock()
	previous := db
	db = loaded
	dbMutex.Unlock()
	t.Cleanup(func() {
		dbMutex.Lock()
		db = previous
		dbMutex.Unlock()
	})
	if _, err = clientTestAdmin(dir, raw, nil); err != nil {
		t.Fatal("repeated revoke after reload must stay idempotent", err)
	}
	_, err = clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_provision", "tombstone-3", strings.Repeat("u", 40), time.Now().Add(time.Minute).Unix()), nil)
	if err == nil || err.Error() != "CONFLICT" {
		t.Fatalf("reload dropped the revoke fence: %v", err)
	}
}

func TestClientTestBootstrapKnownRevokeAndExpiryUnchanged(t *testing.T) {
	dir := bootstrapTombstoneFixture(t)
	future := time.Now().Add(time.Minute).Unix()
	secret := strings.Repeat("v", 40)
	if _, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_provision", "known-1", secret, future), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_revoke", "known-1", "", 0), nil); err != nil {
		t.Fatal("known revoke must keep working", err)
	}
	for name, candidate := range map[string]string{"same": secret, "different": strings.Repeat("w", 40)} {
		if _, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_provision", "known-1", candidate, future), nil); err == nil || err.Error() != "CONFLICT" {
			t.Fatalf("revoked credential resurrected (%s secret): %v", name, err)
		}
	}
	if _, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_provision", "expired-1", strings.Repeat("x", 40), time.Now().Add(-time.Minute).Unix()), nil); err == nil || err.Error() != "BAD_MESSAGE" {
		t.Fatalf("expired provision accepted: %v", err)
	}
	if _, exists := db.ClientBootstrap["expired-1"]; exists {
		t.Fatal("expired provision created an entry")
	}
}

func TestClientTestBootstrapRevokeSaveFailureNoFalseACK(t *testing.T) {
	dir := bootstrapTombstoneFixture(t)
	if err := os.Mkdir(filepath.Join(dir, "passwords.json"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := clientTestAdmin(dir, bootstrapRaw(t, "bootstrap_revoke", "tombstone-4", "", 0), nil)
	if err == nil || err.Error() != "STORAGE_FAILED" {
		t.Fatalf("save failure must not be acknowledged: %v", err)
	}
	if _, exists := db.ClientBootstrap["tombstone-4"]; exists {
		t.Fatal("failed save left an in-memory tombstone behind")
	}
}
