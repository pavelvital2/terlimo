package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
	"wg-turn-client/wlbs"
)

// Public metadata only. No subscription, private key, registration, network or probe call.
func TestPublicTESTAssetsCompatibility(t *testing.T) {
	raw, err := os.ReadFile("../testapp/src/main/assets/test-public-trust.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != "d3a54153dbc936b503dcab55da34a159787069272ce23beb59e86612c9e4e94f" {
		t.Fatal("public metadata SHA changed")
	}
	var m struct {
		Env    string `json:"env"`
		Kid    string `json:"issuer_kid"`
		Issuer string `json:"issuer_spki_b64url"`
		wlbs.Endpoint
		WGPort int    `json:"wg_port"`
		Cap    int    `json:"max_workers"`
		Probe  string `json:"probe_url"`
		Exit   string `json:"expected_exit_ip"`
	}
	if err := wlbs.StrictJSON(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Env != "test" || m.NodeID != "terlimo-test-193-5-251-217" || m.DTLSPort != 56000 || m.WGPort != 56002 || m.Cap != 36 {
		t.Fatal("metadata/schema mismatch")
	}
	spki, err := wlbs.DecodeBinary(m.Issuer, -1)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok || key.Curve.Params().BitSize != 256 {
		t.Fatal("issuer is not P256")
	}
	if _, err := managedPinVerifier(m.DTLSSPKISHA256); err != nil {
		t.Fatal(err)
	}
	if net.ParseIP(m.PeerIP).To4() == nil || net.ParseIP(m.Exit).To4() == nil {
		t.Fatal("IPv4 required")
	}
	u, err := url.Parse(m.Probe)
	if err != nil || u.Scheme != "https" || u.Host != "api.ipify.org" || u.User != nil || u.Fragment != "" {
		t.Fatal("probe schema")
	}
	issuerBytes, err := os.ReadFile("../testapp/src/main/assets/issuers.json")
	if err != nil {
		t.Fatal(err)
	}
	var issuers map[string]string
	if wlbs.StrictJSON(issuerBytes, &issuers) != nil || len(issuers) != 1 || issuers[m.Kid] != m.Issuer {
		t.Fatal("wrong issuer assets")
	}
	// Model validation uses synthetic in-memory access fields only, never creates a grant.
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cat := runnerCatalog(now)
	node := &cat.Nodes[0]
	node.Endpoint = m.Endpoint
	node.WGPort = m.WGPort
	node.MaxWorkers = m.Cap
	if err := cat.Validate("sub", "reg", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < m.Cap; i++ {
		id := wlbs.VPNIdentity{NodeID: m.NodeID, GrantID: "synthetic", RegistrationID: "reg", Generation: "1", LeaseSeq: "1", TransportSession: "0123456789abcdef0123456789abcdef", WorkerID: strconv.Itoa(i), Mode: "data"}
		if i == 0 {
			id.Mode = "getconf"
		}
		if err := id.ValidateWorker(m.Cap); err != nil {
			t.Fatal(err)
		}
	}
	// The server WG port is catalog metadata, not a direct client endpoint.
	config, err := managedWGConfig("[Interface]\nAddress = 10.67.67.2/32\n[Peer]\nEndpoint = 127.0.0.1:56002\n", "12345")
	if err != nil || config != "[Interface]\nAddress = 10.67.67.2/32\n[Peer]\nEndpoint = 127.0.0.1:12345\n" {
		t.Fatal("managed WG rewrite", err)
	}
}
