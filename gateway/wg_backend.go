package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/device"
)

type wgDevice interface {
	IpcSet(string) error
	Close()
}

// Peer counters are the user-data accounting layer for managed grants: one layer only,
// taken from the WireGuard peer statistics (rx/tx), never from the outer WDTT transport.
type peerCounters struct {
	PublicKeyHex  string
	RxBytes       int64
	TxBytes       int64
	LastHandshake int64
}

// Optional capability: engines that can expose per-peer counters implement it. Engines
// without it are reported as unsupported (never as zero usage).
type peerUsageProvider interface {
	PeerUsage() ([]peerCounters, error)
}

type userspaceWG struct {
	*device.Device
}

func (d *userspaceWG) PeerUsage() ([]peerCounters, error) {
	if d == nil || d.Device == nil {
		return nil, errors.New("userspace WireGuard is not initialized")
	}
	raw, err := d.Device.IpcGet()
	if err != nil {
		return nil, err
	}
	return parseUAPIPeerUsage(raw)
}

func parseUAPIPeerUsage(raw string) ([]peerCounters, error) {
	var peers []peerCounters
	var current *peerCounters
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "public_key":
			peers = append(peers, peerCounters{PublicKeyHex: value})
			current = &peers[len(peers)-1]
		case "rx_bytes":
			if current == nil {
				return nil, errors.New("UAPI rx_bytes without public_key")
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return nil, fmt.Errorf("UAPI rx_bytes invalid: %q", value)
			}
			current.RxBytes = parsed
		case "tx_bytes":
			if current == nil {
				return nil, errors.New("UAPI tx_bytes without public_key")
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return nil, fmt.Errorf("UAPI tx_bytes invalid: %q", value)
			}
			current.TxBytes = parsed
		case "last_handshake_time_sec":
			if current == nil {
				return nil, errors.New("UAPI last_handshake_time_sec without public_key")
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("UAPI last_handshake_time_sec invalid: %q", value)
			}
			current.LastHandshake = parsed
		}
	}
	return peers, nil
}

func parseKernelWGDump(raw string) ([]peerCounters, error) {
	var peers []peerCounters
	for index, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if index == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 8 {
			return nil, errors.New("kernel WireGuard dump row malformed")
		}
		pub, err := wireGuardBase64ToHex(fields[0])
		if err != nil {
			return nil, err
		}
		handshake, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil || handshake < 0 {
			return nil, fmt.Errorf("kernel WireGuard handshake invalid: %q", fields[4])
		}
		rx, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil || rx < 0 {
			return nil, fmt.Errorf("kernel WireGuard rx invalid: %q", fields[5])
		}
		tx, err := strconv.ParseInt(fields[6], 10, 64)
		if err != nil || tx < 0 {
			return nil, fmt.Errorf("kernel WireGuard tx invalid: %q", fields[6])
		}
		peers = append(peers, peerCounters{PublicKeyHex: pub, RxBytes: rx, TxBytes: tx, LastHandshake: handshake})
	}
	return peers, nil
}

