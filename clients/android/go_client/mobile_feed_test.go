package main

// Focused mobile transport-feed tests: a controlled mobile-v1 HTTP fixture, the real
// managed bridge, and a stubbed tunnel boundary that mimics the real vpn contract
// (registering its cancellation with the controller while the data plane is active).

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"os"

	"wg-turn-client/accountaccess"
	"wg-turn-client/wlbs"
)

type feedHost struct {
	t        *testing.T
	fixture  *mobileFeedFixture
	mu       sync.Mutex
	raw      []byte
	messages []map[string]any
	persists int
	reply    io.Writer
	out      chan []byte
	vpn      func(request, response map[string]any)
}

// enqueue hands one host->native frame to the dedicated writer. The reader goroutine
// must never block on the reply pipe while the native side may still be writing.
func (h *feedHost) enqueue(raw []byte) {
	if h.out != nil {
		h.out <- raw
		return
	}
	if h.reply != nil {
		_, _ = h.reply.Write(raw)
	}
}

// setVPNResult changes the host answer for the next vpn_config frames. It must be set
// before the affected runtime starts; the hook is called under the host lock.
func (h *feedHost) setVPNResult(hook func(request, response map[string]any)) {
	h.mu.Lock()
	h.vpn = hook
	h.mu.Unlock()
}

// send writes one host->native frame through the real bridge input pipe.
func (h *feedHost) send(t *testing.T, message map[string]any) {
	t.Helper()
	if h.reply == nil {
		t.Fatal("host has no reply pipe")
	}
	message["v"] = 1
	message["attempt_id"] = "attempt"
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	h.enqueue(append(raw, '\n'))
}

// Write is the fake host end of the real bridge: it records every native event and
// answers the production signing/persist requests with the fixture installation key.
func (h *feedHost) Write(p []byte) (int, error) {
	h.mu.Lock()
	h.raw = append(h.raw, p...)
	h.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		var message map[string]any
		if json.Unmarshal([]byte(line), &message) != nil {
			continue
		}
		h.mu.Lock()
		h.messages = append(h.messages, message)
		if message["type"] == "persist" {
			h.persists++
		}
		h.mu.Unlock()
		if h.reply == nil {
			continue
		}
		response := map[string]any{"v": 1, "attempt_id": "attempt"}
		switch message["type"] {
		case "vpn_config":
			// The real managed vpn always asks the host to apply the target config and
			// waits for the vpn_result acknowledgement.
			response["type"] = "vpn_result"
			response["request_id"] = message["request_id"]
			response["runtime_epoch"] = message["runtime_epoch"]
			response["ok"] = true
			if _, switching := message["switch_id"]; switching {
				response["switch_id"] = message["switch_id"]
				response["catalog_revision"] = message["catalog_revision"]
			}
			h.mu.Lock()
			hook := h.vpn
			h.mu.Unlock()
			if hook != nil {
				hook(message, response)
			}
		case "sign":
			transcript, err := base64.RawURLEncoding.DecodeString(fmt.Sprint(message["transcript_b64"]))
			if err != nil {
				continue
			}
			digest := sha256.Sum256(transcript)
			signature, err := ecdsa.SignASN1(rand.Reader, h.fixture.key, digest[:])
			if err != nil {
				continue
			}
			response["type"] = "sign_result"
			response["signing_request_id"] = message["signing_request_id"]
			response["signature_b64"] = accountaccess.B64URLEncode(signature)
		case "persist":
			response["type"] = "persist_result"
			response["request_id"] = message["request_id"]
		default:
			continue
		}
		raw, err := json.Marshal(response)
		if err != nil {
			continue
		}
		h.enqueue(append(raw, '\n'))
	}
	return len(p), nil
}

func (h *feedHost) find(messageType string) (map[string]any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := len(h.messages) - 1; index >= 0; index-- {
		if h.messages[index]["type"] == messageType {
			return h.messages[index], true
		}
	}
	return nil, false
}

