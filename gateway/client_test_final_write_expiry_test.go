package main

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func setManagedGrantState(t *testing.T, identity accessIdentity, expiry int64, revoked bool, generation string) {
	t.Helper()
	dbMutex.Lock()
	defer dbMutex.Unlock()
	entry := db.Passwords[identity.password]
	entry.ExpiresAt = expiry
	entry.ClientTest.Revoked = revoked
	entry.ClientTest.Generation = generation
}

func TestManagedCurrentGrantFinalWritePredicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expiry     int64
		revoked    bool
		generation string
		want       bool
	}{
		{name: "finite_valid", expiry: time.Now().Add(time.Hour).Unix(), generation: "1", want: true},
		{name: "unlimited", expiry: 0, generation: "1", want: true},
		{name: "expired", expiry: time.Now().Add(-time.Hour).Unix(), generation: "1"},
		{name: "revoked", expiry: 0, revoked: true, generation: "1"},
		{name: "generation", expiry: 0, generation: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, grant, _ := managedFixture(t)
			setManagedGrantState(t, identity, tc.expiry, tc.revoked, tc.generation)
			if got := clientTestCurrentGrantActive(identity, grant.Generation, time.Now().Unix()); got != tc.want {
				t.Fatalf("active=%v want=%v", got, tc.want)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		identity, grant, _ := managedFixture(t)
		dbMutex.Lock()
		delete(db.Passwords, identity.password)
		dbMutex.Unlock()
		if clientTestCurrentGrantActive(identity, grant.Generation, time.Now().Unix()) {
			t.Fatal("missing grant accepted")
		}
	})
}

func TestManagedConnFinalWriteGrantBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expiry     int64
		revoked    bool
		generation string
		wantError  bool
	}{
		{name: "finite_valid", expiry: time.Now().Add(time.Hour).Unix(), generation: "1"},
		{name: "unlimited", expiry: 0, generation: "1"},
		{name: "expired", expiry: time.Now().Add(-time.Hour).Unix(), generation: "1", wantError: true},
		{name: "revoked", expiry: 0, revoked: true, generation: "1", wantError: true},
		{name: "generation", expiry: 0, generation: "2", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, grant, _ := managedFixture(t)
			setManagedGrantState(t, identity, tc.expiry, tc.revoked, tc.generation)
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			conn := &clientTestConn{Conn: server, identity: identity, grant: grant, done: make(chan struct{})}
			payload := []byte("final-packet")
			if !tc.wantError {
				received := make(chan string, 1)
				go func() {
					buf := make([]byte, len(payload))
					n, err := client.Read(buf)
					if err != nil {
						received <- "read-error"
						return
					}
					received <- string(buf[:n])
				}()
				n, err := conn.Write(payload)
				if err != nil || n != len(payload) || <-received != string(payload) {
					t.Fatalf("valid write n=%d err=%v", n, err)
				}
				return
			}
			n, err := conn.Write(payload)
			if n != 0 || err == nil || err.Error() != "LEASE_EXPIRED" {
				t.Fatalf("denied write n=%d err=%v", n, err)
			}
		})
	}
}

func TestManagedQueuedPacketExpiryAtBothFinalWrites(t *testing.T) {
	identity, grant, _ := managedFixture(t)
	setManagedGrantState(t, identity, time.Now().Add(time.Hour).Unix(), false, grant.Generation)

	response := make([]byte, 92)
	binary.LittleEndian.PutUint32(response, 2)
	binary.LittleEndian.PutUint32(response[4:], 1001)
	if !clientWGRemember(identity, response) {
		t.Fatal("receiver index setup")
	}
	uplink := make([]byte, 32)
	binary.LittleEndian.PutUint32(uplink, 4)
	binary.LittleEndian.PutUint32(uplink[4:], 1001)
	downlink := []byte("queued-downlink")

	// Exercise the production wrapper: the uplink packet passes Read while the
	// grant is valid, then waits behind the limiter/queue boundary.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	conn := &clientTestConn{trace: newManagedConnTrace(), Conn: server, identity: identity, grant: grant, configured: true, done: make(chan struct{})}
	writeDone := make(chan error, 1)
	go func() { _, err := client.Write(uplink); writeDone <- err }()
	buf := make([]byte, len(uplink))
	n, err := conn.Read(buf)
	if err != nil || n != len(uplink) {
		t.Fatalf("pre-expiry wrapper read n=%d err=%v", n, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	queuedUplink := append([]byte(nil), buf[:n]...)
	queuedDownlink := append([]byte(nil), downlink...)
	setManagedGrantState(t, identity, time.Now().Add(-time.Hour).Unix(), false, grant.Generation)

	writes := 0
	err = clientTestWriteWG(identity, grant.Generation, queuedUplink, nil, func([]byte) error {
		writes++
		return nil
	})
	if err == nil || err.Error() != "LEASE_EXPIRED" || writes != 0 {
		t.Fatalf("queued uplink crossed expiry: writes=%d err=%v", writes, err)
	}

	n, err = conn.Write(queuedDownlink)
	if n != 0 || err == nil || err.Error() != "LEASE_EXPIRED" {
		t.Fatalf("queued downlink crossed expiry: n=%d err=%v", n, err)
	}
}
