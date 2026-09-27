package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"golang.zx2c4.com/wireguard/conn"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingEngine struct {
	commands []string
	fail     bool
	closed   bool
}

func (d *recordingEngine) IpcSet(s string) error {
	if d.fail {
		return errors.New("synthetic failure")
	}
	d.commands = append(d.commands, s)
	return nil
}
func (d *recordingEngine) Close() { d.closed = true }
func syntheticDevice(t *testing.T, id, ip string) *ClientDevice {
	t.Helper()
	pr, pu, e := generateKeyPair()
	if e != nil {
		t.Fatal(e)
	}
	return &ClientDevice{DeviceID: id, IP: ip, PrivKey: pr, PubKey: pu}
}
func mixedEngineFixture(t *testing.T) (*Database, *engineRouter, *recordingEngine, *recordingEngine) {
	t.Helper()
	d := &Database{Passwords: map[string]*PasswordEntry{"legacy": {DeviceID: "legacy"}, "managed": {DeviceID: "managed", ClientTest: &ClientTestGrant{GrantID: "g", RegistrationID: "managed"}}}, Devices: map[string]*ClientDevice{"legacy": syntheticDevice(t, "legacy", "10.66.66.2"), "managed": syntheticDevice(t, "managed", "10.67.67.2")}}
	r, e := validateEngineState(d)
	if e != nil {
		t.Fatal(e)
	}
	l, m := &recordingEngine{}, &recordingEngine{}
	r.legacy = l
	r.managed = m
	r.keys = &wgKeys{serverPublic: "synthetic-only"}
	return d, r, l, m
}
func TestEngineMixedRestoreOwnershipAndCleanup(t *testing.T) {
	d, r, l, m := mixedEngineFixture(t)
	if len(l.commands)+len(m.commands) != 0 {
		t.Fatal("restore installed peer before auth")
	}
	upsertPeerInWG(r, d.Devices["legacy"])
	upsertPeerInWG(r, d.Devices["managed"])
	if len(l.commands) != 1 || len(m.commands) != 1 || !strings.Contains(l.commands[0], "10.66.66.2") || !strings.Contains(m.commands[0], "10.67.67.2") {
		t.Fatal("engine routing")
	}
	old := db
	db = d
	defer func() { db = old }()
	if e := clientTestRemovePeer(r, *d.Passwords["managed"].ClientTest); e != nil {
		t.Fatal(e)
	}
	if len(l.commands) != 1 || len(m.commands) != 2 || !strings.Contains(m.commands[1], "remove=true") {
		t.Fatal("cleanup touched legacy")
	}
	m.fail = true
	if e := clientTestRemovePeer(r, *d.Passwords["managed"].ClientTest); e == nil {
		t.Fatal("removal failure accepted")
	}
	if len(l.commands) != 1 {
		t.Fatal("fallback to legacy")
	}
	r.managed = nil
	if _, _, e := managedRoute(r); e == nil {
		t.Fatal("missing engine accepted")
	}
	pub, _ := b64ToHex(d.Devices["managed"].PubKey)
	if e := r.IpcSet(fmt.Sprintf("public_key=%s\nallowed_ip=10.67.67.2/32\n", pub)); e == nil {
		t.Fatal("missing engine fallback")
	}
	if len(l.commands) != 1 {
		t.Fatal("fallback write")
	}
	r.managed = m
	r.Close()
	if !l.closed || !m.closed {
		t.Fatal("separate close")
	}
}
func TestEngineRestoreRejectsMisownership(t *testing.T) {
	for _, kind := range []string{"pool", "legacy_reference", "admin_reference", "ip_collision", "key_collision", "orphan_managed", "duplicate_grant", "grant_device_mismatch"} {
		t.Run(kind, func(t *testing.T) {
			d, _, _, _ := mixedEngineFixture(t)
			switch kind {
			case "pool":
				d.Devices["managed"].IP = "10.66.66.3"
			case "legacy_reference":
				d.Passwords["legacy"].DeviceID = "managed"
			case "admin_reference":
				d.AdminProfile.DeviceIDs = []string{"managed"}
			case "ip_collision":
				d.Devices["legacy_duplicate"] = syntheticDevice(t, "legacy_duplicate", d.Devices["legacy"].IP)
			case "key_collision":
				d.Devices["managed"].PubKey = d.Devices["legacy"].PubKey
			case "orphan_managed":
				delete(d.Passwords, "managed")
			case "duplicate_grant":
				d.Passwords["duplicate"] = d.Passwords["managed"]
			case "grant_device_mismatch":
				d.Passwords["managed"].DeviceID = "legacy"
			}
			if _, e := validateEngineState(d); e == nil {
				t.Fatal("accepted", kind)
			}
		})
	}
}
func TestEngineRestoreRetainsRevokedGrantTombstone(t *testing.T) {
	d, _, _, _ := mixedEngineFixture(t)
	d.Passwords["revoked"] = &PasswordEntry{DeviceID: "managed", IsDeactivated: true, ClientTest: &ClientTestGrant{GrantID: "old", RegistrationID: "managed", Revoked: true}}
	for i := 0; i < 20; i++ {
		r, err := validateEngineState(d)
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := b64ToHex(d.Devices["managed"].PubKey)
		if !r.peers[pub].managed || r.managed != nil || r.legacy != nil {
			t.Fatal("restore lost managed ownership or installed engine")
		}
	}
	d.Passwords["second_active"] = &PasswordEntry{DeviceID: "managed", ClientTest: &ClientTestGrant{GrantID: "new", RegistrationID: "managed"}}
	if _, err := validateEngineState(d); err == nil {
		t.Fatal("accepted two non-revoked grants")
	}
}
func TestEngineRejectUnknownPeerAndCrossPool(t *testing.T) {
	d, r, l, m := mixedEngineFixture(t)
	pub, _ := b64ToHex(d.Devices["managed"].PubKey)
	for _, s := range []string{fmt.Sprintf("public_key=%s\nallowed_ip=10.66.66.2/32\n", pub), "public_key=" + hex.EncodeToString(make([]byte, 32)) + "\nremove=true\n", "listen_port=56001\n"} {
		if r.IpcSet(s) == nil {
			t.Fatal("accepted cross-engine operation")
		}
	}
	if len(l.commands)+len(m.commands) != 0 {
		t.Fatal("operation escaped")
	}
	if r.register(d.Devices["managed"], false) == nil {
		t.Fatal("legacy registration accepted managed device")
	}
	old := db
	db = d
	defer func() { db = old }()
	if nextManagedIP() != "10.67.67.3" {
		t.Fatal("allocator overlap")
	}
}
func TestManagedKeysSeparateAndMissingFailClosed(t *testing.T) {
	dir := t.TempDir()
	old := db
	db = &Database{Passwords: map[string]*PasswordEntry{}, Devices: map[string]*ClientDevice{}}
	defer func() { db = old }()
	legacy, e := loadOrGenerateKeys(dir)
	if e != nil {
		t.Fatal(e)
	}
	managed, e := loadManagedKeys(dir, false)
	if e != nil {
		t.Fatal(e)
	}
	if legacy.serverPublic == managed.serverPublic {
		t.Fatal("shared key")
	}
	again, e := loadManagedKeys(dir, true)
	if e != nil || again.serverPublic != managed.serverPublic {
		t.Fatal("restore key changed")
	}
	if e = os.Remove(filepath.Join(dir, "managed-test", "wg-keys.dat")); e != nil {
		t.Fatal(e)
	}
	if _, e = loadManagedKeys(dir, true); e == nil {
		t.Fatal("regenerated persisted managed key")
	}
}
func TestManagedLoopbackBindOffline(t *testing.T) {
	b := &loopbackBind{}
	fns, port, e := b.Open(0)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if b.socket.LocalAddr().(*net.UDPAddr).IP.String() != "127.0.0.1" || len(fns) != 1 {
		t.Fatal("wildcard/v6 bind")
	}
	for _, ep := range []string{"0.0.0.0:56002", "[::1]:56002", "192.0.2.1:56002"} {
		if _, e = b.ParseEndpoint(ep); e == nil {
			t.Fatal("foreign endpoint")
		}
	}
	c, e := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, e = c.Write([]byte("offline")); e != nil {
		t.Fatal(e)
	}
	p := [][]byte{make([]byte, 128)}
	sizes := make([]int, 1)
	eps := make([]conn.Endpoint, 1)
	if n, e := fns[0](p, sizes, eps); e != nil || n != 1 || string(p[0][:sizes[0]]) != "offline" {
		t.Fatal("receive", e)
	}
	if e = b.Send([][]byte{[]byte("reply")}, eps[0]); e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, 128)
	if n, e := c.Read(buf); e != nil || string(buf[:n]) != "reply" {
		t.Fatal("send", e)
	}
	b.Close()
	if _, e = fns[0](p, sizes, eps); !errors.Is(e, net.ErrClosed) {
		t.Fatal("close did not unblock", e)
	}
}

func TestManagedOnlyRejectsLegacyAndMainRelay(t *testing.T) {
	t.Setenv("WL_TEST_ENABLED", "1")
	if vpnIdentityAllowed(false) || !vpnIdentityAllowed(true) {
		t.Fatal("managed-only identity gate")
	}
	t.Setenv("WL_TEST_ENABLED", "0")
	if !vpnIdentityAllowed(false) {
		t.Fatal("non-TEST behavior changed")
	}
}