func (h *feedHost) waitMessage(t *testing.T, messageType string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if message, ok := h.find(messageType); ok {
			return message
		}
		if time.Now().After(deadline) {
			h.mu.Lock()
			raw := string(h.raw)
			h.mu.Unlock()
			t.Fatalf("%s never reached the bridge: %q", messageType, raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *feedHost) persistCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.persists
}

type mobileFeedOptions struct {
	deadline          time.Duration
	validUntil        time.Duration
	bindingStatus     string
	entitlementStatus string
	dataAccess        string
	omitLeaseSeq      bool
	omitTargetWorkers bool
	countryNull       bool
	revision          string
	// indefinite mirrors the real perpetual commercial entitlement: perpetual_commercial
	// true, valid_until null and a null effective_deadline for subscription_data.
	indefinite bool
}

func (o mobileFeedOptions) withDefaults() mobileFeedOptions {
	if o.deadline == 0 {
		o.deadline = time.Hour
	}
	if o.validUntil == 0 {
		o.validUntil = 10 * time.Minute
	}
	if o.bindingStatus == "" {
		o.bindingStatus = "active"
	}
	if o.entitlementStatus == "" {
		o.entitlementStatus = "active"
	}
	if o.dataAccess == "" {
		o.dataAccess = "subscription_data"
	}
	if o.revision == "" {
		o.revision = "7"
	}
	return o
}

type mobileFeedFixture struct {
	t            *testing.T
	mu           sync.Mutex
	key          *ecdsa.PrivateKey
	spkiDER      []byte
	fingerprint  string
	challenges   map[string]string
	linked       bool
	me           map[string]any
	gateways     []map[string]any
	revision     string
	issuedAt     time.Time
	validUntil   time.Time
	meCalls      int
	catalogCalls int
}

func feedStamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func mobileFeedMeBody(options mobileFeedOptions, now time.Time) map[string]any {
	options = options.withDefaults()
	grant := map[string]any{
		"control_available":             true,
		"restricted_checkout_available": true,
		"data_access":                   options.dataAccess,
	}
	deadline := now.Add(options.deadline)
	if options.indefinite || options.dataAccess == "none" || options.dataAccess == "restricted_checkout" {
		grant["effective_deadline"] = nil
	} else {
		grant["effective_deadline"] = feedStamp(deadline)
	}
	entitlement := map[string]any{
		"type": "trial", "status": options.entitlementStatus,
		"valid_from": feedStamp(now.Add(-time.Hour)), "valid_until": feedStamp(now.Add(24 * time.Hour)),
		"effective_device_limit": 2, "slots_used": 1,
		"revision": "3", "perpetual_commercial": false,
	}
	if options.indefinite {
		entitlement["valid_until"] = nil
		entitlement["perpetual_commercial"] = true
	}
	return map[string]any{
		"request_id":       "0123456789abcdef0123456789abcdef",
		"server_time":      feedStamp(now),
		"schema_version":   "1.0",
		"status":           "ok",
		"account_state":    "ACTIVE_TRIAL",
		"telegram_linked":  true,
		"entitlement":      entitlement,
		"binding_status":   options.bindingStatus,
		"binding_revision": "1",
		"management_only":  false,
		"onboarding": map[string]any{
			"state": "not_started", "started_by": "server_confirmed_first_connection",
			"started_at": nil, "not_after": nil, "duration_seconds": 3600, "one_time": true,
			"extends_on_refresh": false, "extends_on_restart": false, "creates_trial": false,
			"requires_hardware_id": false, "unit": "installation_fingerprint",
			"post_telegram_identity": "account_history_correlation",
			"pre_telegram_reinstall": "may_be_indistinguishable_new_key_separate_unit",
		},
		"grant_resolution": grant,
		"revision":         options.revision,
	}
}

func feedSPKI(index int) string {
	spki := make([]byte, 32)
	spki[0] = byte(index + 1)
	spki[1] = byte(index >> 8)
	return wlbs.EncodeBinary(spki)
}

func mobileFeedGatewayBody(fingerprint string, index int, notAfter time.Time, options mobileFeedOptions) map[string]any {
	access := map[string]any{
		"grant_id":   fmt.Sprintf("grant-%d", index),
		"device_ref": fingerprint,
		"password":   fmt.Sprintf("synthetic-%d", index),
		"generation": "1",
		"not_after":  feedStamp(notAfter),
	}
	if !options.omitLeaseSeq {
		access["lease_seq"] = "2"
	}
	gateway := map[string]any{
		"gateway_id":   fmt.Sprintf("gw-%d", index),
		"name":         fmt.Sprintf("Synthetic %d", index),
		"region":       "test",
		"capabilities": []string{"managed"},
		"transport": map[string]any{
			"protocol": "wdtt-v17", "peer_ip": "127.0.0.1",
			"dtls_port": 56000 + index, "wg_port": 57000 + index,
			"dtls_spki_sha256": feedSPKI(index),
		},
		"access": access,
	}
	if !options.omitTargetWorkers {
		gateway["target_workers"] = 36
	}
	if options.countryNull && index == 0 {
		gateway["country_code"] = nil
	} else {
		gateway["country_code"] = "XX"
	}
	return gateway
}

func newMobileFeedFixture(t *testing.T, gatewayCount int, options mobileFeedOptions) *mobileFeedFixture {
	t.Helper()
	options = options.withDefaults()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fixture := &mobileFeedFixture{
		t:           t,
		key:         key,
		spkiDER:     der,
		fingerprint: accountaccess.InstallationFingerprint(der),
		challenges:  map[string]string{},
		revision:    options.revision,
		issuedAt:    now.Add(-5 * time.Minute),
		validUntil:  now.Add(options.validUntil),
	}
	fixture.me = mobileFeedMeBody(options, now)
	for index := 0; index < gatewayCount; index++ {
		fixture.gateways = append(fixture.gateways, mobileFeedGatewayBody(fixture.fingerprint, index, fixture.validUntil, options))
	}
	return fixture
}

func (f *mobileFeedFixture) setMe(body map[string]any) {
	f.mu.Lock()
	f.me = body
	f.mu.Unlock()
}

func (f *mobileFeedFixture) gatewayBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.gateways...)
}

func (f *mobileFeedFixture) setCatalog(revision string, gateways []map[string]any) {
	f.mu.Lock()
	f.revision = revision
	f.gateways = gateways
	f.mu.Unlock()
}

func (f *mobileFeedFixture) setValidUntil(value time.Time) {
	f.mu.Lock()
	f.validUntil = value
	f.mu.Unlock()
}

func (f *mobileFeedFixture) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meCalls, f.catalogCalls
}

