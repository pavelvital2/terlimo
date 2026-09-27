package main

import (
	"net"
	"testing"
	"time"
)

// Exercise the production wrapper with actual GETCONF bytes, beyond auth.
func TestManagedReadGETCONFExpiry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expiry     int64
		revoked    bool
		generation string
		wantError  bool
	}{
		{"expiry0", 0, false, "1", false},
		{"finite_valid", time.Now().Add(time.Hour).Unix(), false, "1", false},
		{"expired", time.Now().Add(-time.Hour).Unix(), false, "1", true},
		{"revoked_unlimited", 0, true, "1", true},
		{"revoked_finite", time.Now().Add(time.Hour).Unix(), true, "1", true},
		{"generation_mismatch", 0, false, "2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, grant, _ := managedFixture(t)
			db.Passwords[identity.password].ExpiresAt = tc.expiry
			db.Passwords[identity.password].ClientTest.Revoked = tc.revoked
			db.Passwords[identity.password].ClientTest.Generation = tc.generation
			server, client := net.Pipe()
			defer client.Close()
			c := &clientTestConn{Conn: server, identity: identity, grant: grant,
				auth: clientAuthPayload{Mode: "getconf", Session: "test-session"}, done: make(chan struct{})}
			defer c.Close()
			_ = server.SetDeadline(time.Now().Add(2 * time.Second))
			_ = client.SetDeadline(time.Now().Add(2 * time.Second))
			request := "GETCONF:9000|" + grant.RegistrationID + "|" + identity.password + "|test-client|test-session"
			written := make(chan error, 1)
			go func() { _, err := client.Write([]byte(request)); written <- err }()
			buf := make([]byte, 2048)
			n, err := c.Read(buf)
			if writeErr := <-written; writeErr != nil {
				t.Fatal(writeErr)
			}
			if tc.wantError {
				if err == nil || err.Error() != "LEASE_EXPIRED" || n != 0 {
					t.Fatalf("want LEASE_EXPIRED n=0, got n=%d err=%v", n, err)
				}
			} else if err != nil || string(buf[:n]) != request {
				t.Fatalf("GETCONF must reach handler intact: n=%d err=%v", n, err)
			}
			if !tc.wantError {
				response := buildClientConfig("test-server-public", "test-device-private", "10.0.0.2", "9000")
				go func() { _, err := c.Write([]byte(response)); written <- err }()
				n, err = client.Read(buf)
				if writeErr := <-written; writeErr != nil {
					t.Fatal(writeErr)
				}
				if err != nil || string(buf[:n]) != response {
					t.Fatalf("config response read failed: n=%d err=%v", n, err)
				}
				clientTestWorkers.Lock()
				ready := clientTestWorkers.sessions[grant.GrantID] == c.auth.Session
				clientTestWorkers.Unlock()
				if !c.configured || !ready {
					t.Fatal("config response did not activate authenticated session")
				}
			}
		})
	}
}