func wireGuardBase64ToHex(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("key length %d != 32", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

type kernelWGDevice struct {
	iface string
	mu    sync.Mutex
}

func (d *kernelWGDevice) Close() {
	if d == nil || d.iface == "" {
		return
	}
	_, _ = runCmd("ip", "link", "del", d.iface)
}

func (d *kernelWGDevice) PeerUsage() ([]peerCounters, error) {
	if d == nil || d.iface == "" {
		return nil, errors.New("kernel WireGuard is not initialized")
	}
	output, err := runCmd("wg", "show", d.iface, "dump")
	if err != nil {
		return nil, fmt.Errorf("wg show dump: %s", output)
	}
	return parseKernelWGDump(output)
}

func (d *kernelWGDevice) IpcSet(configuration string) error {
	if d == nil || d.iface == "" {
		return errors.New("kernel WireGuard is not initialized")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	values := make(map[string]string)
	for _, line := range strings.Split(configuration, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	publicHex := strings.TrimSpace(values["public_key"])
	if publicHex == "" {
		return errors.New("kernel WireGuard peer update has no public key")
	}
	publicKey, err := wireGuardHexToBase64(publicHex)
	if err != nil {
		return fmt.Errorf("kernel WireGuard public key: %w", err)
	}
	args := []string{"set", d.iface, "peer", publicKey}
	if values["remove"] == "true" {
		args = append(args, "remove")
	} else if allowedIP := strings.TrimSpace(values["allowed_ip"]); allowedIP != "" {
		args = append(args, "allowed-ips", allowedIP)
	} else {
		return errors.New("kernel WireGuard peer update has no operation")
	}
	if output, err := runCmd("wg", args...); err != nil {
		return fmt.Errorf("wg peer update: %s", output)
	}
	return nil
}

func wireGuardHexToBase64(value string) (string, error) {
	raw, err := hex.DecodeString(value)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("key length %d != 32", len(raw))
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func startKernelWG(keys *wgKeys, wgPort int, configDir string) (wgDevice, error) {
	if !commandExists("wg") || !commandExists("ip") {
		return nil, errors.New("commands ip/wg are not installed")
	}
	_, _ = runCmd("ip", "link", "del", wgIfaceName)
	if output, err := runCmd("ip", "link", "add", wgIfaceName, "type", "wireguard"); err != nil {
		return nil, fmt.Errorf("kernel WireGuard unavailable: %s", output)
	}
	dev := &kernelWGDevice{iface: wgIfaceName}
	ok := false
	defer func() {
		if !ok {
			dev.Close()
		}
	}()

	runtimeDir := filepath.Join(configDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return nil, fmt.Errorf("create WireGuard runtime directory: %w", err)
	}
	keyFile, err := os.CreateTemp(runtimeDir, "wg-private-*")
	if err != nil {
		return nil, fmt.Errorf("create WireGuard key file: %w", err)
	}
	keyPath := keyFile.Name()
	defer os.Remove(keyPath)
	if err := keyFile.Chmod(0600); err != nil {
		keyFile.Close()
		return nil, err
	}
	if _, err := keyFile.WriteString(keys.serverPrivate + "\n"); err != nil {
		keyFile.Close()
		return nil, err
	}
	if err := keyFile.Close(); err != nil {
		return nil, err
	}
	if output, err := runCmd(
		"wg", "set", wgIfaceName,
		"private-key", keyPath,
		"listen-port", strconv.Itoa(wgPort),
	); err != nil {
		return nil, fmt.Errorf("configure kernel WireGuard: %s", output)
	}
	if err := configureInterface(wgIfaceName); err != nil {
		return nil, err
	}
	if err := setupFullConeNAT(wgIfaceName); err != nil {
		return nil, err
	}
	ok = true
	logWGBackend("kernel")
	return dev, nil
}

func startWGBackend(mode string, keys *wgKeys, wgPort int, configDir string) (wgDevice, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "kernel":
		return startKernelWG(keys, wgPort, configDir)
	case "userspace":
		dev, err := startUserspaceWG(keys, wgPort)
		if err != nil {
			return nil, err
		}
		logWGBackend("userspace")
		return &userspaceWG{Device: dev}, nil
	case "auto":
		if dev, err := startKernelWG(keys, wgPort, configDir); err == nil {
			return dev, nil
		} else {
			fmt.Printf("[WG] Kernel backend недоступен, использую userspace: %v\n", err)
		}
		dev, err := startUserspaceWG(keys, wgPort)
		if err != nil {
			return nil, err
		}
		logWGBackend("userspace")
		return &userspaceWG{Device: dev}, nil
	default:
		return nil, fmt.Errorf("unknown WireGuard backend %q", mode)
	}
}

var activeWGBackend atomicString

type atomicString struct {
	mu    sync.RWMutex
	value string
}

func (s *atomicString) Store(value string) {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
}

func (s *atomicString) Load() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

func logWGBackend(name string) {
	activeWGBackend.Store(name)
	fmt.Printf("[WG] Backend: %s\n", name)
}