func (f *mobileFeedFixture) catalogJSON() []byte {
	f.mu.Lock()
	revision, issuedAt, validUntil := f.revision, f.issuedAt, f.validUntil
	gateways := append([]map[string]any(nil), f.gateways...)
	f.mu.Unlock()
	body := map[string]any{
		"request_id":     "0123456789abcdef0123456789abcdef",
		"server_time":    feedStamp(time.Now().UTC()),
		"schema_version": "1.0",
		"status":         "ok",
		"revision":       revision,
		"valid_until":    feedStamp(validUntil),
		"issued_at":      feedStamp(issuedAt),
		"gateways":       gateways,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *mobileFeedFixture) verifyProof(proof map[string]any) map[string]any {
	if len(proof) != 8 || proof["algorithm"] != "ES256" {
		return nil
	}
	challengeID, _ := proof["challenge_id"].(string)
	nonce, _ := proof["nonce_b64"].(string)
	requestID, _ := proof["request_id"].(string)
	signed, _ := proof["signed_payload_b64"].(string)
	f.mu.Lock()
	expectedNonce := f.challenges[challengeID]
	f.mu.Unlock()
	if expectedNonce == "" || expectedNonce != nonce {
		return nil
	}
	raw, payload, err := accountaccess.DecodeProofPayload(signed)
	if err != nil || payload["request_id"] != requestID || payload["nonce"] != nonce {
		return nil
	}
	message, err := accountaccess.POPMessage(requestID, challengeID, nonce, raw)
	if err != nil {
		return nil
	}
	signature, err := accountaccess.B64URLDecodeStrict(fmt.Sprint(proof["signature_b64"]), -1)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(message)
	if !ecdsa.VerifyASN1(&f.key.PublicKey, digest[:], signature) {
		return nil
	}
	return payload
}

func (f *mobileFeedFixture) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var body map[string]any
		if request.Body != nil {
			_ = json.NewDecoder(request.Body).Decode(&body)
		}
		switch request.URL.Path {
		case "/api/mobile/v1/auth/challenge":
			challengeID := feedHex(16)
			nonce := accountaccess.B64URLEncode(feedBytes(32))
			f.mu.Lock()
			f.challenges[challengeID] = nonce
			f.mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": feedStamp(time.Now().UTC()),
				"schema_version": "1.0", "status": "ok", "challenge_id": challengeID, "nonce_b64": nonce,
				"purpose": body["purpose"], "environment": body["environment"],
				"expires_at": feedStamp(time.Now().Add(5 * time.Minute)), "single_use": true,
			})
		case "/api/mobile/v1/auth/session":
			proof, _ := body["proof"].(map[string]any)
			payload := f.verifyProof(proof)
			if payload == nil {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(writer, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"PROOF_INVALID","retryable":false}`)
				return
			}
			requested := feedStrings(payload["requested_scopes"])
			f.mu.Lock()
			linked := f.linked
			f.mu.Unlock()
			denied := ""
			if !linked {
				for _, scope := range requested {
					if scope == "access:sync" {
						denied = scope
					}
				}
			}
			if denied != "" {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(writer, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_DENIED","retryable":false}`)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": feedStamp(time.Now().UTC()),
				"schema_version": "1.0", "status": "ok",
				"session": map[string]any{"session_id": feedHex(16), "account_ref": nil,
					"installation_ref": f.fingerprint, "scopes": requested, "generation": "1",
					"issued_at": feedStamp(time.Now().UTC()), "expires_at": "2030-01-01T00:00:00Z"},
			})
		case "/api/mobile/v1/me":
			f.mu.Lock()
			f.meCalls++
			body := f.me
			f.mu.Unlock()
			_ = json.NewEncoder(writer).Encode(body)
		case "/api/mobile/v1/gateways":
			f.mu.Lock()
			f.catalogCalls++
			f.mu.Unlock()
			_, _ = writer.Write(f.catalogJSON())
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	})
}

func feedHex(size int) string { return fmt.Sprintf("%x", feedBytes(size)) }

func feedBytes(size int) []byte {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return raw
}

func feedStrings(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

func feedDecodeMe(t *testing.T, body map[string]any) accountaccess.MeResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	me, err := accountaccess.DecodeMeStrict(raw)
	if err != nil {
		t.Fatal(err)
	}
	return me
}

func feedDecodeCatalog(t *testing.T, fixture *mobileFeedFixture) accountaccess.CatalogResponse {
	t.Helper()
	catalog, err := accountaccess.DecodeCatalogStrict(fixture.catalogJSON())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func mobileFeedStart(t *testing.T, fixture *mobileFeedFixture, server *httptest.Server, probeIDs ...string) managedStart {
	t.Helper()
	probes := map[string]managedProbe{}
	for _, id := range probeIDs {
		probes[id] = managedProbe{ProbeURL: "https://probe.invalid/", ExpectedExitIP: "192.0.2.9"}
	}
	return managedStart{
		V: 1, Type: "start", AttemptID: "attempt",
		MobileBaseURL: server.URL, MobileEnvironment: "test",
		PublicKey: wlbs.EncodeBinary(fixture.spkiDER), InstallationID: fixture.fingerprint,
		ProbeByNode: probes,
	}
}

func newMobileFeedRun(t *testing.T, fixture *mobileFeedFixture, start managedStart) (*managedController, *managedBridge, *feedHost, context.CancelFunc, chan error) {
	t.Helper()
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()
	t.Cleanup(func() {
		_ = outWriter.Close()
		_ = inWriter.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := &feedHost{t: t, fixture: fixture, reply: inWriter, out: make(chan []byte, 256)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-host.out:
				if _, err := inWriter.Write(raw); err != nil {
					return
				}
			}
		}
	}()
	bridge := newManagedBridge(outWriter, "attempt", cancel)
	go bridge.read(ctx, bufio.NewScanner(inReader))
	go func() {
		scanner := bufio.NewScanner(outReader)
		scanner.Buffer(make([]byte, 4096), managedBridgeLimit)
		for scanner.Scan() {
			_, _ = host.Write(append(scanner.Bytes(), '\n'))
		}
	}()
	controller := &managedController{bridge: bridge, start: start, signSem: make(chan struct{}, 1)}
	public, err := wlbs.DecodeBinary(start.PublicKey, -1)
	if err != nil {
		t.Fatal(err)
	}
	controller.public = public
	signer := func(ctx context.Context, transcript []byte) ([]byte, error) {
		digest := sha256.Sum256(transcript)
		return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	}
	_ = signer
	errCh := make(chan error, 1)
	go func() {
		errCh <- controller.runMobile(ctx, cancel, fixture.fingerprint)
	}()
	return controller, bridge, host, cancel, errCh
}

type mobileVPNSample struct {
	node         wlbs.Node
	registration string
	operation    *managedSwitch
}

type mobileVPNStub struct {
	mu        sync.Mutex
	calls     []mobileVPNSample
	cancelled []bool
	started   chan int
}

// apply mirrors the real vpn contract: while the data plane is active the runtime
// registers its cancel with the controller and blocks until that cancel arrives.
func (s *mobileVPNStub) apply(c *managedController, parent context.Context, parentCancel context.CancelFunc,
	node wlbs.Node, registration string, replace func(), operation *managedSwitch, terminal chan<- error, preparation context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	release := c.registerMobileStop(cancel, nil)
	defer release()
	s.mu.Lock()
	index := len(s.calls)
	s.calls = append(s.calls, mobileVPNSample{node: node, registration: registration, operation: operation})
	for len(s.cancelled) <= index {
		s.cancelled = append(s.cancelled, false)
	}
	s.mu.Unlock()
	select {
	case s.started <- index:
	default:
	}
	<-ctx.Done()
	s.mu.Lock()
	s.cancelled[index] = true
	s.mu.Unlock()
	return ctx.Err()
}

func installMobileVPNStub(t *testing.T, stub *mobileVPNStub) {
	t.Helper()
	if stub.started == nil {
		stub.started = make(chan int, 4)
	}
	previous := managedVPNApply
	managedVPNApply = stub.apply
	t.Cleanup(func() { managedVPNApply = previous })
}

func (s *mobileVPNStub) waitStarted(t *testing.T) int {
	t.Helper()
	select {
	case index := <-s.started:
		return index
	case <-time.After(5 * time.Second):
		t.Fatal("vpn start was not reached")
		return -1
	}
}

func (s *mobileVPNStub) sample(index int) mobileVPNSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[index]
}

