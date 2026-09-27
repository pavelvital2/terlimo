package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// A revoked/deactivated tombstone and the active re-provisioned grant share the same
// registration. Ownership must be decided by the active entry regardless of map order;
// the tombstone must never randomly deny (or own) the device.
func TestClientTestDeviceOwnedSkipsRevokedTombstone(t *testing.T) {
	previous := db
	defer func() { db = previous }()
	db = &Database{Passwords: map[string]*PasswordEntry{
		"old-pass": {
			ClientTest:    &ClientTestGrant{RegistrationID: "reg-dup", Revoked: true},
			IsDeactivated: true,
			DeviceID:      "reg-dup",
		},
		"new-pass": {
			ClientTest: &ClientTestGrant{RegistrationID: "reg-dup"},
			DeviceID:   "reg-dup",
		},
	}}
	for i := 0; i < 500; i++ {
		if !clientTestDeviceOwnedLocked("new-pass", "reg-dup") {
			t.Fatalf("iteration %d: active owner randomly denied by tombstone", i)
		}
		if clientTestDeviceOwnedLocked("old-pass", "reg-dup") {
			t.Fatalf("iteration %d: revoked tombstone owned the device", i)
		}
		if clientTestDeviceOwnedLocked("unrelated", "reg-dup") {
			t.Fatalf("iteration %d: unrelated password owned the active device", i)
		}
	}
	if !clientTestDeviceOwnedLocked("unrelated", "legacy-device") {
		t.Fatal("legacy/unknown registration boundary changed")
	}
	// Revoked-only registration keeps the previous fail-closed boundary: the revoked
	// owner matches, unrelated does not.
	db = &Database{Passwords: map[string]*PasswordEntry{
		"old-pass": {
			ClientTest:    &ClientTestGrant{RegistrationID: "reg-revoked", Revoked: true},
			IsDeactivated: true,
			DeviceID:      "reg-revoked",
		},
	}}
	if !clientTestDeviceOwnedLocked("old-pass", "reg-revoked") {
		t.Fatal("revoked-only owner boundary changed")
	}
	if clientTestDeviceOwnedLocked("unrelated", "reg-revoked") {
		t.Fatal("revoked-only unrelated must stay denied")
	}
}

func TestManagedDenyTraceAllowlistedNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	for _, class := range []string{
		"device_mismatch", "deactivated", "engine_ownership",
		"server_storage", "expired", "wrong_password", "not_found",
	} {
		buf.Reset()
		traceManagedDeny(&clientTestConn{trace: newManagedConnTrace()}, class)
		line := buf.String()
		if !strings.Contains(line, "stage=denied class="+class) {
			t.Fatalf("allowlisted class %q not traced verbatim: %q", class, line)
		}
	}

	buf.Reset()
	traceManagedDeny(&clientTestConn{trace: newManagedConnTrace()}, "reg=8fa209af993e256ed007daa2eb7f7ada6 pass=secret-value")
	line := buf.String()
	if !strings.Contains(line, "stage=denied class=other") {
		t.Fatalf("hostile class not collapsed to other: %q", line)
	}
	if strings.Contains(line, "8fa209af") || strings.Contains(line, "secret-value") {
		t.Fatalf("deny trace leaked identity material: %q", line)
	}

	for _, tc := range []struct{ stage, reason string }{
		{"config", "server_storage"},
		{"config", "noconf"},
		{"session_activate", "inactive"},
	} {
		buf.Reset()
		traceManagedEvent(&clientTestConn{trace: newManagedConnTrace()}, tc.stage, tc.reason)
		line := buf.String()
		if !strings.Contains(line, "stage="+tc.stage+" class="+tc.reason) {
			t.Fatalf("quiet-return marker %s/%s missing: %q", tc.stage, tc.reason, line)
		}
	}
}
