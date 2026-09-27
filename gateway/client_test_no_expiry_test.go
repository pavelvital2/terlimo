package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedNoExpirySurvivesCleanup(t *testing.T) {
	identity, _, _ := managedFixture(t)
	entry := db.Passwords[identity.password]
	entry.ExpiresAt = 0
	if isPasswordExpiredAt(entry, time.Now().Unix()) {
		t.Fatal("zero expiry must be non-expiring")
	}
	if removed := cleanupExpiredPasswordsLockedAt(nil, time.Now().Add(24*time.Hour).Unix()); removed != 0 {
		t.Fatalf("managed no-expiry record removed: %d", removed)
	}
	if db.Passwords[identity.password] != entry {
		t.Fatal("managed no-expiry record changed during cleanup")
	}
}

func TestManagedExpiredRestartThenNoExpiryRefresh(t *testing.T) {
	identity, grant, _ := managedFixture(t)
	dir := t.TempDir()
	dbFile = filepath.Join(dir, "passwords.json")
	db.Passwords[identity.password].ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if removed := cleanupExpiredPasswordsLockedAt(nil, time.Now().Unix()); removed != 0 {
		t.Fatalf("recoverable managed record removed at restart: %d", removed)
	}
	grant.LeaseSeq = "2"
	grant.OperationID = "refresh-no-expiry"
	raw, _ := json.Marshal(clientTestCommand{
		Operation: "refresh_lease", Password: identity.password, Grant: grant,
		ExpectedSeq: "1", ExpiresAt: 0,
	})
	if _, err := clientTestAdmin(dir, raw, nil); err != nil {
		t.Fatalf("no-expiry refresh: %v", err)
	}
	entry := db.Passwords[identity.password]
	if entry == nil || entry.ExpiresAt != 0 || entry.ClientTest.LeaseSeq != "2" || entry.ClientTest.Revoked {
		t.Fatalf("unexpected refreshed state: %#v", entry)
	}
}

func TestManagedNoExpiryRevocationStillWins(t *testing.T) {
	identity, grant, _ := managedFixture(t)
	entry := db.Passwords[identity.password]
	entry.ExpiresAt = 0
	grant.Revoked = true
	entry.ClientTest = &grant
	entry.IsDeactivated = true
	if clientTestExpiryActive(entry.ExpiresAt, time.Now().Unix()) && !entry.IsDeactivated {
		t.Fatal("revoked no-expiry grant admitted")
	}
	readback := clientTestReadback(entry)
	if readback["runtime_applied"] == true {
		t.Fatal("revoked no-expiry grant reported active")
	}
}