func (s *mobileVPNStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *mobileVPNStub) waitCancelled(t *testing.T, index int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		cancelled := index < len(s.cancelled) && s.cancelled[index]
		s.mu.Unlock()
		if cancelled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d was not cancelled", index)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func feedWaitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMobileRunSelectsAndStartsWithExactFields(t *testing.T) {
	fixture := newMobileFeedFixture(t, 3, mobileFeedOptions{})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0", "gw-1", "gw-2")
	controller, bridge, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	catalog := host.waitMessage(t, "catalog")
	nodes, _ := catalog["nodes"].([]any)
	if len(nodes) != 3 || catalog["selected_node_id"] != "" {
		t.Fatalf("host catalog wrong: %v", catalog)
	}
	if catalog["revision"] != "7" {
		t.Fatalf("revision wrong: %v", catalog["revision"])
	}

	bridge.selection <- "gw-1"
	index := stub.waitStarted(t)
	if stub.count() != 1 {
		t.Fatalf("exactly one child may start: %d", stub.count())
	}
	sample := stub.sample(index)
	node := sample.node
	if node.NodeID != "gw-1" || node.Name != "Synthetic 1" || node.CountryCode != "XX" ||
		node.PeerIP != "127.0.0.1" || node.DTLSPort != 56001 || node.WGPort != 57001 ||
		node.MaxWorkers != 36 || node.Protocol != "wdtt-v17" || node.AuthMode != "installation-pop-v1" ||
		node.DTLSSPKISHA256 != feedSPKI(1) {
		t.Fatalf("admitted node fields wrong: %+v", node)
	}
	if node.Access.GrantID != "grant-1" || node.Access.DeviceID != fixture.fingerprint ||
		node.Access.Password != "synthetic-1" || node.Access.Generation != "1" || node.Access.LeaseSeq != "2" {
		t.Fatalf("admitted grant fields wrong: %+v", node.Access)
	}
	if sample.registration != fixture.fingerprint {
		t.Fatalf("registration wrong: %q", sample.registration)
	}
	if controller.selectionID() != "gw-1" || controller.link.SubscriptionRef != mobileSubscriptionRef(fixture.fingerprint) {
		t.Fatal("mobile selection/technical ref not committed")
	}
	decision := controller.mobile.lastDecision()
	if decision == nil || !decision.Admitted || decision.Grant == nil || decision.Grant.NodeID != "gw-1" || decision.Grant.LeaseSeq != "2" {
		t.Fatalf("admission decision wrong: %+v", decision)
	}
	controller.mu.Lock()
	stops := len(controller.mobileStops)
	controller.mu.Unlock()
	if stops != 1 {
		t.Fatalf("active mobile runtime must register exactly one stop: %d", stops)
	}
	if host.persistCount() != 0 {
		t.Fatal("mobile mode must not persist legacy state")
	}
	if controller.saved.Subscription != "" || len(controller.link.Bootstrap) != 0 {
		t.Fatal("mobile mode must not copy legacy link fields")
	}
	cancel()
	<-errCh
}

func TestMobileRunStopsOnBindingRevoke(t *testing.T) {
	fixture := newMobileFeedFixture(t, 2, mobileFeedOptions{})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0", "gw-1")
	controller, bridge, _, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	bridge.selection <- "gw-0"
	index := stub.waitStarted(t)

	revoked := mobileFeedMeBody(mobileFeedOptions{bindingStatus: "revoked", revision: "8"}, time.Now().UTC())
	fixture.setMe(revoked)
	controller.mobile.runner.Trigger("wake")
	stub.waitCancelled(t, index)
	decision := controller.mobile.lastDecision()
	if decision == nil || !decision.StopDataPlane || decision.Reason != "BINDING_REVOKED" {
		t.Fatalf("revoke must stop with an explicit reason: %+v", decision)
	}
	if controller.store.Snapshot() == nil || !controller.store.Snapshot().MobileAuthority() {
		t.Fatal("last-good mobile metadata must be retained on revoke")
	}
	cancel()
	<-errCh
}

func TestMobileDeadlineTimerStopsWithoutWakeOrHTTP(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{deadline: 500 * time.Millisecond, validUntil: 400 * time.Millisecond})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0")
	controller, bridge, _, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	bridge.selection <- "gw-0"
	index := stub.waitStarted(t)
	meCalls, catalogCalls := fixture.calls()
	stub.waitCancelled(t, index)
	afterMe, afterCatalog := fixture.calls()
	if afterMe != meCalls || afterCatalog != catalogCalls {
		t.Fatalf("deadline stop must not need HTTP: me %d->%d catalog %d->%d", meCalls, afterMe, catalogCalls, afterCatalog)
	}
	if controller.store.Snapshot() == nil {
		t.Fatal("projected snapshot lost")
	}
	cancel()
	<-errCh
}

