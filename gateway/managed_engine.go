package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

const managedInterface = "wdttm0"
const managedEndpoint = "127.0.0.1:56002"
const managedCIDR = "10.67.67.1/24"

// Ownership is reconstructed from authoritative grants before any engine starts.
// No peers are installed on restore: every managed connection still needs PoP/GETCONF.
type enginePeer struct {
	managed      bool
	ip           string
	registration string
}
type engineRouter struct {
	mu              sync.Mutex
	legacy, managed wgDevice
	keys            *wgKeys
	peers           map[string]enginePeer
}

func (r *engineRouter) register(d *ClientDevice, managed bool) error {
	if d == nil {
		return errors.New("ENGINE_DEVICE_MISSING")
	}
	prefix := netip.MustParsePrefix("10.66.66.0/24")
	if managed {
		prefix = netip.MustParsePrefix("10.67.67.0/24")
	}
	addr, err := netip.ParseAddr(d.IP)
	if err != nil || !prefix.Contains(addr) || addr.As4()[3] < 2 || addr.As4()[3] > 250 {
		return errors.New("ENGINE_POOL_MISMATCH")
	}
	pub, err := b64ToHex(d.PubKey)
	if err != nil {
		return errors.New("ENGINE_KEY_INVALID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := enginePeer{managed: managed, ip: d.IP, registration: d.DeviceID}
	if old, ok := r.peers[pub]; ok && old != next {
		return errors.New("ENGINE_OWNERSHIP_CONFLICT")
	}
	r.peers[pub] = next
	return nil
}

func validateEngineState(loaded *Database) (*engineRouter, error) {
	r := &engineRouter{peers: map[string]enginePeer{}}
	managed := map[string]bool{}
	active := map[string]bool{}
	for _, entry := range loaded.Passwords {
		if entry == nil || entry.ClientTest == nil {
			continue
		}
		id := entry.ClientTest.RegistrationID
		if id == "" || entry.DeviceID != id {
			return nil, errors.New("ENGINE_GRANT_OWNERSHIP")
		}
		if !entry.ClientTest.Revoked {
			if active[id] {
				return nil, errors.New("ENGINE_GRANT_OWNERSHIP")
			}
			active[id] = true
		}
		managed[id] = true
	}
	for _, id := range loaded.AdminProfile.DeviceIDs {
		if managed[id] {
			return nil, errors.New("ENGINE_ADMIN_COLLISION")
		}
	}
	for _, entry := range loaded.Passwords {
		if entry != nil && entry.ClientTest == nil && managed[entry.DeviceID] {
			return nil, errors.New("ENGINE_LEGACY_COLLISION")
		}
	}
	seenIP, seenKey := map[string]bool{}, map[string]bool{}
	for id, d := range loaded.Devices {
		if d == nil || d.DeviceID != id {
			return nil, errors.New("ENGINE_DEVICE_ID")
		}
		if seenIP[d.IP] || seenKey[d.PubKey] {
			return nil, errors.New("ENGINE_DEVICE_COLLISION")
		}
		seenIP[d.IP], seenKey[d.PubKey] = true, true
		if err := r.register(d, managed[id]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *engineRouter) IpcSet(config string) error {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(config), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || fields[key] != "" {
			return errors.New("ENGINE_OPERATION_INVALID")
		}
		switch key {
		case "public_key", "allowed_ip", "remove":
		default:
			return errors.New("ENGINE_OPERATION_INVALID")
		}
		fields[key] = value
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, ok := r.peers[fields["public_key"]]
	if !ok {
		return errors.New("ENGINE_OWNER_UNKNOWN")
	}
	if !((len(fields) == 2 && fields["remove"] == "true") || (len(fields) == 2 && fields["allowed_ip"] == owner.ip+"/32")) {
		return errors.New("ENGINE_OPERATION_INVALID")
	}
	target := r.legacy
	if owner.managed {
		target = r.managed
	}
	if target == nil {
		return errors.New("ENGINE_UNAVAILABLE")
	}
	return target.IpcSet(config)
}

type peerUsageEntry struct {
	RegistrationID string `json:"registration_id"`
	PublicKey      string `json:"public_key"`
	Managed        bool   `json:"managed"`
	RxBytes        int64  `json:"rx_bytes"`
	TxBytes        int64  `json:"tx_bytes"`
	LastHandshake  int64  `json:"last_handshake"`
}

// PeerUsage returns one accounting layer for known peers only: counters as reported by the
// WireGuard engines, mapped back to their registration identity. Unknown peers are never
// credited, and unsupported engines are reported via counters_supported=false by callers.
func (r *engineRouter) PeerUsage() ([]peerUsageEntry, bool, error) {
	r.mu.Lock()
	managed, legacy := r.managed, r.legacy
	owners := make(map[string]enginePeer, len(r.peers))
	for key, peer := range r.peers {
		owners[key] = peer
	}
	r.mu.Unlock()
	entries := map[string]peerUsageEntry{}
	supported := false
	for _, target := range []wgDevice{managed, legacy} {
		provider, ok := target.(peerUsageProvider)
		if !ok {
			continue
		}
		supported = true
		counters, err := provider.PeerUsage()
		if err != nil {
			return nil, supported, err
		}
		for _, counter := range counters {
			owner, known := owners[counter.PublicKeyHex]
			if !known {
				continue
			}
			if _, exists := entries[counter.PublicKeyHex]; exists {
				continue
			}
			entries[counter.PublicKeyHex] = peerUsageEntry{
				RegistrationID: owner.registration,
				PublicKey:      counter.PublicKeyHex,
				Managed:        owner.managed,
				RxBytes:        counter.RxBytes,
				TxBytes:        counter.TxBytes,
				LastHandshake:  counter.LastHandshake,
			}
		}
	}
	result := make([]peerUsageEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	return result, supported, nil
}

func (r *engineRouter) Close() {
	if r.managed != nil {
		r.managed.Close()
	}
	if r.legacy != nil {
		r.legacy.Close()
	}
}
func managedRoute(wg wgDevice) (string, *wgKeys, error) {
	r, ok := wg.(*engineRouter)
	if !ok || r.managed == nil || r.keys == nil {
		return "", nil, errors.New("MANAGED_ENGINE_UNAVAILABLE")
	}
	return managedEndpoint, r.keys, nil
}
func registerConnectionDevice(wg wgDevice, d *ClientDevice, managed bool) error {
	if r, ok := wg.(*engineRouter); ok {
		return r.register(d, managed)
	}
	if managed {
		return errors.New("MANAGED_ENGINE_UNAVAILABLE")
	}
	return nil // Legacy unit fixtures and non-managed callers.
}
func nextManagedIP() string {
	used := map[string]bool{}
	for _, d := range db.Devices {
		if d != nil {
			used[d.IP] = true
		}
	}
	for i := 2; i <= 250; i++ {
		ip := fmt.Sprintf("10.67.67.%d", i)
		if !used[ip] {
			return ip
		}
	}
	return ""
}

func loadManagedKeys(dir string, hasManagedDevices bool) (*wgKeys, error) {
	dir = filepath.Join(dir, "managed-test")
	path := filepath.Join(dir, "wg-keys.dat")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("MANAGED_KEY_PERMISSIONS")
		}
		return loadOrGenerateKeys(dir)
	}
	if !os.IsNotExist(err) || hasManagedDevices {
		return nil, errors.New("MANAGED_KEYS_MISSING")
	}
	if err := ensurePrivateDatabaseDirectory(dir); err != nil {
		return nil, err
	}
	sp, su, err := generateKeyPair()
	if err != nil {
		return nil, err
	}
	cp, cu, err := generateKeyPair()
	if err != nil {
		return nil, err
	}
	if err := writeSyncedFileAtomically(path, []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n", sp, su, cp, cu)), 0600); err != nil {
		return nil, err
	}
	return &wgKeys{sp, su, cp, cu}, nil
}

// Does not invoke the legacy network helper, delete an interface, open UAPI,
// or configure NAT. The reviewed rollout owns the new pool's firewall rules.
func startManagedWG(keys *wgKeys) (wgDevice, error) {
	if _, err := os.Stat("/sys/class/net/" + managedInterface); !os.IsNotExist(err) {
		return nil, errors.New("MANAGED_INTERFACE_OCCUPIED")
	}
	td, err := tun.CreateTUN(managedInterface, wgMTU)
	if err != nil {
		return nil, err
	}
	dev := device.NewDevice(td, &loopbackBind{}, device.NewLogger(device.LogLevelError, "[WG managed] "))
	good := false
	defer func() {
		if !good {
			dev.Close()
		}
	}()
	private, err := b64ToHex(keys.serverPrivate)
	if err != nil {
		return nil, err
	}
	if err = dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=56002\n", private)); err != nil {
		return nil, err
	}
	if err = dev.Up(); err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"addr", "add", managedCIDR, "dev", managedInterface}, {"link", "set", "mtu", fmt.Sprint(wgMTU), "dev", managedInterface}, {"link", "set", managedInterface, "up"}} {
		if _, err = runCmd("ip", args...); err != nil {
			return nil, errors.New("MANAGED_INTERFACE_CONFIG_FAILED")
		}
	}
	good = true
	return &userspaceWG{Device: dev}, nil
}

// Bootstrap is dispatched before this check. The main password remains usable
// for administrative operations, never as a TEST VPN bypass.
func vpnIdentityAllowed(managed bool) bool { return managed || os.Getenv("WL_TEST_ENABLED") != "1" }
