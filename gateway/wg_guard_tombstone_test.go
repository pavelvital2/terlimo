package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// Live re-provision and its revoked/deactivated tombstone share one registration device key.
// The guard must resolve the key to the non-tombstone owner regardless of map order, and must
// reject tombstone-only or unknown keys. Helper-level: no packet crypto needed.
func TestClientWGMatchedGrantIgnoresRevokedTombstone(t *testing.T) {
	previous := db
	defer func() { db = previous }()
	key := bytes.Repeat([]byte{7}, 32)
	device := func() map[string]*ClientDevice {
		return map[string]*ClientDevice{
			"reg-wg": {DeviceID: "reg-wg", PubKey: base64.StdEncoding.EncodeToString(key), IP: "10.67.67.9"},
		}
	}
	tombstone := &PasswordEntry{
		ClientTest:    &ClientTestGrant{RegistrationID: "reg-wg", GrantID: "old-grant", Revoked: true},
		IsDeactivated: true,
		DeviceID:      "reg-wg",
	}
	active := &PasswordEntry{
		ClientTest: &ClientTestGrant{RegistrationID: "reg-wg", GrantID: "new-grant"},
		DeviceID:   "reg-wg",
	}

	db = &Database{Passwords: map[string]*PasswordEntry{"old-pass": tombstone, "new-pass": active}, Devices: device()}
	for i := 0; i < 1000; i++ {
		if got := clientWGMatchedGrantLocked(key, "new-grant"); got != "new-grant" {
			t.Fatalf("iteration %d: matched %q, want active new-grant", i, got)
		}
	}

	// Revoked-only tombstone with a different own grant: no match (reject).
	db = &Database{Passwords: map[string]*PasswordEntry{"old-pass": tombstone}, Devices: device()}
	if got := clientWGMatchedGrantLocked(key, "new-grant"); got != "" {
		t.Fatalf("revoked-only tombstone matched %q, want no match", got)
	}
	// Own revoked tombstone keeps the existing revoked classification path.
	if got := clientWGMatchedGrantLocked(key, "old-grant"); got != "old-grant" {
		t.Fatalf("own revoked tombstone matched %q, want old-grant", got)
	}
	// Deactivated-only variant (expired/superseded entry swept to tombstone).
	tombstone.ClientTest.Revoked = false
	if got := clientWGMatchedGrantLocked(key, "new-grant"); got != "" {
		t.Fatalf("deactivated-only tombstone matched %q, want no match", got)
	}
	// Unknown key.
	if got := clientWGMatchedGrantLocked(bytes.Repeat([]byte{9}, 32), "new-grant"); got != "" {
		t.Fatalf("unknown key matched %q, want no match", got)
	}
	// Active record whose device is missing cannot own a key.
	db = &Database{
		Passwords: map[string]*PasswordEntry{
			"new-pass": {ClientTest: &ClientTestGrant{RegistrationID: "missing-device", GrantID: "new-grant"}, DeviceID: "missing-device"},
		},
		Devices: device(),
	}
	if got := clientWGMatchedGrantLocked(key, "new-grant"); got != "" {
		t.Fatalf("missing device matched %q, want no match", got)
	}
}