func TestMobileNoRightKeepsRefreshingAndAdmitsLater(t *testing.T) {
	fixture := newMobileFeedFixture(t, 2, mobileFeedOptions{dataAccess: "none"})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0", "gw-1")
	controller, _, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	// The first verified snapshot carries no data right: the lifecycle must continue.
	_, meBeforeCall := fixture.calls()
	feedWaitFor(t, func() bool {
		meAfter, _ := fixture.calls()
		return meAfter > meBeforeCall
	})
	feedWaitFor(t, func() bool { return controller.mobile.lastProjectionError() != nil })
	if err := controller.mobile.lastProjectionError(); err.Error() != "RIGHT_DEADLINE_MISSING" {
		t.Fatalf("verified no-right snapshot must be recorded as a non-terminal projection state: %v", err)
	}
	if _, ok := host.find("catalog"); ok {
		t.Fatal("no-right snapshot must not publish a catalog")
	}
	if stub.count() != 0 {
		t.Fatal("no-right snapshot must not start a child")
	}
	meBefore, catalogBefore := fixture.calls()
	controller.mobile.runner.Trigger("wake")
	feedWaitFor(t, func() bool {
		meAfter, catalogAfter := fixture.calls()
		return meAfter > meBefore && catalogAfter > catalogBefore
	})
	select {
	case err := <-errCh:
		t.Fatalf("no-right state must not end the mobile attempt: %v", err)
	default:
	}

	// A later verified right must be admitted without restarting the attempt.
	now := time.Now().UTC()
	fixture.setMe(mobileFeedMeBody(mobileFeedOptions{dataAccess: "subscription_data", deadline: time.Hour, revision: "8"}, now))
	controller.mobile.runner.Trigger("wake")
	feedWaitFor(t, func() bool {
		_, ok := host.find("catalog")
		return ok
	})
	bridge := controller.bridge
	bridge.selection <- "gw-0"
	index := stub.waitStarted(t)
	if stub.count() != 1 {
		t.Fatalf("exactly one child after the later right: %d", stub.count())
	}
	node := stub.sample(index).node
	if node.NodeID != "gw-0" || node.Access.GrantID != "grant-0" || node.Access.LeaseSeq != "2" {
		t.Fatalf("admitted node after the later right wrong: %+v", node)
	}
	cancel()
	<-errCh
}

func TestMobileMalformedCatalogWithoutLeaseSeqKeepsCyclingWithoutAdmission(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{omitLeaseSeq: true})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0")
	controller, _, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	// The accepted contract requires lease_seq, so the strict decoder rejects the whole
	// response: the runner keeps cycling and nothing is admitted or published.
	_, catalogBefore := fixture.calls()
	feedWaitFor(t, func() bool {
		_, catalogAfter := fixture.calls()
		return catalogAfter > catalogBefore
	})
	meBefore, catalogBefore := fixture.calls()
	controller.mobile.runner.Trigger("wake")
	feedWaitFor(t, func() bool {
		meAfter, catalogAfter := fixture.calls()
		return meAfter > meBefore && catalogAfter > catalogBefore
	})
	if stub.count() != 0 {
		t.Fatal("a malformed catalog must never start a child")
	}
	if _, ok := host.find("catalog"); ok {
		t.Fatal("unprojectable catalog must not be published as ready")
	}
	if host.persistCount() != 0 {
		t.Fatal("no legacy persist on a malformed mobile catalog")
	}
	select {
	case err := <-errCh:
		t.Fatalf("malformed catalog must not end the mobile attempt: %v", err)
	default:
	}
	cancel()
	<-errCh
}

func TestMobileProjectionCarriesAcceptedVKHashesAndAdmissionFields(t *testing.T) {
	now := time.Now().UTC()
	meRaw, err := os.ReadFile("accountaccess/testdata/accepted-253cd8a2/me_response.json")
	if err != nil {
		t.Fatal(err)
	}
	catalogRaw, err := os.ReadFile("accountaccess/testdata/accepted-253cd8a2/catalog_response.json")
	if err != nil {
		t.Fatal(err)
	}
	me, err := accountaccess.DecodeMeStrict(meRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := accountaccess.DecodeCatalogStrict(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Gateways) != 2 || len(catalog.Gateways[0].Access.VKHashes) == 0 {
		t.Fatalf("accepted fixture must carry multiple gateways with vk_hashes")
	}
	projected, err := projectMobileCatalog(me, catalog, "inst-fingerprint", "gw-b", now)
	if err != nil {
		t.Fatalf("accepted projection failed: %v", err)
	}
	if len(projected.Nodes) != 2 {
		t.Fatalf("multiple gateways must be preserved: %d", len(projected.Nodes))
	}
	for index, gateway := range catalog.Gateways {
		got := projected.Nodes[index].Access.VKHashes
		want := gateway.Access.VKHashes
		if len(got) != len(want) {
			t.Fatalf("gateway %s hashes lost: %v vs %v", gateway.GatewayID, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("gateway %s hash[%d]=%q want %q", gateway.GatewayID, i, got[i], want[i])
			}
		}
		node := projected.Nodes[index]
		access := node.Access
		if access.GrantID != gateway.Access.GrantID || access.DeviceID != "inst-fingerprint" ||
			access.Generation != gateway.Access.Generation || access.LeaseSeq != gateway.Access.LeaseSeq ||
			access.ExpiresAt != gateway.Access.NotAfter || access.Password != gateway.Access.Password {
			t.Fatalf("admission fields lost for %s: %+v", gateway.GatewayID, access)
		}
	}
	// The projection must own its slice: mutating the decoded source afterwards must not
	// change the transport parameters.
	catalog.Gateways[0].Access.VKHashes[0] = "mutated"
	if projected.Nodes[0].Access.VKHashes[0] == "mutated" {
		t.Fatal("vk_hashes must be copied, not aliased")
	}

	// Optional hashes: absent stays absent (fail-closed in the transport, no synthesis,
	// no invented fallback).
	empty := newMobileFeedFixture(t, 1, mobileFeedOptions{})
	emptyProjected, err := projectMobileCatalog(feedDecodeMe(t, empty.me), feedDecodeCatalog(t, empty), empty.fingerprint, "gw-0", now)
	if err != nil {
		t.Fatalf("catalog without optional vk_hashes must still project: %v", err)
	}
	if len(emptyProjected.Nodes[0].Access.VKHashes) != 0 {
		t.Fatalf("optional hashes must not be synthesized: %v", emptyProjected.Nodes[0].Access.VKHashes)
	}
}

func TestMobileProjectionDependencyErrors(t *testing.T) {
	now := time.Now().UTC()
	base := newMobileFeedFixture(t, 2, mobileFeedOptions{})
	me := feedDecodeMe(t, base.me)

	projected, err := projectMobileCatalog(me, feedDecodeCatalog(t, base), base.fingerprint, "gw-1", now)
	if err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
	if projected.V != 2 || projected.SubscriptionRef != mobileSubscriptionRef(base.fingerprint) ||
		projected.RegistrationID != base.fingerprint || projected.RefreshAfter != projected.IssuedAt ||
		projected.CatalogExpiresAt != feedStamp(base.validUntil) || projected.SubscriptionStatus != "active" {
		t.Fatalf("projected envelope wrong: %+v", projected)
	}
	if len(projected.Nodes) != 2 || projected.Nodes[1].Access.LeaseSeq != "2" || projected.Nodes[1].MaxWorkers != 36 {
		t.Fatalf("projected nodes wrong: %+v", projected.Nodes)
	}

	// The strict decoder already rejects a catalog without lease_seq; a manually built
	// response must still surface the explicit dependency instead of a silent zero.
	noLease := feedDecodeCatalog(t, base)
	noLease.Gateways[0].Access.LeaseSeq = ""
	if _, err := projectMobileCatalog(me, noLease, base.fingerprint, "gw-0", now); err == nil || err.Error() != "LEASE_SEQ_MISSING" {
		t.Fatalf("missing lease_seq must be an explicit dependency: %v", err)
	}
	if _, err := accountaccess.DecodeCatalogStrict([]byte(newMobileFeedFixture(t, 1, mobileFeedOptions{omitLeaseSeq: true}).catalogJSON())); err == nil {
		t.Fatal("accepted contract requires lease_seq: strict decode must reject its absence")
	}
	noWorkers := newMobileFeedFixture(t, 1, mobileFeedOptions{omitTargetWorkers: true})
	if _, err := projectMobileCatalog(feedDecodeMe(t, noWorkers.me), feedDecodeCatalog(t, noWorkers), noWorkers.fingerprint, "gw-0", now); err == nil || err.Error() != "TARGET_WORKERS_MISSING" {
		t.Fatalf("missing target_workers must be an explicit dependency: %v", err)
	}
	none := newMobileFeedFixture(t, 1, mobileFeedOptions{dataAccess: "none"})
	if _, err := projectMobileCatalog(feedDecodeMe(t, none.me), feedDecodeCatalog(t, none), none.fingerprint, "gw-0", now); err == nil || err.Error() != "RIGHT_DEADLINE_MISSING" {
		t.Fatalf("no data access must not project a right: %v", err)
	}
	nullCountry := newMobileFeedFixture(t, 2, mobileFeedOptions{countryNull: true})
	projected, err = projectMobileCatalog(feedDecodeMe(t, nullCountry.me), feedDecodeCatalog(t, nullCountry), nullCountry.fingerprint, "gw-0", now)
	if err != nil || projected.Nodes[0].CountryCode != "" || projected.Nodes[1].CountryCode != "XX" {
		t.Fatalf("null country must map to the display-safe empty value: %+v %v", projected, err)
	}
}

func TestMobileCatalogPublicationSupportsArbitraryGateways(t *testing.T) {
	now := time.Now().UTC()
	for _, count := range []int{3, 1000} {
		fixture := newMobileFeedFixture(t, count, mobileFeedOptions{})
		me := feedDecodeMe(t, fixture.me)
		catalog := feedDecodeCatalog(t, fixture)
		host := &feedHost{}
		controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {}), start: managedStart{MobileBaseURL: "https://mobile.invalid"}}
		projected, err := projectMobileCatalog(me, catalog, fixture.fingerprint, "gw-1", now)
		if err != nil {
			t.Fatalf("%d gateways: %v", count, err)
		}
		if err := controller.store.Apply(projected, "", fixture.fingerprint, now); err != nil {
			t.Fatalf("%d gateways store: %v", count, err)
		}
		controller.saved.SelectedNodeID = "gw-1"
		if err := controller.publishCatalogContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		message := host.waitMessage(t, "catalog")
		nodes, _ := message["nodes"].([]any)
		if len(nodes) != count {
			t.Fatalf("published %d of %d gateways", len(nodes), count)
		}
		if message["selected_node_id"] != "gw-1" || len(controller.store.Snapshot().Nodes) != count {
			t.Fatalf("selection/truncation wrong for %d gateways", count)
		}
		if message["subscription_status"] != "active" || message["catalog_expires_at"] != feedStamp(fixture.validUntil) {
			t.Fatalf("subscription display must come from the mobile entitlement: %v", message)
		}

		// A same-revision reorder keeps the selection by ID.
		reordered := append([]map[string]any(nil), fixture.gateways...)
		for left, right := 0, len(reordered)-1; left < right; left, right = left+1, right-1 {
			reordered[left], reordered[right] = reordered[right], reordered[left]
		}
		fixture.setCatalog("8", reordered)
		reorderedProjected, err := projectMobileCatalog(me, feedDecodeCatalog(t, fixture), fixture.fingerprint, "gw-1", now)
		if err != nil {
			t.Fatalf("reorder projection: %v", err)
		}
		if err := controller.store.Apply(reorderedProjected, "", fixture.fingerprint, now); err != nil {
			t.Fatalf("reorder store: %v", err)
		}
		if err := controller.publishCatalogContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		message = host.waitMessage(t, "catalog")
		if message["selected_node_id"] != "gw-1" || message["revision"] != "8" {
			t.Fatalf("selection must survive reorder/refresh by ID: %v", message)
		}
	}
}

func TestMobileRemovalClearsSelectionAndStops(t *testing.T) {
	now := time.Now().UTC()
	fixture := newMobileFeedFixture(t, 3, mobileFeedOptions{})
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {}), start: managedStart{MobileBaseURL: "https://mobile.invalid"}}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if err := controller.chooseNode(context.Background(), "gw-1"); err != nil {
		t.Fatal(err)
	}
	if controller.selectionID() != "gw-1" {
		t.Fatal("selection not committed")
	}
	stopCalled := false
	release := controller.registerMobileStop(func() { stopCalled = true }, nil)
	defer release()

	fixture.setCatalog("8", []map[string]any{
		mobileFeedGatewayBody(fixture.fingerprint, 0, fixture.validUntil, mobileFeedOptions{}),
		mobileFeedGatewayBody(fixture.fingerprint, 2, fixture.validUntil, mobileFeedOptions{}),
	})
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if !stopCalled {
		t.Fatal("removed selected gateway must stop the running child")
	}
	if controller.selectionID() != "" {
		t.Fatal("removed gateway must clear the selection explicitly")
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-1", now) == nil {
		t.Fatal("a removed gateway must never be startable")
	}
	message := host.waitMessage(t, "catalog")
	if message["selected_node_id"] != "" {
		t.Fatalf("host must see the explicit clear, got %v", message["selected_node_id"])
	}
	nodes, _ := message["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("removal refresh must publish the remaining gateways: %d", len(nodes))
	}
	decision := mobile.lastDecision()
	if decision == nil || !decision.StopDataPlane || decision.Reason != "SELECTED_NODE_REMOVED" {
		t.Fatalf("removal decision wrong: %+v", decision)
	}
}

func TestMobileSwitchTargetAdmissionFences(t *testing.T) {
	now := time.Now().UTC()
	fixture := newMobileFeedFixture(t, 3, mobileFeedOptions{})
	host := &feedHost{}
	controller := &managedController{bridge: newManagedBridge(host, "attempt", func() {}), start: managedStart{MobileBaseURL: "https://mobile.invalid"}}
	controller.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fixture.fingerprint)}
	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint, verifiedSig: make(chan struct{}, 1)}
	controller.mobile = mobile
	mobile.onVerified(feedDecodeMe(t, fixture.me), feedDecodeCatalog(t, fixture))
	if err := controller.chooseNode(context.Background(), "gw-0"); err != nil {
		t.Fatal(err)
	}
	active, err := managedNodeByID(controller.store.Snapshot(), "gw-0")
	if err != nil {
		t.Fatal(err)
	}
	target, err := managedNodeByID(controller.store.Snapshot(), "gw-2")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := mobile.prepareSwitchTarget(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("non-adjacent switch target refused: %v", err)
	}
	if prepared.NodeID != "gw-2" || prepared.Access.GrantID != "grant-2" {
		t.Fatalf("prepared target wrong: %+v", prepared)
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-2", now) != nil {
		t.Fatal("target must be admitted before start")
	}
	if !controller.mobileAdmissionFence(controller.store.Snapshot(), fixture.revision, prepared) {
		t.Fatal("prepared target must pass the vpn admission snapshot fence")
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-0", now) == nil {
		t.Fatal("the old selection must not keep admission during an in-flight switch")
	}
	if !controller.rollbackAllowed(active, fixture.fingerprint) {
		t.Fatal("a failed switch must roll back to the still-eligible active node")
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-0", now) != nil {
		t.Fatal("rollback must re-bind the active selection proof")
	}

	revoked := mobileFeedMeBody(mobileFeedOptions{bindingStatus: "revoked"}, now)
	revokedFixture := fixture
	revokedFixture.setMe(revoked)
	mobile.onVerified(feedDecodeMe(t, revoked), feedDecodeCatalog(t, revokedFixture))
	if _, err := mobile.prepareSwitchTarget(context.Background(), target, nil); err == nil || err.Error() != "BINDING_REVOKED" {
		t.Fatalf("revoked target must be refused before switching: %v", err)
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-0", now) != nil {
		t.Fatal("refused target must not disturb the committed selection")
	}

	// A late target after a selection change must not pass the store admission fence.
	restored := mobileFeedMeBody(mobileFeedOptions{}, now)
	revokedFixture.setMe(restored)
	revokedFixture.setCatalog("9", revokedFixture.gateways)
	mobile.onVerified(feedDecodeMe(t, restored), feedDecodeCatalog(t, revokedFixture))
	if err := controller.chooseNode(context.Background(), "gw-1"); err != nil {
		t.Fatal(err)
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-2", now) == nil {
		t.Fatal("a late target callback must not start after a new selection")
	}
	if controller.mobileAdmissionFence(controller.store.Snapshot(), fixture.revision, prepared) {
		t.Fatal("a late target must fail the mobile admission fence after a new selection")
	}
	if controller.store.Snapshot().ValidateNode(mobileSubscriptionRef(fixture.fingerprint), "", "gw-1", now) != nil {
		t.Fatal("the new selection must be startable")
	}
}

func TestMobileSynchronizeSeamAwaitsOneRunnerCycle(t *testing.T) {
	fixture := newMobileFeedFixture(t, 2, mobileFeedOptions{})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0", "gw-1")
	controller, _, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	host.waitMessage(t, "catalog")
	meBefore, catalogBefore := fixture.calls()
	if err := controller.synchronize(context.Background(), true); err != nil {
		t.Fatalf("mobile refresh seam failed: %v", err)
	}
	meAfter, catalogAfter := fixture.calls()
	if meAfter <= meBefore || catalogAfter <= catalogBefore {
		t.Fatalf("seam must run the single runner refresh cycle: me %d->%d catalog %d->%d", meBefore, meAfter, catalogBefore, catalogAfter)
	}
	cancel()
	<-errCh
}

func TestMobileBridgeWakeReasonRecoveryAfterSleep(t *testing.T) {
	plain := newManagedBridge(io.Discard, "attempt", func() {})
	readAndWait := func(bridge *managedBridge, input string) string {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go bridge.read(ctx, bufio.NewScanner(strings.NewReader(input)))
		select {
		case reason := <-bridge.wake:
			return reason
		case <-time.After(2 * time.Second):
			t.Fatal("wake reason never signalled")
			return ""
		}
	}
	wake := readAndWait(plain, `{"v":1,"attempt_id":"attempt","type":"device_wake","lifecycle_revision":1}`+"\n")
	if wake != "wake" {
		t.Fatalf("plain device_wake reason=%q", wake)
	}
	resumed := newManagedBridge(io.Discard, "attempt", func() {})
	recovery := readAndWait(resumed, `{"v":1,"attempt_id":"attempt","type":"device_sleep","lifecycle_revision":1}`+"\n"+
		`{"v":1,"attempt_id":"attempt","type":"device_wake","lifecycle_revision":2}`+"\n")
	if recovery != "recovery" {
		t.Fatalf("sleep->wake resume reason=%q", recovery)
	}
}

// Indefinite commercial right: /me 200 (perpetual, valid_until null, null deadline)
// must reach GET /gateways 200, project a finite proof and keep the real stop bound
// from the verified catalog validity without any wake or HTTP at expiry.
func TestMobileIndefiniteRightStartsAndStopsAtCatalogDeadline(t *testing.T) {
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{indefinite: true, validUntil: 400 * time.Millisecond})
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0")
	controller, bridge, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	stub := &mobileVPNStub{started: make(chan int, 4)}
	installMobileVPNStub(t, stub)

	host.waitMessage(t, "catalog")
	meCalls, catalogCalls := fixture.calls()
	if meCalls == 0 || catalogCalls == 0 {
		t.Fatalf("indefinite pair must reach /me and /gateways: me=%d catalog=%d", meCalls, catalogCalls)
	}
	bridge.selection <- "gw-0"
	index := stub.waitStarted(t)
	if decision := controller.mobile.lastDecision(); decision == nil || !decision.Admitted {
		t.Fatalf("indefinite right must admit: %+v", decision)
	}
	stub.waitCancelled(t, index)
	afterMe, afterCatalog := fixture.calls()
	if afterMe != meCalls || afterCatalog != catalogCalls {
		t.Fatalf("deadline stop must not need HTTP: me %d->%d catalog %d->%d", meCalls, afterMe, catalogCalls, afterCatalog)
	}
	snapshot := controller.store.Snapshot()
	if snapshot == nil || !snapshot.MobileAuthority() {
		t.Fatal("indefinite verified pair must leave a mobile-authority snapshot")
	}
	if snapshot.SubscriptionExpiresAt != "" {
		t.Fatalf("null valid_until must not synthesize a business expiry: %q", snapshot.SubscriptionExpiresAt)
	}
	cancel()
	<-errCh
}

func TestMobileIndefiniteProofDeadlineStaysFinite(t *testing.T) {
	now := time.Now().UTC()
	fixture := newMobileFeedFixture(t, 2, mobileFeedOptions{indefinite: true})
	me := feedDecodeMe(t, fixture.me)
	catalog := feedDecodeCatalog(t, fixture)
	projected, err := projectMobileCatalog(me, catalog, fixture.fingerprint, "gw-0", now)
	if err != nil {
		t.Fatalf("indefinite projection rejected: %v", err)
	}
	if projected.CatalogExpiresAt != feedStamp(fixture.validUntil) || projected.SubscriptionExpiresAt != "" {
		t.Fatalf("projected envelope must keep the catalog bound and no business expiry: %+v", projected)
	}
	// Store admission proves the finite proof chain (zero or extended deadlines fail).
	controller := &managedController{bridge: newManagedBridge(&feedHost{}, "attempt", func() {}), start: managedStart{MobileBaseURL: "https://mobile.invalid"}}
	if err := controller.store.Apply(projected, "", fixture.fingerprint, now); err != nil {
		t.Fatalf("indefinite proof must satisfy mobile admission: %v", err)
	}
	validUntil, err := wlbs.UTC(catalog.ValidUntil)
	if err != nil {
		t.Fatal(err)
	}

	mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint}
	mobile.me = &me
	mobile.catalog = &catalog
	deadline := mobile.provenDeadlineLocked("gw-0")
	if deadline.IsZero() || deadline.After(validUntil) {
		t.Fatalf("indefinite stop bound must stay finite and <= catalog valid_until: %v", deadline)
	}
	if !deadline.Equal(validUntil) {
		t.Fatalf("fixture leases equal the catalog validity, bound=%v want %v", deadline, validUntil)
	}

	// A node lease that outlives the catalog may raise only the proof fence (handled by
	// the store proof), never the real stop timer.
	catalog.Gateways[0].Access.NotAfter = feedStamp(validUntil.Add(time.Minute))
	if deadline := mobile.provenDeadlineLocked("gw-0"); !deadline.Equal(validUntil) {
		t.Fatalf("node lease must not extend the finite catalog stop bound: %v", deadline)
	}
	// An earlier node lease is the binding minimum even with a finite entitlement.
	early := feedDecodeMe(t, newMobileFeedFixture(t, 1, mobileFeedOptions{deadline: 30 * time.Minute}).me)
	mobile.me = &early
	catalog.Gateways[0].Access.NotAfter = feedStamp(validUntil)
	if deadline := mobile.provenDeadlineLocked("gw-0"); deadline.IsZero() || deadline.After(validUntil) {
		t.Fatalf("finite entitlement bound wrong: %v", deadline)
	}
}

func TestMobileProjectionRejectsInconsistentIndefiniteShapes(t *testing.T) {
	now := time.Now().UTC()
	fixture := newMobileFeedFixture(t, 1, mobileFeedOptions{indefinite: true})
	me := feedDecodeMe(t, fixture.me)
	catalog := feedDecodeCatalog(t, fixture)
	if _, err := projectMobileCatalog(me, catalog, fixture.fingerprint, "gw-0", now); err != nil {
		t.Fatalf("indefinite fixture must project: %v", err)
	}

	notPerpetual := me
	notPerpetual.Entitlement.PerpetualCommercial = false
	if _, err := projectMobileCatalog(notPerpetual, catalog, fixture.fingerprint, "gw-0", now); err == nil || err.Error() != "RIGHT_DEADLINE_MISSING" {
		t.Fatalf("perpetual false with null deadline must fail closed: %v", err)
	}
	withValidUntil := me
	validUntil := feedStamp(now.Add(24 * time.Hour))
	withValidUntil.Entitlement.ValidUntil = &validUntil
	if _, err := projectMobileCatalog(withValidUntil, catalog, fixture.fingerprint, "gw-0", now); err == nil || err.Error() != "RIGHT_DEADLINE_MISSING" {
		t.Fatalf("valid_until with null deadline must fail closed: %v", err)
	}
	onboarding := me
	onboarding.GrantResolution.DataAccess = "onboarding_hour"
	if _, err := projectMobileCatalog(onboarding, catalog, fixture.fingerprint, "gw-0", now); err == nil || err.Error() != "RIGHT_DEADLINE_MISSING" {
		t.Fatalf("onboarding with null deadline must fail closed: %v", err)
	}
}
