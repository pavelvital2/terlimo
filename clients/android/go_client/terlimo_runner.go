package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/dtls/v3"
	"wg-turn-client/wlbs"
)

var managedCaptchaOutput func(string, string, string, string, time.Duration)

type managedSaved struct {
	Subscription   string                 `json:"subscription_ref"`
	Installation   string                 `json:"installation_id"`
	Pending        *wlbs.PendingOperation `json:"pending,omitempty"`
	Catalog        *wlbs.Catalog          `json:"catalog,omitempty"`
	SelectedNodeID string                 `json:"selected_node_id,omitempty"`
	Completed      *managedCompletion     `json:"completed_operation,omitempty"`
}

// One bounded receipt, not an operation history. It preserves an immutable
// completion even when a newer catalog is subsequently fetched for admission.
type managedCompletion struct {
	RequestID string        `json:"request_id"`
	Catalog   *wlbs.Catalog `json:"catalog"`
}

func decodeManagedSaved(raw []byte, saved *managedSaved) error {
	if err := wlbs.StrictJSON(raw, saved); err != nil {
		if wlbs.IsCountryCodeWireError(err) {
			return errors.New("BAD_CATALOG")
		}
		return errors.New("SAVED_STATE_INVALID")
	}
	return nil
}

type managedController struct {
	bridge         *managedBridge
	start          managedStart
	link           *wlbs.Link
	public         []byte
	mu             sync.Mutex
	saved          managedSaved
	store          wlbs.CatalogStore
	signSem        chan struct{}
	vpnDiagnostics *managedDiagnostics
	mobile         *managedMobile
	mobileStops    map[uint64]mobileStopEntry
	mobileStopSeq  uint64
	probeEpoch     atomic.Uint64
	// pendingExplicitGateway is the one after-consent gateway of the pre-admission
	// explicit_connect command. It is armed only by the explicit branch of runMobile
	// (never by a background/resume path), consumed at most once at the start of
	// runSelected, and cleared when the mobile attempt ends.
	pendingExplicitGateway string
}

// nextProbeEpoch invalidates every in-flight probe result. It is bumped on a
// newly accepted probe, probeStop, selection and attempt teardown so a stale
// same-node result can never be forwarded to the host.
func (c *managedController) nextProbeEpoch() uint64 { return c.probeEpoch.Add(1) }

// probeResultIsFresh reports whether the goroutine that captured epoch is still
// the latest accepted probe.
func (c *managedController) probeResultIsFresh(epoch uint64) bool {
	return c.probeEpoch.Load() == epoch
}

// mobileMode reports whether the start frame selected the mobile account-access path.
func (c *managedController) mobileMode() bool { return c.start.MobileBaseURL != "" }

func (c *managedController) selectionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saved.SelectedNodeID
}

// clearSelection drops a removed selected gateway explicitly; a removal never silently
// falls back to another node.
func (c *managedController) clearSelection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saved.SelectedNodeID = ""
}

// armPendingExplicitGateway records the after-consent gateway an explicit_connect
// command was started for, so the post-start selection can use the SAME gateway
// without a second user tap. The first armed value wins: a duplicate callback (even
// one naming another gateway after the hour already started) never replaces it or
// creates a second pending. An empty key never arms (the backend assigned as today).
func (c *managedController) armPendingExplicitGateway(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingExplicitGateway == "" {
		c.pendingExplicitGateway = id
	}
}

// consumePendingExplicitGateway returns the pending explicit gateway and clears it,
// so exactly one post-start selection can carry it.
func (c *managedController) consumePendingExplicitGateway() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.pendingExplicitGateway
	c.pendingExplicitGateway = ""
	return id
}

func (c *managedController) clearPendingExplicitGateway() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingExplicitGateway = ""
}

// registerMobileStop tracks one active mobile data-plane cancel while its vpn runtime
// lives. The returned release removes exactly that registration.
type mobileStopEntry struct {
	cancel      context.CancelFunc
	diagnostics *managedDiagnostics
}

// noteSwitchCancelSource / noteRevokedCancelSource capture the CALLER's attempt
// diagnostics, so an async old-attempt callback can never mark a replaced attempt.
func noteSwitchCancelSource(diagnostics *managedDiagnostics, cancel context.CancelFunc) {
	diagnostics.noteCancelSource(vpnCancelSwitch)
	emitRuntimeCancel(vpnCancelSwitch)
	cancel()
}

func noteRevokedCancelSource(diagnostics *managedDiagnostics, parentCancel context.CancelFunc) {
	diagnostics.noteCancelSource(vpnCancelRevoked)
	emitRuntimeCancel(vpnCancelRevoked)
	parentCancel()
}

func (c *managedController) registerMobileStop(cancel context.CancelFunc, diagnostics *managedDiagnostics) func() {
	c.mu.Lock()
	if c.mobileStops == nil {
		c.mobileStops = map[uint64]mobileStopEntry{}
	}
	id := c.mobileStopSeq
	c.mobileStopSeq++
	c.mobileStops[id] = mobileStopEntry{cancel: cancel, diagnostics: diagnostics}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.mobileStops, id)
		c.mu.Unlock()
	}
}

// stopMobileDataPlane cancels every active mobile data-plane runtime. It is called from
// the admission callback and from the proven-deadline timer; it never fetches anything.
func (c *managedController) stopMobileDataPlane() {
	c.mu.Lock()
	stops := make([]mobileStopEntry, 0, len(c.mobileStops))
	for _, stop := range c.mobileStops {
		stops = append(stops, stop)
	}
	c.mu.Unlock()
	for _, stop := range stops {
		// Host cancel action / proven stop: record on THIS attempt before invoking cancel.
		stop.diagnostics.noteCancelSource(vpnCancelHostStop)
		emitRuntimeCancel(vpnCancelHostStop)
		stop.cancel()
	}
}

func runTerlimoMain() {
	// Upstream diagnostics contain raw provider errors: this private mode emits
	// only typed codes. It never executes the legacy config-print/file branch.
	log.SetOutput(io.Discard)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), managedBridgeLimit)
	if !scanner.Scan() {
		return
	}
	var start managedStart
	if wlbs.StrictJSON(scanner.Bytes(), &start) != nil || start.V != 1 || start.Type != "start" || len(start.AttemptID) < 1 || len(start.AttemptID) > 128 {
		return
	}
	out, err := managedBridgeOutput(os.Stdout)
	if err != nil {
		return
	}
	defer out.Close()
	b := newManagedBridge(out, start.AttemptID, cancel)
	defer b.send(bridgeMessage{"type": "stopped"})
	go b.read(ctx, scanner)
	managedCaptchaOutput = func(id, mode, redirect, token string, timeout time.Duration) {
		b.send(bridgeMessage{"type": "state", "state": "WaitingUser"})
		b.send(bridgeMessage{"type": "captcha", "request_id": id, "mode": mode, "redirect_uri": redirect, "session_token": token, "deadline_unix_ms": time.Now().Add(timeout).UnixMilli()})
	}
	c := &managedController{bridge: b, start: start, signSem: make(chan struct{}, 1)}
	if err := c.run(ctx, cancel); err != nil && !errors.Is(err, context.Canceled) {
		if c.vpnDiagnostics != nil {
			_ = b.send(c.vpnDiagnostics.terminalMessage())
		}
		b.send(bridgeMessage{"type": "error", "code": managedCode(err)})
	}
}

// Java ProcessBuilder owns the peer end. Duplicate the inherited stdout so this
// process explicitly owns a pollable descriptor whose deadlines can cancel a
// blocked bridge write; never fall back to an unbounded write.
func managedBridgeOutput(inherited *os.File) (*os.File, error) {
	info, err := inherited.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return nil, errors.New("BRIDGE_STDOUT_INVALID")
	}
	fd, err := syscall.Dup(int(inherited.Fd()))
	if err != nil {
		return nil, err
	}
	if err = syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	owned := os.NewFile(uintptr(fd), "terlimo-managed-stdout")
	if owned == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("BRIDGE_STDOUT_INVALID")
	}
	if err = owned.SetWriteDeadline(time.Time{}); err != nil {
		_ = owned.Close()
		return nil, err
	}
	return owned, nil
}

func managedCode(err error) string {
	if err == nil {
		return ""
	}
	var we *wlbs.Error
	if errors.As(err, &we) {
		return safeManagedCode(we.Code)
	}
	return safeManagedCode(err.Error())
}
func safeManagedCode(s string) string {
	if len(s) == 0 || len(s) > 64 {
		return "TRANSPORT_FAILED"
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && r != '_' && (r < '0' || r > '9') {
			return "TRANSPORT_FAILED"
		}
	}
	return s
}

func (c *managedController) state(name string) {
	_ = c.stateContext(context.Background(), name)
}
func (c *managedController) stateContext(ctx context.Context, name string) error {
	return c.bridge.sendContext(ctx, bridgeMessage{"type": "state", "state": name})
}
func (c *managedController) sign(ctx context.Context, t []byte) ([]byte, error) {
	sig, err := c.bridge.sign(ctx, t)
	if err == nil {
		err = wlbs.VerifyTranscript(c.public, t, sig)
	}
	return sig, err
}
func (c *managedController) saveLocked(ctx context.Context) error {
	if c.mobileMode() {
		// Mobile never persists or restores the legacy wlbs subscription/catalog/blob;
		// the accountaccess receipt namespace owns durable mobile state.
		return nil
	}
	raw, e := json.Marshal(c.saved)
	if e != nil {
		return e
	}
	return c.bridge.persist(ctx, raw)
}
func (c *managedController) pending(ctx context.Context, p wlbs.PendingOperation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saved.Pending != nil && *c.saved.Pending != p {
		return errors.New("OPERATION_PENDING")
	}
	previous := c.saved.Pending
	c.saved.Pending = &p
	if e := c.saveLocked(ctx); e != nil {
		c.saved.Pending = previous
		return e
	}
	return nil
}

func (c *managedController) run(ctx context.Context, cancel context.CancelFunc) error {
	if c.mobileMode() {
		public, e := wlbs.DecodeBinary(c.start.PublicKey, -1)
		if e != nil {
			return e
		}
		c.public = public
		finger, e := wlbs.InstallationID(c.public)
		if e != nil || finger != c.start.InstallationID {
			return errors.New("KEY_UNAVAILABLE")
		}
		c.state("ImportVerified")
		c.bridge.send(bridgeMessage{"type": "imported"})
		handle, e := strconv.ParseUint(c.start.PhysicalNetwork, 10, 64)
		if e != nil {
			return errors.New("PHYSICAL_NETWORK_UNAVAILABLE")
		}
		if e = bindManagedNetwork(handle); e != nil {
			return e
		}
		setCaptchaMode("auto")
		setVKCallsPreflight(true)
		return c.runMobile(ctx, cancel, finger)
	}
	if len(c.start.Issuers) == 0 {
		return errors.New("ISSUER_NOT_PROVISIONED")
	}
	issuers := make(map[string][]byte, len(c.start.Issuers))
	for k, v := range c.start.Issuers {
		p, e := wlbs.DecodeBinary(v, -1)
		if e != nil {
			return errors.New("TRUST_FAILED")
		}
		issuers[k] = p
	}
	link, e := wlbs.VerifyLink(strings.TrimSpace(c.start.Link), issuers, "test", time.Now())
	if e != nil {
		return e
	}
	c.link = link
	c.public, e = wlbs.DecodeBinary(c.start.PublicKey, -1)
	if e != nil {
		return e
	}
	finger, e := wlbs.InstallationID(c.public)
	if e != nil || finger != c.start.InstallationID {
		return errors.New("KEY_UNAVAILABLE")
	}
	c.saved = managedSaved{Subscription: link.SubscriptionRef, Installation: finger}
	if c.start.State != "" {
		raw, e := base64.RawURLEncoding.DecodeString(c.start.State)
		if e != nil {
			return errors.New("SAVED_STATE_INVALID")
		}
		if e = decodeManagedSaved(raw, &c.saved); e != nil {
			return e
		}
		if c.saved.Installation != finger {
			return errors.New("KEY_UNAVAILABLE")
		}
		if c.saved.Subscription != link.SubscriptionRef {
			return errors.New("DIFFERENT_SUBSCRIPTION")
		}
		if c.saved.Catalog != nil {
			if e = restoreManagedCatalog(&c.store, c.saved.Catalog, link.SubscriptionRef, time.Now()); e != nil {
				return e
			}
		}
	}
	c.state("ImportVerified")
	c.bridge.send(bridgeMessage{"type": "imported"})
	handle, e := strconv.ParseUint(c.start.PhysicalNetwork, 10, 64)
	if e != nil {
		return errors.New("PHYSICAL_NETWORK_UNAVAILABLE")
	}
	if e = bindManagedNetwork(handle); e != nil {
		return e
	}
	setCaptchaMode("auto")
	setVKCallsPreflight(true)
	if e = c.synchronize(ctx, false); e != nil {
		return e
	}
	c.publishCatalog()
	return c.runSelected(ctx, cancel)
}

// runMobile owns the mobile lifecycle: the single mobile runner is started, the first
// projectable verified pair populates the store before the host catalog is published,
// and the rest of the managed flow (selection, probe, vpn) is shared with the legacy
// path. A pair without a right yet keeps the runner cycling instead of ending the
// attempt; catalog and selection start with the first projectable snapshot.
func (c *managedController) runMobile(ctx context.Context, cancel context.CancelFunc, fingerprint string) error {
	// The actual mobile attempt starts here: one monotonic base for every per-attempt
	// cycle/VK stage marker (set before any attempt goroutine spawns). A finished
	// attempt must never leave its explicit gateway pending for anything else.
	defer c.clearPendingExplicitGateway()
	diagMarkAttemptStart()
	mobile, e := newManagedMobile(c.start, c.public, c.sign, c.bridge, c)
	if e != nil {
		return e
	}
	c.mobile = mobile
	c.link = &wlbs.Link{SubscriptionRef: mobileSubscriptionRef(fingerprint)}
	go mobile.run(ctx, c.bridge)
	// The pre-admission wait must not require a catalogue: a fresh installation with
	// data_access=none projects no gateways, yet an explicit after-consent first
	// Connect command must still be handled (the assigned gateway comes from the
	// intent ready poll, not from a selection). The loop stays until a projectable
	// pair lands or the attempt context ends; an explicit failure is diagnostic-only,
	// so a failed start can never end the attempt or fall back to another transport.
	for !mobile.readySnapshot() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-mobile.verifiedSignal():
		case gatewayKey := <-c.bridge.explicit:
			onboardingCtx, stopOnboarding := context.WithTimeout(ctx, mobileHTTPTimeout)
			activated, onboardingErr := mobile.explicitConnect(onboardingCtx, gatewayKey)
			stopOnboarding()
			if onboardingErr == nil {
				if activated {
					// The hour is server-confirmed for THIS attempt: remember the
					// after-consent gateway so runSelected passes the SAME gateway
					// through the normal admission/VPN gates without a second tap.
					// explicitConnect resolves an empty key to the persisted
					// selection; mirror that resolution so the pending id is the
					// effective one. It is never resolved from a background path.
					gateway := gatewayKey
					if gateway == "" {
						gateway = c.selectionID()
					}
					c.armPendingExplicitGateway(gateway)
				}
				// The started hour becomes visible on the next verified /me; trigger
				// exactly one refresh through the single runner owner. explicitConnect
				// already emitted its single fixed terminal diagnostic token.
				mobile.runner.Trigger("manual")
			}
		}
	}
	// The store is populated by the first verified pair before the host list/select
	// event is emitted; an oversized/blocked event is an explicit failure, never a
	// silent truncation.
	if e = c.publishCatalogContext(ctx); e != nil {
		return e
	}
	return c.runSelected(ctx, cancel)
}

// runSelected is the shared post-catalog flow: exact node selection, probe, one Connect
// budget covering admission refresh and the real vpn start.
func (c *managedController) runSelected(ctx context.Context, cancel context.CancelFunc) error {
	// One after-consent explicit gateway (if any) is consumed exactly once: the
	// post-start selection then uses the SAME gateway the hour was started for and
	// never waits for a second host tap. Every other path keeps waitForNode.
	explicitNodeID := c.consumePendingExplicitGateway()
	nodeID := explicitNodeID
	var e error
	if explicitNodeID == "" {
		nodeID, e = c.waitForNode(ctx)
		if e != nil {
			return e
		}
	}
	// One Connect budget covers admission refresh, workers and host readiness.
	// The established runtime retains ctx, not this preparation deadline.
	connectCtx, stopConnect := context.WithTimeout(ctx, 15*time.Second)
	defer stopConnect()
	catalog := c.store.Snapshot()
	if _, e = c.probeForNode(nodeID); e != nil {
		return e
	}
	if explicitNodeID != "" {
		// The verified mobile catalog is proof-bound to one node, so the explicit
		// gateway must be bound (chooseNode → mobile bindSelection) before the
		// existing ValidateNode/synchronize gates: bindSelection re-proves the store
		// for exactly this id and publishes the selection for the host. An absent or
		// not-admitted gateway is a bounded refusal here: no fallback, no second
		// explicit onboarding.
		if _, e = managedNodeByID(c.store.Snapshot(), explicitNodeID); e != nil {
			return e
		}
		if e = c.chooseNode(ctx, explicitNodeID); e != nil {
			return e
		}
		c.publishCatalog()
		catalog = c.store.Snapshot()
	}
	if catalog == nil || catalog.ValidateNode(c.link.SubscriptionRef, "", nodeID, time.Now()) != nil {
		// Selection may occur after the displayed lease expires. Reuse the
		// existing registration, never start VPN from the displayed snapshot.
		if e = c.synchronize(connectCtx, true); e != nil {
			if errors.Is(connectCtx.Err(), context.DeadlineExceeded) {
				return errors.New("VPN_SETUP_TIMEOUT")
			}
			return e
		}
		catalog = c.store.Snapshot()
		if catalog == nil || catalog.ValidateNode(c.link.SubscriptionRef, "", nodeID, time.Now()) != nil {
			return errors.New("CATALOG_EXPIRED")
		}
	}
	node, e := managedNodeByID(catalog, nodeID)
	if e != nil {
		return e
	}
	if _, e = c.probeForNode(node.NodeID); e != nil {
		return e
	}
	terminal := make(chan error, 1)
	err := managedVPNApply(c, ctx, cancel, node, catalog.RegistrationID, nil, nil, terminal, connectCtx)
	if errors.Is(err, errManagedRuntimeHandedOff) {
		select {
		case err = <-terminal:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	return err
}

// onboardingRefreshMobile is the mobile side used by the explicit-Connect caller
// branch; it keeps the optional onboarding step separable from the accepted
// admission path.
type onboardingRefreshMobile interface {
	explicitConnect(ctx context.Context, gatewayKey string) (bool, error)
	awaitOnboardingRefresh(ctx context.Context) error
}

// runExplicitOnboarding runs the optional explicit onboarding step for one explicit
// selection. An explicitConnect error is diagnostic-only (the caller continues to the
// accepted paid/trial/retained admission path) unless the parent context is canceled;
// only an activated attempt waits for the post-start refresh outcome, and only that
// wait's error is returned.
func (c *managedController) runExplicitOnboarding(parentCtx, onboardingCtx context.Context, gatewayKey string, mobile onboardingRefreshMobile) error {
	activated, err := mobile.explicitConnect(onboardingCtx, gatewayKey)
	if err != nil {
		// parentCtx, not the child 15s budget: an optional onboarding-hour error
		// (including the child deadline) stays diagnostic-only while the parent runs.
		if parentCtx.Err() != nil {
			return parentCtx.Err()
		}
		return nil
	}
	if !activated {
		// A canceled parent is never reported as success.
		if parentCtx.Err() != nil {
			return parentCtx.Err()
		}
		return nil
	}
	// The post-start wait uses the same onboardingCtx; its deadline/error is terminal.
	return mobile.awaitOnboardingRefresh(onboardingCtx)
}

// managedController must satisfy nothing extra here; the branch is method-local.
func (c *managedController) synchronize(ctx context.Context, refresh bool) (resultErr error) {
	return c.synchronizeTarget(ctx, refresh, "")
}

func (c *managedController) synchronizeTarget(ctx context.Context, refresh bool, targetID string) (resultErr error) {
	if c.mobileMode() {
		if c.mobile == nil {
			return errors.New("MOBILE_STATE_UNAVAILABLE")
		}
		// The mobile runner stays the single refresh owner; the seam only triggers it
		// and waits for the next verified, projected snapshot.
		return c.mobile.synchronize(ctx, refresh)
	}
	diagnostic := newBootstrapDiagnostic(ctx)
	ctx = context.WithValue(ctx, bootstrapDiagnosticKey{}, diagnostic)
	_ = c.stateContext(ctx, "BootstrapConnecting")
	ep := c.link.Bootstrap[0]
	conn, closeConn, e := DialBootstrapTransport(ctx, BootstrapTransportConfig{Peer: &net.UDPAddr{IP: net.ParseIP(ep.PeerIP), Port: ep.DTLSPort}, Password: c.link.BootstrapSecret, Hashes: c.link.VKHashes, Pin: ep.DTLSSPKISHA256, StreamID: 0})
	if e != nil {
		_ = c.bridge.send(diagnostic.finish(e))
		return e
	}
	// Freeze before cleanup, but preserve cleanup-before-output ordering. Even a
	// blocked host pipe must not hold an established transport open for telemetry.
	defer func() {
		diagnostic.complete(resultErr, closeConn, func(message bridgeMessage) error {
			return c.bridge.sendContext(ctx, message)
		})
	}()
	client := &wlbs.BootstrapClient{RPC: &wlbs.RPC{Conn: conn, ObserveIO: diagnostic.observeIO}, Signer: c.sign, CredentialID: c.link.CredentialID, PublicKeySPKI: c.public, Persist: c.pending}
	return c.synchronizeClientTargetRound(ctx, client, refresh, 3, targetID == "", targetID)
}

type managedBootstrapClient interface {
	Register(context.Context) ([]byte, *wlbs.PendingOperation, error)
	Refresh(context.Context, wlbs.RefreshPayload) ([]byte, *wlbs.PendingOperation, error)
	SyncAccess(context.Context, string) ([]byte, *wlbs.PendingOperation, error)
	Status(context.Context, wlbs.PendingOperation, string) ([]byte, error)
	Resume(context.Context, wlbs.PendingOperation) ([]byte, error)
	Catalog(context.Context, string, string) ([]byte, error)
}

func (c *managedController) syncAccessThenCatalog(ctx context.Context, client managedBootstrapClient, old *wlbs.Catalog, registration string) ([]byte, error) {
	_ = c.stateContext(ctx, "SyncingAccess")
	raw, pending, err := client.SyncAccess(ctx, registration)
	if err != nil {
		return nil, err
	}
	completed, err := managedCatalogResponse(raw)
	if err != nil {
		return nil, err
	}
	if completed.RegistrationID != registration {
		return nil, errors.New("BAD_CATALOG")
	}
	if err = restoreManagedCatalog(&wlbs.CatalogStore{}, completed, c.link.SubscriptionRef, time.Now()); err != nil {
		return nil, err
	}
	if err = sameManagedGrant(old, completed); err != nil {
		return nil, err
	}
	if pending == nil {
		return nil, errors.New("BAD_MESSAGE")
	}
	// Keep the exact sync mutation and immutable completion durable until an
	// explicit CATALOG response has also been validated and persisted.
	if err = c.recordCompletion(ctx, pending.RequestID, completed); err != nil {
		return nil, err
	}
	return client.Catalog(ctx, registration, "")
}

// At most three reconciliation/refresh rounds per bootstrap connection. RPC
// retries remain separately bounded and pending mutations retain exact bytes.
func (c *managedController) synchronizeClient(ctx context.Context, client managedBootstrapClient, refresh bool, rounds int) error {
	// Legacy focused tests exercise reconciliation without the new-update
	// trigger. Production bootstrap enters synchronizeClientRound with it set.
	return c.synchronizeClientRound(ctx, client, refresh, rounds, false)
}

func (c *managedController) synchronizeClientRound(ctx context.Context, client managedBootstrapClient, refresh bool, rounds int, allowSync bool) error {
	return c.synchronizeClientTargetRound(ctx, client, refresh, rounds, allowSync, "")
}

func (c *managedController) synchronizeClientTargetRound(ctx context.Context, client managedBootstrapClient, refresh bool, rounds int, allowSync bool, targetID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rounds <= 0 {
		return errors.New("RETRY_EXHAUSTED")
	}
	var e error
	c.mu.Lock()
	pending := c.saved.Pending
	old := c.saved.Catalog
	selectedID := managedSelectionID(old, c.saved.SelectedNodeID)
	c.mu.Unlock()
	// An explicit active replacement renews its target without committing a
	// different saved/active selection before readiness and ownership handoff.
	if targetID != "" {
		selectedID = targetID
	}
	selectedExpired := false
	if selected, err := managedNodeByID(old, selectedID); err == nil {
		selectedExpired = !selected.Access.ValidAt(time.Now())
	}
	registration := ""
	if old != nil {
		registration = old.RegistrationID
	}
	var raw []byte
	didSyncCatalog := false
	if pending != nil {
		_ = c.stateContext(ctx, "ResolvingOperation")
		resumed := false
		raw, e = client.Status(ctx, *pending, registration)
		if managedCode(e) == "OPERATION_UNKNOWN" {
			raw, e = client.Resume(ctx, *pending)
			resumed = true
		}
		if e == nil {
			var state struct {
				Status string `json:"status"`
			}
			if wlbs.StrictJSON(raw, &state) != nil {
				return errors.New("BAD_MESSAGE")
			}
			if state.Status == "unknown" {
				if _, check := managedCatalogResponse(raw); managedCode(check) != "OPERATION_UNKNOWN" {
					return errors.New("BAD_MESSAGE")
				}
				raw, e = client.Resume(ctx, *pending)
				resumed = true
				if e == nil && wlbs.StrictJSON(raw, &state) != nil {
					return errors.New("BAD_MESSAGE")
				}
			}
			if state.Status == "failed" {
				var failed wlbs.OperationStatus
				if wlbs.StrictJSON(raw, &failed) != nil || failed.Validate() != nil {
					return errors.New("BAD_MESSAGE")
				}
				if e = c.clearPending(ctx); e != nil {
					return e
				}
				if allowSync && pending.Op != "register" && pending.Op != "sync_access" && registration != "" && failed.Code != "DEVICE_REVOKED" && failed.Code != "SUBSCRIPTION_EXPIRED" {
					// A terminal older operation is resolved. Preserve the one-shot
					// update intent instead of silently falling back to a catalog read.
					raw, e = c.syncAccessThenCatalog(ctx, client, old, registration)
					didSyncCatalog = true
				} else {
					if failed.Code != "LEASE_CONFLICT" || registration == "" {
						return errors.New(safeManagedCode(failed.Code))
					}
					raw, e = client.Catalog(ctx, registration, "")
				}
			}
			if state.Status == "complete" || (resumed && state.Status == "ok") {
				completed, parseErr := managedCatalogResponse(raw)
				if parseErr != nil {
					return parseErr
				}
				if registration != "" && completed.RegistrationID != registration {
					return errors.New("BAD_CATALOG")
				}
				if parseErr = restoreManagedCatalog(&wlbs.CatalogStore{}, completed, c.link.SubscriptionRef, time.Now()); parseErr != nil {
					return parseErr
				}
				if old != nil {
					if parseErr = sameManagedGrant(old, completed); parseErr != nil {
						return parseErr
					}
				}
				// STATUS may replay an old immutable result. Persist that exact
				// receipt and selected ID with pending still intact BEFORE a new
				// metadata request. Never roll the live version fence backwards.
				if parseErr = c.recordCompletion(ctx, pending.RequestID, completed); parseErr != nil {
					return parseErr
				}
				if allowSync && pending.Op != "register" && pending.Op != "sync_access" {
					// The older mutation is durably complete. Clear only that resolved
					// pending record before admitting the new immutable sync request.
					if parseErr = c.clearPending(ctx); parseErr != nil {
						return parseErr
					}
					raw, e = c.syncAccessThenCatalog(ctx, client, old, completed.RegistrationID)
					didSyncCatalog = true
				}
				stale := completed.Validate(c.link.SubscriptionRef, registration, time.Now()) != nil
				if old != nil {
					cmp, cmpErr := wlbs.CompareDecimal(completed.Revision, old.Revision)
					if cmpErr != nil {
						return cmpErr
					}
					stale = stale || cmp < 0
				}
				if e == nil && !didSyncCatalog && (stale || pending.Op == "sync_access") {
					registration = completed.RegistrationID
					raw, e = client.Catalog(ctx, registration, "")
				}
			}
			if state.Status == "ok" && !resumed {
				return errors.New("BAD_MESSAGE")
			}
		}
	} else if registration == "" {
		_ = c.stateContext(ctx, "Registering")
		raw, _, e = client.Register(ctx)
	} else if allowSync {
		raw, e = c.syncAccessThenCatalog(ctx, client, old, registration)
	} else if refresh || selectedExpired {
		if old == nil || len(old.Nodes) == 0 || len(old.Nodes) > 2 {
			return errors.New("BAD_CATALOG")
		}
		if e = old.ValidateRefreshMetadata(c.link.SubscriptionRef, registration, time.Now()); e != nil {
			if managedCode(e) != "SUBSCRIPTION_EXPIRED" {
				return e
			}
			// A cached subscription deadline can be stale after an owner
			// renewal. Only an authenticated Catalog may establish that.
			raw, e = client.Catalog(ctx, registration, "")
		} else {
			// Only the selected grant is renewed. Other expired grants remain
			// authenticated metadata and never block admission of this node.
			n, pickErr := managedNodeByID(old, selectedID)
			if pickErr != nil {
				return pickErr
			}
			raw, _, e = client.Refresh(ctx, wlbs.RefreshPayload{RegistrationID: registration, NodeID: n.NodeID, GrantID: n.Access.GrantID, ExpectedGeneration: n.Access.Generation, ExpectedLeaseSeq: n.Access.LeaseSeq})
		}
	} else {
		raw, e = client.Catalog(ctx, registration, "")
	}
	if e != nil {
		if managedCode(e) == "LEASE_CONFLICT" {
			// Never overwrite the conflicting operation. Resolve its durable
			// status before reading Catalog and considering a new mutation ID.
			c.mu.Lock()
			hasPending := c.saved.Pending != nil
			c.mu.Unlock()
			if hasPending {
				return c.synchronizeClientTargetRound(ctx, client, false, rounds-1, allowSync, targetID)
			}
		}
		return e
	}
	cat, e := managedCatalogResponse(raw)
	if e != nil {
		return e
	}
	if old != nil {
		if e = sameManagedGrant(old, cat); e != nil {
			return e
		}
	}
	// Full authenticated metadata may be expired, but must pass the same
	// revision/lease fences. Keep it out of the live store until fresh.
	metadata := &wlbs.CatalogStore{}
	if old != nil {
		if e = restoreManagedCatalog(metadata, old, c.link.SubscriptionRef, time.Now()); e != nil {
			return e
		}
	}
	if e = metadata.ApplyRefreshMetadata(cat, c.link.SubscriptionRef, registration, time.Now()); e != nil {
		return e
	}
	needsSelectedRefresh := false
	if selected, err := managedNodeByID(cat, selectedID); err == nil {
		needsSelectedRefresh = !selected.Access.ValidAt(time.Now())
	}
	if cat.Validate(c.link.SubscriptionRef, registration, time.Now()) != nil || needsSelectedRefresh {
		// Persist resolved operation + latest authoritative fences together.
		// Refresh subsequently persists its NEW ID/payload before mutation.
		c.mu.Lock()
		previous := c.saved
		c.saved.Catalog = metadata.Snapshot()
		c.captureCompletionLocked(cat)
		c.saved.Pending = nil
		e = c.saveLocked(ctx)
		if e != nil {
			c.saved = previous
		}
		c.mu.Unlock()
		if e != nil {
			return e
		}
		return c.synchronizeClientTargetRound(ctx, client, needsSelectedRefresh, rounds-1, false, targetID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saved.Catalog != nil {
		cmp, e := wlbs.CompareDecimal(cat.Revision, c.saved.Catalog.Revision)
		if e != nil || cmp < 0 {
			return errors.New("STALE_CATALOG")
		}
	}
	preview := &wlbs.CatalogStore{}
	if previous := c.store.Snapshot(); previous != nil {
		if e = restoreManagedCatalog(preview, previous, c.link.SubscriptionRef, time.Now()); e != nil {
			return e
		}
	}
	if e = preview.Apply(cat, c.link.SubscriptionRef, registration, time.Now()); e != nil {
		return e
	}
	previousSaved := c.saved
	c.saved.Catalog = preview.Snapshot()
	c.captureCompletionLocked(cat)
	c.saved.Pending = nil
	if e = c.saveLocked(ctx); e != nil {
		c.saved = previousSaved
		return e
	}
	return c.store.Apply(cat, c.link.SubscriptionRef, registration, time.Now())
}

func (c *managedController) captureCompletionLocked(cat *wlbs.Catalog) {
	if p := c.saved.Pending; p != nil && (c.saved.Completed == nil || c.saved.Completed.RequestID != p.RequestID) {
		c.saved.Completed = &managedCompletion{RequestID: p.RequestID, Catalog: cat}
	}
}

func (c *managedController) recordCompletion(ctx context.Context, id string, cat *wlbs.Catalog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.saved.Completed
	if previous != nil && previous.RequestID == id {
		if !reflect.DeepEqual(previous.Catalog, cat) {
			return errors.New("REVISION_CONFLICT")
		}
		return nil
	}
	c.saved.Completed = &managedCompletion{RequestID: id, Catalog: cat}
	if err := c.saveLocked(ctx); err != nil {
		c.saved.Completed = previous
		return err
	}
	return nil
}

func sameManagedGrant(old, next *wlbs.Catalog) error {
	if old == nil || next == nil || next.RegistrationID != old.RegistrationID || next.SubscriptionRef != old.SubscriptionRef {
		return errors.New("BAD_CATALOG")
	}
	for _, a := range old.Nodes {
		for _, b := range next.Nodes {
			if a.Access.GrantID == b.Access.GrantID && a.NodeID != b.NodeID {
				return errors.New("GRANT_REVOKED")
			}
			if a.NodeID == b.NodeID && (a.Access.GrantID != b.Access.GrantID || a.Access.Generation != b.Access.Generation) {
				return errors.New("GRANT_REVOKED")
			}
		}
	}
	return nil
}

// Exact ID lookup: no fallback to the first remaining node after deletion.
func managedNodeByID(cat *wlbs.Catalog, id string) (wlbs.Node, error) {
	if cat != nil && id != "" {
		for _, node := range cat.Nodes {
			if node.NodeID == id {
				return node, nil
			}
		}
	}
	return wlbs.Node{}, errors.New("SELECTED_NODE_REMOVED")
}

func managedSelectionID(_ *wlbs.Catalog, saved string) string {
	// Manual selection is authoritative for every catalog size. Empty remains
	// empty until an explicit choose_node is durably acknowledged.
	return saved
}

// sameManagedNodeConfig compares every material identity/config field of one signed
// node. The volatile lease freshness literal Access.ExpiresAt is deliberately excluded:
// a same-revision catalog refresh may re-issue the identical node with a new lease
// window, and shortening or extension stays enforced by DecideAdmission and the proven
// deadline, never by this fence. Endpoint, name/country, ports, protocol/auth mode,
// workers, grant, device, password, hashes, unlimited flag and the generation/lease_seq
// fence all remain fail-closed.
func sameManagedNodeConfig(a, b wlbs.Node) bool {
	return a.Endpoint == b.Endpoint && a.Name == b.Name && a.CountryCode == b.CountryCode &&
		a.WGPort == b.WGPort && a.Protocol == b.Protocol && a.AuthMode == b.AuthMode &&
		a.MaxWorkers == b.MaxWorkers && a.Access.GrantID == b.Access.GrantID &&
		a.Access.DeviceID == b.Access.DeviceID && a.Access.Password == b.Access.Password &&
		slices.Equal(a.Access.VKHashes, b.Access.VKHashes) && a.Access.Unlimited == b.Access.Unlimited &&
		a.Access.Generation == b.Access.Generation && a.Access.LeaseSeq == b.Access.LeaseSeq
}

func sameManagedAdmission(cat *wlbs.Catalog, revision string, node wlbs.Node) bool {
	latest, err := managedNodeByID(cat, node.NodeID)
	return err == nil && cat != nil && cat.Revision == revision && sameManagedNodeConfig(latest, node)
}

// mobileAdmissionFence additionally requires the live mobile proof to still belong to
// this exact node, so a late target that lost the selection cannot commit.
func (c *managedController) mobileAdmissionFence(cat *wlbs.Catalog, revision string, node wlbs.Node) bool {
	if !sameManagedAdmission(cat, revision, node) {
		return false
	}
	if c.mobileMode() && c.mobile != nil && c.mobile.proofNode() != node.NodeID {
		return false
	}
	return true
}

func (c *managedController) chooseNode(ctx context.Context, id string) error {
	if c.mobileMode() {
		if c.mobile == nil {
			return errors.New("MOBILE_STATE_UNAVAILABLE")
		}
		// No legacy fallback: the selected gateway must carry an admitted decision
		// before the selection becomes durable or any start is possible.
		if err := c.mobile.bindSelection(id); err != nil {
			return err
		}
	}
	if _, err := managedNodeByID(c.store.Snapshot(), id); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saved.SelectedNodeID == id {
		return nil
	}
	previous := c.saved.SelectedNodeID
	c.saved.SelectedNodeID = id
	if err := c.saveLocked(ctx); err != nil {
		c.saved.SelectedNodeID = previous
		return err
	}
	return nil
}

func (c *managedController) waitForNode(ctx context.Context) (string, error) {
	var probeCancel context.CancelFunc
	probeDone := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			c.nextProbeEpoch()
			if probeCancel != nil {
				probeCancel()
			}
			return "", ctx.Err()
		case <-probeDone:
			probeCancel = nil
		case <-c.bridge.probeStop:
			c.nextProbeEpoch()
			if probeCancel != nil {
				probeCancel()
			}
		case request := <-c.bridge.probe:
			if probeCancel != nil {
				_ = c.bridge.send(echoProbeID(bridgeMessage{"type": "node_probe_result", "node_id": request.NodeID, "status": "busy"}, request))
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			probeCancel = cancel
			epoch := c.nextProbeEpoch()
			go func(request managedProbeRequest, epoch uint64) {
				defer func() {
					cancel()
					select {
					case probeDone <- struct{}{}:
					default:
					}
				}()
				measurement, err := c.runNodeProbe(probeCtx, request.NodeID)
				if !c.probeResultIsFresh(epoch) {
					return
				}
				_ = c.bridge.send(echoProbeID(managedProbeResult(request.NodeID, measurement, err), request))
			}(request, epoch)
		case id := <-c.bridge.preference:
			if err := c.chooseNode(ctx, id); err != nil {
				return "", err
			}
			c.publishCatalog()
		case id := <-c.bridge.selection:
			c.nextProbeEpoch()
			// The selected node is the gateway the explicit Connect binds to; the host
			// may also send the optional gateway_key explicitly (SessionService), which
			// takes precedence over the selected id.
			gatewayKey := c.bridge.selectionGatewayKey()
			if gatewayKey == "" {
				gatewayKey = id
			}
			if probeCancel != nil {
				probeCancel()
				select {
				case <-probeDone:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				probeCancel = nil
			}
			// The explicit Connect funnel (select / one-tap retained, after VPN
			// consent) is the only selection allowed to run the pre-admission
			// onboarding-hour intent/start. A failure is diagnostic-only: paid,
			// revoked and already-started sessions continue through the accepted
			// admission path unchanged, and no background/resume path ever arms it.
			if c.bridge.explicitSelection() && c.mobile != nil {
				// Bound the explicit hour attempt by the existing mobile call budget;
				// it never extends the attempt lifetime or the Connect deadline policy.
				onboardingCtx, stopOnboarding := context.WithTimeout(ctx, mobileHTTPTimeout)
				onboardingErr := c.runExplicitOnboarding(ctx, onboardingCtx, gatewayKey, c.mobile)
				stopOnboarding()
				// Only a post-start wait failure or a canceled parent is terminal here;
				// an optional onboarding-hour API error never blocks the accepted
				// paid/trial/retained admission path.
				if onboardingErr != nil {
					return "", onboardingErr
				}
			}
			if err := c.chooseNode(ctx, id); err != nil {
				return "", err
			}
			c.publishCatalog()
			return id, nil
		}
	}
}

// managedProbeMeasurement separates the transport setup duration from the
// measured keepalive echo RTT. Setup is diagnostic only; only RTT is a ping.
type managedProbeMeasurement struct {
	Setup time.Duration
	RTT   time.Duration
}

// managedProbeConnectFunc acquires TURN -> WRAP -> DTLS for one manual probe.
type managedProbeConnectFunc func(context.Context, wlbs.Node) (net.Conn, func(), error)

// managedProbeConnect is the credential fetch + transport setup seam. Focused
// tests replace it with a synthetic connection; the production default always
// dials the real managed transport. Credential fetch and setup are explicitly
// outside the echo RTT window measured by managedProbeEcho.
var managedProbeConnect = dialProbeTransport

// managedProbeEcho is the echo measurement seam used by runNodeProbe.
var managedProbeEcho = measureEchoRTT

func dialProbeTransport(ctx context.Context, node wlbs.Node) (net.Conn, func(), error) {
	key, err := deriveWrapKey(node.Access.Password)
	if err != nil {
		return nil, nil, errors.New("BAD_MESSAGE")
	}
	var user, pass string
	var urls []string
	for index, hash := range node.Access.VKHashes {
		user, pass, urls, err = GetCreds(ctx, hash, 0)
		if err == nil || index == len(node.Access.VKHashes)-1 || !isHashFallbackCredentialError(err) {
			break
		}
	}
	if err != nil {
		return nil, nil, errors.New("VK_API_UNAVAILABLE")
	}
	peer := &net.UDPAddr{IP: net.ParseIP(node.PeerIP), Port: node.DTLSPort}
	conn, cleanup, err := dialManagedTransport(ctx, &TurnParams{WrapKey: key, Hashes: node.Access.VKHashes}, peer,
		&Credentials{User: user, Pass: pass, TurnURLs: urls}, node.DTLSSPKISHA256, 0, false, 0, false, nil)
	if err != nil {
		return nil, cleanup, err
	}
	if conn == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, errors.New("TRANSPORT_FAILED")
	}
	return conn, cleanup, nil
}

// runNodeProbe measures one exact cached node: TURN/WRAP/pinned-DTLS setup is
// timed up to the established connection, then the echo RTT is measured on that
// same live connection. It performs no VPN auth, config, selection, persistence
// or HTTP exit probe. On an echo failure the error is returned with the setup
// duration for diagnostics and no RTT.
func (c *managedController) runNodeProbe(ctx context.Context, id string) (managedProbeMeasurement, error) {
	started := time.Now()
	cat := c.store.Snapshot()
	node, err := managedProbeNode(cat, c.saved.Pending != nil, c.link.SubscriptionRef, id, time.Now())
	if err != nil {
		return managedProbeMeasurement{}, err
	}
	conn, cleanup, err := managedProbeConnect(ctx, node)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return managedProbeMeasurement{}, err
	}
	if conn == nil {
		return managedProbeMeasurement{}, errors.New("TRANSPORT_FAILED")
	}
	setup := time.Since(started)
	rtt, err := managedProbeEcho(ctx, conn)
	if err != nil {
		return managedProbeMeasurement{Setup: setup}, err
	}
	return managedProbeMeasurement{Setup: setup, RTT: rtt}, nil
}

// managedProbeResult builds the host-visible probe result. status stays
// ok/failed/timeout/cancelled. transport_setup_ms is diagnostic and must never
// be displayed as ping/RTT; rtt_ms is the echo measurement and the only value
// the host may render as node RTT. Failure paths carry no rtt_ms.
func managedProbeResult(nodeID string, measurement managedProbeMeasurement, probeErr error) bridgeMessage {
	status := managedProbeStatus(probeErr)
	message := bridgeMessage{"type": "node_probe_result", "node_id": nodeID, "status": status}
	if status == "ok" {
		message["transport_setup_ms"] = measurement.Setup.Milliseconds()
		message["rtt_ms"] = measurement.RTT.Milliseconds()
	}
	return message
}

// managedProbeStatus maps an echo error to the host-visible status vocabulary.
func managedProbeStatus(probeErr error) string {
	if probeErr == nil {
		return "ok"
	}
	if errors.Is(probeErr, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(probeErr, context.Canceled) {
		return "cancelled"
	}
	return "failed"
}

// connectedProbeTimeout bounds one manual probe of the already-Connected node.
const connectedProbeTimeout = 12 * time.Second

// managedConnectedEcho is the connected-session echo seam used by the live
// Connected event loop. Focused tests replace it; the production default asks
// the registered config session for a sequenced 0xFF echo RTT and never opens
// a second connection.
var managedConnectedEcho = requestConnectedEchoRTT

// managedNonCurrentProbe is the direct echo seam used by the Connected event
// loop for a manual probe of a node that is NOT the current one. Focused tests
// replace it; the production default measures that node with the existing
// pre-connect direct path (catalog validation, then TURN/WRAP/pinned-DTLS setup
// on a separate socket and a keepalive echo RTT) without switching, without
// mutating the selection/session and without touching the live data plane.
var managedNonCurrentProbe = func(c *managedController, ctx context.Context, id string) (managedProbeMeasurement, error) {
	return c.runNodeProbe(ctx, id)
}

func requestConnectedEchoRTT(ctx context.Context, d *Dispatcher) (time.Duration, error) {
	return d.RequestConnectedRTT(ctx)
}

// connectedProbeResult builds the host-visible result for a probe measured on
// the already-Connected session. There is no transport setup in this window,
// so the frame never carries transport_setup_ms; rtt_ms is present only on a
// successful same-session echo.
func connectedProbeResult(nodeID string, rtt time.Duration, probeErr error) bridgeMessage {
	status := managedProbeStatus(probeErr)
	if errors.Is(probeErr, errEchoProbeBusy) {
		status = "busy"
	}
	message := bridgeMessage{"type": "node_probe_result", "node_id": nodeID, "status": status}
	if status == "ok" {
		message["rtt_ms"] = rtt.Milliseconds()
	}
	return message
}

func managedProbeNode(cat *wlbs.Catalog, pending bool, subscription, id string, now time.Time) (wlbs.Node, error) {
	if pending {
		return wlbs.Node{}, errors.New("OPERATION_PENDING")
	}
	if cat == nil {
		return wlbs.Node{}, errors.New("CATALOG_EXPIRED")
	}
	if err := cat.ValidateNode(subscription, cat.RegistrationID, id, now); err != nil {
		return wlbs.Node{}, errors.New("CATALOG_EXPIRED")
	}
	return managedNodeByID(cat, id)
}

func (c *managedController) probeForNode(id string) (managedProbe, error) {
	p, ok := c.start.ProbeByNode[id]
	u, err := url.Parse(p.ProbeURL)
	if !ok || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || net.ParseIP(p.ExpectedExitIP) == nil {
		return managedProbe{}, errors.New("PROBE_NOT_PROVISIONED")
	}
	return p, nil
}

// Validate expired snapshots at their original issue time, preserving full
// revision/generation fences without using them to authorize a new tunnel.
func restoreManagedCatalog(store *wlbs.CatalogStore, cat *wlbs.Catalog, subscription string, now time.Time) error {
	if cat == nil {
		return errors.New("BAD_CATALOG")
	}
	issued, e := wlbs.UTC(cat.IssuedAt)
	if e != nil || issued.After(now.Add(time.Minute)) {
		return errors.New("BAD_CATALOG")
	}
	// A reconciled metadata snapshot may have been issued after its old
	// lease expired. It is restorable for refresh, never for VPN admission.
	if cat.ValidateRefreshMetadata(subscription, cat.RegistrationID, now) == nil {
		return store.ApplyRefreshMetadata(cat, subscription, cat.RegistrationID, now)
	}
	return store.ApplyRefreshMetadata(cat, subscription, cat.RegistrationID, issued)
}

func (c *managedController) clearPending(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.saved.Pending
	c.saved.Pending = nil
	if e := c.saveLocked(ctx); e != nil {
		c.saved.Pending = previous
		return e
	}
	return nil
}

// Agreed SCHEMA_DECISIONS_01: immediate register/refresh and completed status
// wrap a full catalog; catalog read itself may return the full snapshot.
func managedCatalogResponse(raw []byte) (*wlbs.Catalog, error) {
	var envelope struct {
		V               int           `json:"v"`
		Status          string        `json:"status"`
		Catalog         *wlbs.Catalog `json:"catalog"`
		SubscriptionRef string        `json:"subscription_ref"`
	}
	if decodeErr := wlbs.StrictJSON(raw, &envelope); decodeErr != nil {
		// A country_code wire-shape failure is already the catalog-specific,
		// protocol-safe error. Other envelope decode failures stay BAD_MESSAGE.
		if wlbs.IsCountryCodeWireError(decodeErr) {
			return nil, errors.New("BAD_CATALOG")
		}
		return nil, errors.New("BAD_MESSAGE")
	}
	switch envelope.Status {
	case "ok", "complete":
		if envelope.Catalog != nil {
			if envelope.V != 1 {
				return nil, errors.New("BAD_MESSAGE")
			}
			return envelope.Catalog, nil
		}
		if envelope.Status == "ok" && envelope.SubscriptionRef != "" && (envelope.V == 1 || envelope.V == 2) {
			var cat wlbs.Catalog
			if wlbs.StrictJSON(raw, &cat) == nil && cat.V == envelope.V {
				return &cat, nil
			}
		}
		return nil, errors.New("BAD_CATALOG")
	case "pending", "unknown", "failed":
		if envelope.V != 1 {
			return nil, errors.New("BAD_MESSAGE")
		}
		var operation wlbs.OperationStatus
		if wlbs.StrictJSON(raw, &operation) != nil || operation.Validate() != nil {
			return nil, errors.New("BAD_MESSAGE")
		}
		if operation.Status == "failed" {
			return nil, errors.New(safeManagedCode(operation.Code))
		}
		if operation.Status == "unknown" {
			return nil, errors.New("OPERATION_UNKNOWN")
		}
		return nil, errors.New("OPERATION_PENDING")
	default:
		return nil, errors.New("BAD_MESSAGE")
	}
}
func (c *managedController) publishCatalog() { _ = c.publishCatalogContext(context.Background()) }

func (c *managedController) publishCatalogContext(ctx context.Context) error {
	cat := c.store.Snapshot()
	if cat == nil {
		return nil
	}
	nodes := make([]bridgeMessage, 0, len(cat.Nodes))
	for _, n := range cat.Nodes {
		nodes = append(nodes, bridgeMessage{"node_id": n.NodeID, "name": n.Name, "country_code": n.CountryCode})
	}
	// Public display projection only. Never expose access secrets/IDs or use this
	// host summary instead of the native catalog/lease validation gates.
	c.mu.Lock()
	selected := c.saved.SelectedNodeID
	c.mu.Unlock()
	var subscriptionExpiry any = cat.SubscriptionExpiresAt
	if cat.SubscriptionUnlimited {
		subscriptionExpiry = nil
	}
	message := bridgeMessage{"type": "catalog", "nodes": nodes, "selected_node_id": selected, "slots_used": cat.SlotsUsed, "slots_limit": cat.SlotsLimit,
		"subscription_status": cat.SubscriptionStatus, "subscription_expires_at": subscriptionExpiry,
		"catalog_expires_at": cat.CatalogExpiresAt, "issued_at": cat.IssuedAt, "revision": cat.Revision}
	if cat.SubscriptionUnlimited {
		message["catalog_version"] = cat.V
		message["subscription_unlimited"] = true
	}
	if err := c.bridge.sendContext(ctx, message); err != nil {
		return err
	}
	return c.stateContext(ctx, "CatalogReady")
}

func managedWGConfig(raw, port string) (string, error) {
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return "", errors.New("BAD_WG_CONFIG")
	}
	if len(raw) > 16*1024 || !strings.Contains(raw, "[Interface]") || !strings.Contains(raw, "[Peer]") {
		return "", errors.New("BAD_WG_CONFIG")
	}
	lines := strings.Split(raw, "\n")
	found := false
	for i, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Endpoint") {
			if found {
				return "", errors.New("BAD_WG_CONFIG")
			}
			found = true
			lines[i] = "Endpoint = 127.0.0.1:" + port
		}
	}
	if !found {
		return "", errors.New("BAD_WG_CONFIG")
	}
	return strings.Join(lines, "\n"), nil
}

func managedWorkerIdentity(node wlbs.Node, registration, session, mode string, id int) wlbs.VPNIdentity {
	return wlbs.VPNIdentity{NodeID: node.NodeID, GrantID: node.Access.GrantID, RegistrationID: registration, Generation: node.Access.Generation, LeaseSeq: node.Access.LeaseSeq, TransportSession: session, WorkerID: strconv.Itoa(id - 1), Mode: mode}
}

func managedWorkerTransportPolicy() (hashFallback, turnStreamFirst bool) {
	// Managed catalogs already carry up to four signed VK hashes. Use those
	// reserves while retaining the compatible UDP-first TURN ordering.
	return true, false
}

var errManagedRuntimeHandedOff = errors.New("MANAGED_RUNTIME_HANDOFF")

func validManagedSwitch(value managedSwitch) bool {
	return len(value.ID) == 36 && managedLengthInRange(len(value.NodeID), 1, 128) && managedLengthInRange(len(value.Revision), 1, 128)
}

func managedLengthInRange(value, low, high int) bool { return value >= low && value <= high }

func (c *managedController) prepareSwitchTarget(ctx context.Context, operation managedSwitch, activeID, registration string,
	refreshTarget func(context.Context, string) error) (wlbs.Node, error) {
	if err := ctx.Err(); err != nil {
		return wlbs.Node{}, err
	}
	catalog := c.store.Snapshot()
	target, err := managedNodeByID(catalog, operation.NodeID)
	if !validManagedSwitch(operation) || catalog == nil || catalog.Revision != operation.Revision ||
		(err == nil && target.NodeID == activeID) {
		return wlbs.Node{}, errors.New("REVISION_CONFLICT")
	}
	if err != nil {
		return wlbs.Node{}, err
	}
	if c.mobileMode() {
		if c.mobile == nil {
			return wlbs.Node{}, errors.New("MOBILE_STATE_UNAVAILABLE")
		}
		return c.mobile.prepareSwitchTarget(ctx, target, refreshTarget)
	}
	if err = catalog.Validate(c.link.SubscriptionRef, registration, time.Now()); err != nil {
		return wlbs.Node{}, err
	}
	if !target.Access.ValidAt(time.Now()) {
		if refreshTarget == nil {
			return wlbs.Node{}, errors.New("BAD_CATALOG")
		}
		if err = refreshTarget(ctx, target.NodeID); err != nil {
			return wlbs.Node{}, err
		}
		if err = ctx.Err(); err != nil {
			return wlbs.Node{}, err
		}
		catalog = c.store.Snapshot()
		target, err = managedNodeByID(catalog, operation.NodeID)
		if err != nil {
			return wlbs.Node{}, err
		}
	}
	if err = catalog.ValidateNode(c.link.SubscriptionRef, registration, operation.NodeID, time.Now()); err != nil {
		return wlbs.Node{}, err
	}
	return target, nil
}

func (c *managedController) rollbackAllowed(node wlbs.Node, registration string) bool {
	if c.mobileMode() && c.mobile != nil {
		// Re-bind the committed selection before evaluating the rollback fence so a
		// failed switch still sees the running node as eligible.
		c.mobile.restoreSelection()
	}
	cat := c.store.Snapshot()
	current, err := managedNodeByID(cat, node.NodeID)
	return err == nil && reflect.DeepEqual(current, node) &&
		cat.ValidateNode(c.link.SubscriptionRef, registration, node.NodeID, time.Now()) == nil
}

func finishManagedOwner(parent context.Context, owned <-chan struct{}, done chan<- error,
	terminal chan<- error, err error) {
	select {
	case <-owned:
		if !errors.Is(err, errManagedRuntimeHandedOff) {
			select {
			case terminal <- err:
			case <-parent.Done():
			}
		}
	default:
		done <- err
	}
}

func cancelManagedPreparation(preparation context.Context, cancel context.CancelFunc,
	diagnostics *managedDiagnostics, connecting bool) func() bool {
	return context.AfterFunc(preparation, func() {
		if connecting && errors.Is(preparation.Err(), context.DeadlineExceeded) {
			diagnostics.noteCancelSource(vpnCancelConnectBudget)
			emitRuntimeCancel(vpnCancelConnectBudget)
			diagnostics.freezeVPNAtSetupTimeout()
		} else {
			diagnostics.noteCancelSource(vpnCancelParentShutdown)
			emitRuntimeCancel(vpnCancelParentShutdown)
		}
		cancel()
	})
}

// managedVPNApply is the single production entry into the managed vpn runtime. Focused
// tests replace it to record the exact admitted node fields and to block until the data
// plane is cancelled; the production default always calls the real vpn method.
var managedVPNApply func(c *managedController, parent context.Context, parentCancel context.CancelFunc, node wlbs.Node,
	registration string, replace func(), operation *managedSwitch, terminal chan<- error, preparation context.Context) error

func init() {
	managedVPNApply = func(c *managedController, parent context.Context, parentCancel context.CancelFunc, node wlbs.Node,
		registration string, replace func(), operation *managedSwitch, terminal chan<- error, preparation context.Context) error {
		return c.vpn(parent, parentCancel, node, registration, replace, operation, terminal, preparation)
	}
}

// managedTunnelPlan carries the prepared local relay, credentials and diagnostics one
// vpn runtime needs to start its DTLS/worker child.
type managedTunnelPlan struct {
	Node          wlbs.Node
	Workers       int
	Dispatcher    *Dispatcher
	Port          string
	TurnParams    *TurnParams
	Peer          *net.UDPAddr
	Registration  string
	Session       string
	Stats         *Stats
	Parent        context.Context
	Cancel        context.CancelFunc
	Diagnostics   *managedDiagnostics
	Connecting    bool
	Preparation   context.Context
	Authenticated func(*wlbs.VPNOK)
}

// managedTunnelRuntime is the live child data plane owned by one vpn invocation.
type managedTunnelRuntime struct {
	Config  string
	Configs <-chan string
	Stop    func()
	Restart func()
	Running func() bool
}

// managedTunnelStart is the tunnel start/stop seam. The production default runs the
// real worker/DTLS pipeline; focused tests substitute a synthetic tunnel so the real
// switch orchestration is exercised without a transport.
var managedTunnelStart = startManagedTunnel

func channelsStatusMessage(runtimeEpoch, lifecycleRevision, generation uint64, active, target int) bridgeMessage {
	return bridgeMessage{"type": "channels_status", "runtime_epoch": runtimeEpoch, "lifecycle_revision": lifecycleRevision,
		"generation": generation, "active": active, "target": target}
}

func startManagedTunnel(ctx context.Context, plan managedTunnelPlan) (managedTunnelRuntime, error) {
	configCh := make(chan string, 1)
	var stopWorkers context.CancelFunc
	var workersDone <-chan struct{}
	workersRunning := false
	startWorkers := func() {
		workerCtx, stop := context.WithCancel(ctx)
		workerCtx = withWorkerCancelDiagnostics(workerCtx, plan.Diagnostics)
		stopWorkers = stop
		gate := newConfigFirstStartGate(true)
		pacer := newStartPacer(workerStartInterval(len(plan.TurnParams.Hashes), false))
		credsGate := newCredentialRequestGate(credentialRequestCooldown)
		primary := make(chan struct{})
		var pause int32
		var workers sync.WaitGroup
		for g := 0; g < (plan.Workers+8)/9; g++ {
			ids := []int{}
			for id := g*9 + 1; id <= plan.Workers && id < g*9+10; id++ {
				ids = append(ids, id)
			}
			var wait <-chan struct{}
			var ready chan<- struct{}
			var out chan<- string
			if g == 0 {
				ready = primary
				out = configCh
			} else {
				wait = primary
			}
			workers.Add(1)
			go func(group int, ids []int, wait <-chan struct{}, ready chan<- struct{}, out chan<- string) {
				defer workers.Done()
				hashFallback, turnStreamFirst := managedWorkerTransportPolicy()
				WorkerGroup(workerCtx, plan.Cancel, group+1, group, plan.TurnParams, plan.Peer, plan.Dispatcher, plan.Port, group == 0, out, ids, plan.Workers, hashFallback, &pause, plan.Registration, plan.Node.Access.Password, "", plan.Session, plan.Stats, turnStreamFirst, gate, pacer, credsGate, wait, ready)
			}(g, ids, wait, ready, out)
		}

		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		workersDone = done
		workersRunning = true
	}
	startWorkers()
	runtime := managedTunnelRuntime{
		Configs: configCh,
		Stop:    func() { stopWorkers(); <-workersDone; workersRunning = false },
		Restart: startWorkers,
		Running: func() bool { return workersRunning },
	}
	setup := time.NewTimer(15 * time.Second)
	defer setup.Stop()
	raw, setupErr := awaitManagedSetup(ctx, plan, configCh, setup.C)
	if setupErr != nil {
		runtime.Stop()
		return managedTunnelRuntime{}, setupErr
	}
	runtime.Config = raw
	return runtime, nil
}

// awaitManagedSetup is the production config-wait decision: it returns the config, the
// 15 s setup timeout, the parent error, or VPN_SETUP_FAILED (emitting exactly one
// first-cause CANCEL_SOURCE line). It is extracted so tests drive the real terminal path.
func awaitManagedSetup(ctx context.Context, plan managedTunnelPlan, configCh <-chan string, setupC <-chan time.Time) (string, error) {
	select {
	case raw := <-configCh:
		plan.Diagnostics.noteVPN(vpnStageConfig, nil)
		return raw, nil
	case <-ctx.Done():
		if plan.Connecting && plan.Preparation != nil && errors.Is(plan.Preparation.Err(), context.DeadlineExceeded) {
			plan.Diagnostics.freezeVPNAtSetupTimeout()
		}
		plan.Diagnostics.noteVPNCancelIfNoFailure(vpnStageConfig, ctx.Err())
		if plan.Parent.Err() != nil {
			return "", plan.Parent.Err()
		}
		// Synchronous fallback: derive the preparation/parent cause from the observed
		// preparation context instead of relying on the asynchronous AfterFunc, which can
		// lose the race against this select and yield UNKNOWN. Only fills a still-unset
		// cause: worker/host causes are recorded first-wins before their cancel.
		if plan.Diagnostics.cancelSourceOrUnknown() == vpnCancelUnknown && plan.Preparation != nil {
			if errors.Is(plan.Preparation.Err(), context.DeadlineExceeded) {
				plan.Diagnostics.noteCancelSource(vpnCancelConnectBudget)
			} else if plan.Preparation.Err() != nil {
				plan.Diagnostics.noteCancelSource(vpnCancelParentShutdown)
			}
		}
		emitVPNSource(plan.Diagnostics.cancelSourceOrUnknown())
		return "", errors.New("VPN_SETUP_FAILED")
	case <-setupC:
		plan.Diagnostics.freezeVPNAtSetupTimeout()
		return "", errors.New("VPN_SETUP_TIMEOUT")
	}
}

func (c *managedController) vpn(parent context.Context, parentCancel context.CancelFunc, node wlbs.Node, registration string,
	replace func(), operation *managedSwitch, terminal chan<- error, preparation context.Context) (resultErr error) {
	runtimeEpoch := c.bridge.nextRuntimeEpoch()
	connecting, connected := operation == nil, false
	defer func() {
		if connecting && !connected && preparation != nil && errors.Is(preparation.Err(), context.DeadlineExceeded) {
			resultErr = errors.New("VPN_SETUP_TIMEOUT")
		}
	}()
	admission := c.store.Snapshot()
	if admission == nil {
		return errors.New("CATALOG_EXPIRED")
	}
	if err := admission.ValidateNode(c.link.SubscriptionRef, registration, node.NodeID, time.Now()); err != nil {
		return err
	}
	probe, probeErr := c.probeForNode(node.NodeID)
	if probeErr != nil {
		return probeErr
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	diagnostics := &managedDiagnostics{}
	c.vpnDiagnostics = diagnostics
	if c.mobileMode() {
		// A proven stop decision or the proven deadline cancels this exact runtime
		// even while no refresh/HTTP is completing.
		release := c.registerMobileStop(cancel, diagnostics)
		defer release()
	}
	commitCtx := ctx
	stopBudget := func() bool { return true }
	if preparation != nil {
		commitCtx = preparation
		stopBudget = cancelManagedPreparation(commitCtx, cancel, diagnostics, connecting)
		defer stopBudget()
	}
	var refreshWorkers sync.WaitGroup
	defer func() { cancel(); refreshWorkers.Wait() }()
	_ = c.stateContext(commitCtx, "NodeAuthenticating")
	n := node.MaxWorkers
	if n < 1 || n > 128 {
		return errors.New("WORKER_POLICY_INVALID")
	}
	session := newTransportSession()
	// A dead host pipe must not make cancellation wait for the observer.
	go c.bridge.emitDiagnostics(ctx, diagnostics)
	managed := &ManagedTransportConfig{NodePin: node.DTLSSPKISHA256, RegistrationDeviceID: registration, TransportSession: session, Diagnostics: diagnostics}
	var authMu sync.Mutex
	var firstOK *wlbs.VPNOK
	managed.Authenticate = func(authctx context.Context, conn *dtls.Conn, mode string, id int) error {
		select {
		case c.signSem <- struct{}{}:
		case <-authctx.Done():
			return authctx.Err()
		}
		defer func() { <-c.signSem }()
		state, valid := conn.ConnectionState()
		if !valid {
			return errors.New("TRUST_FAILED")
		}
		identity := managedWorkerIdentity(node, registration, session, mode, id)
		if e := identity.ValidateWorker(n); e != nil {
			return e
		}
		authctx = context.WithValue(authctx, managedWorkerContextKey{}, identity.WorkerID)
		ok, e := wlbs.AuthenticateVPN(authctx, conn, state.ExportKeyingMaterial, c.sign, identity)
		if e == nil && mode == "getconf" {
			authMu.Lock()
			firstOK = ok
			authMu.Unlock()
		}
		return e
	}
	ctx = WithManagedTransport(ctx, managed)
	key, e := deriveWrapKey(node.Access.Password)
	if e != nil {
		return errors.New("BAD_MESSAGE")
	}
	// Explicit binding also covers the local relay's return socket. Loopback
	// remains loopback; the physical binding must exist before any packet I/O.
	listener := net.ListenConfig{Control: managedSocketControl}
	local, e := listener.ListenPacket(ctx, "udp", "127.0.0.1:0")
	if e != nil {
		return errors.New("LOCAL_RELAY_FAILED")
	}
	stop := context.AfterFunc(ctx, func() { local.Close() })
	defer stop()
	_, port, _ := net.SplitHostPort(local.LocalAddr().String())
	stats := NewStats()
	disp := NewDispatcher(ctx, local, stats)
	defer disp.Shutdown()
	// The dispatcher read loop owns the socket: closing it first keeps teardown of a
	// failed switch target bounded by this runtime, not by its preparation deadline.
	defer local.Close()
	tp := &TurnParams{Hashes: node.Access.VKHashes, WrapKey: key}
	peer := &net.UDPAddr{IP: net.ParseIP(node.PeerIP), Port: node.DTLSPort}
	tunnel, e := managedTunnelStart(ctx, managedTunnelPlan{
		Node: node, Workers: n, Dispatcher: disp, Port: port, TurnParams: tp, Peer: peer,
		Registration: registration, Session: session, Stats: stats, Parent: parent, Cancel: cancel,
		Diagnostics: diagnostics, Connecting: connecting, Preparation: preparation,
		Authenticated: func(ok *wlbs.VPNOK) {
			authMu.Lock()
			firstOK = ok
			authMu.Unlock()
		},
	})
	if e != nil {
		return e
	}
	defer tunnel.Stop()
	var wake managedWakeTracker
	defer func() {
		if connecting && !connected && preparation != nil && errors.Is(preparation.Err(), context.DeadlineExceeded) {
			diagnostics.freezeVPNAtSetupTimeout()
		}
	}()
	config, e := managedWGConfig(tunnel.Config, port)
	if e != nil {
		return e
	}
	authMu.Lock()
	ok := firstOK
	authMu.Unlock()
	if ok == nil {
		return errors.New("AUTH_REQUIRED")
	}
	// The target was authenticated against one exact signed catalog snapshot.
	// A later generation, grant or revision must restart admission rather than
	// completing a stale switch.
	if !c.mobileAdmissionFence(c.store.Snapshot(), admission.Revision, node) {
		return errors.New("REVISION_CONFLICT")
	}
	applyCtx, stopApply := context.WithTimeout(commitCtx, 45*time.Second)
	diagnostics.noteVPN(vpnStageBridge, nil)
	request := bridgeMessage{"type": "vpn_config", "runtime_epoch": runtimeEpoch, "node_id": node.NodeID, "config": config, "access_expires_at": ok.AccessExpiresAt, "server_time": ok.ServerTime, "probe_url": probe.ProbeURL, "expected_exit_ip": probe.ExpectedExitIP}
	if ok.AccessUnlimited {
		request["access_expires_at"] = nil
		request["access_unlimited"] = true
	}
	if operation != nil {
		request["switch_id"] = operation.ID
		request["catalog_revision"] = operation.Revision
	}
	result, e := c.bridge.request(applyCtx, request, "request_id", "vpn_result")
	stopApply()
	if e != nil {
		if connecting && preparation != nil && errors.Is(preparation.Err(), context.DeadlineExceeded) {
			diagnostics.freezeVPNAtSetupTimeout()
		}
		diagnostics.noteVPN(vpnStageBridge, e)
		return e
	}
	if echoedEpoch, valid := result.positiveSafeUint64("runtime_epoch"); !valid || echoedEpoch != runtimeEpoch {
		return errors.New("REVISION_CONFLICT")
	}
	if operation != nil && (result.string("switch_id") != operation.ID ||
		result.string("catalog_revision") != operation.Revision) {
		return errors.New("REVISION_CONFLICT")
	}
	if yes, _ := result["ok"].(bool); !yes {
		return errors.New("VPN_PROBE_FAILED")
	}
	if replace == nil && preparation != nil {
		if err := commitCtx.Err(); err != nil {
			return err
		}
		if !stopBudget() {
			return context.DeadlineExceeded
		}
	}
	connected = true
	if replace != nil {
		if operation == nil {
			return errors.New("REVISION_CONFLICT")
		}
		// Readiness may take long enough for a signed catalog refresh to land.
		// Fence again immediately before persisting the target and cancelling
		// the last-good runtime; never commit a stale grant/config completion.
		if !c.mobileAdmissionFence(c.store.Snapshot(), admission.Revision, node) {
			return errors.New("REVISION_CONFLICT")
		}
		if e := commitCtx.Err(); e != nil {
			return e
		}
		if !stopBudget() {
			return context.DeadlineExceeded
		}
		if e := c.chooseNode(commitCtx, node.NodeID); e != nil {
			return e
		}
		if e := c.publishCatalogContext(commitCtx); e != nil {
			return e
		}
		if e := c.bridge.sendContext(commitCtx, bridgeMessage{"type": "switch_result", "node_id": node.NodeID,
			"switch_id": operation.ID, "catalog_revision": operation.Revision, "status": "ok"}); e != nil {
			return e
		}
		switchDiag(c.bridge, "runner_result_ok")
		replace()
		operation = nil
	}
	// The host already verified Connected, native never guesses based on workers.
	refreshResult := make(chan error, 1)
	refreshing := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	retryAt := time.Time{}
	// S5 07.2: manual echo probe state for this live runtime. At most one probe
	// is in flight; the shared probeEpoch fences cancel_probe/selection/
	// supersession, and the runtime ctx drops any result produced after this
	// runtime ended (Disconnect, switch handoff, restart).
	probeDone := make(chan struct{}, 1)
	probePending := false
	var probeCancel context.CancelFunc
	defer func() {
		if probeCancel != nil {
			probeCancel()
		}
	}()
	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			return errors.New("TUNNEL_STOPPED")
		case <-probeDone:
			probePending = false
			probeCancel = nil
		case <-c.bridge.probeStop:
			// cancel_probe invalidates whatever connected probe is in flight:
			// bump the shared epoch before releasing it so a late same-node
			// result can never reach the host.
			c.nextProbeEpoch()
			if probeCancel != nil {
				probeCancel()
			}
		case request := <-c.bridge.probe:
			// Manual only: a probe frame never starts a connection and never
			// retries by itself. Exactly one probe may be in flight; a second
			// request is answered busy with the same probe_id echoed. The
			// current node is measured on the live session echo; any other
			// catalog node is measured on its own direct echo connection
			// without switching or touching the active session.
			if probePending {
				_ = c.bridge.send(echoProbeID(bridgeMessage{"type": "node_probe_result", "node_id": request.NodeID, "status": "busy"}, request))
				continue
			}
			probePending = true
			probeCtx, cancelProbe := context.WithTimeout(ctx, connectedProbeTimeout)
			probeCancel = cancelProbe
			epoch := c.nextProbeEpoch()
			// The seams are read on the event-loop goroutine (never inside
			// the probe goroutine), so test substitution can never race the
			// call.
			directProbe := managedNonCurrentProbe
			if request.NodeID != node.NodeID {
				go func(request managedProbeRequest, epoch uint64, probe func(*managedController, context.Context, string) (managedProbeMeasurement, error)) {
					defer func() {
						cancelProbe()
						select {
						case probeDone <- struct{}{}:
						default:
						}
					}()
					measurement, err := probe(c, probeCtx, request.NodeID)
					if !c.probeResultIsFresh(epoch) || ctx.Err() != nil {
						return
					}
					_ = c.bridge.send(echoProbeID(managedProbeResult(request.NodeID, measurement, err), request))
				}(request, epoch, directProbe)
				continue
			}
			echoRequest := managedConnectedEcho
			go func(request managedProbeRequest, epoch uint64,
				echo func(context.Context, *Dispatcher) (time.Duration, error),
				probe func(*managedController, context.Context, string) (managedProbeMeasurement, error)) {
				defer func() {
					cancelProbe()
					select {
					case probeDone <- struct{}{}:
					default:
					}
				}()
				rtt, err := echo(probeCtx, disp)
				if errors.Is(err, errEchoChannelDirty) {
					// Fail closed: an earlier ping is still unanswered, so a
					// live pong could be the old answer and must not settle a
					// fresh measurement. Re-measure the same node on a fresh
					// independent connection within the same probe budget.
					measurement, directErr := probe(c, probeCtx, request.NodeID)
					if !c.probeResultIsFresh(epoch) || ctx.Err() != nil {
						return
					}
					_ = c.bridge.send(echoProbeID(managedProbeResult(request.NodeID, measurement, directErr), request))
					return
				}
				if !c.probeResultIsFresh(epoch) || ctx.Err() != nil {
					return
				}
				_ = c.bridge.send(echoProbeID(connectedProbeResult(request.NodeID, rtt, err), request))
			}(request, epoch, echoRequest, directProbe)
		case switchRequest := <-c.bridge.switchNode:
			switchDiag(c.bridge, "runner_consumed")
			targetID := switchRequest.NodeID
			switchCtx, stopSwitch := context.WithTimeout(parent, 10*time.Second)
			var target wlbs.Node
			var switchErr error
			if refreshing {
				// Do not race two durable reconciliation transactions.
				switchErr = errors.New("REVISION_CONFLICT")
			} else {
				target, switchErr = c.prepareSwitchTarget(switchCtx, switchRequest, node.NodeID, registration,
					func(ctx context.Context, id string) error { return c.synchronizeTarget(ctx, true, id) })
			}
			if switchErr == nil {
				_, switchErr = c.probeForNode(targetID)
			}
			if switchErr == nil {
				// The current runtime remains alive while the target authenticates and
				// produces a fully validated configuration. It is cancelled only after
				// the host acknowledges the replacement as ready.
				owned := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					err := managedVPNApply(c, parent, parentCancel, target, registration, func() {
						// Local attempt diagnostics captured here, never c.vpnDiagnostics.
						noteSwitchCancelSource(diagnostics, cancel)
						close(owned)
					}, &switchRequest, terminal, switchCtx)
					finishManagedOwner(parent, owned, done, terminal, err)
				}()
				select {
				case <-owned:
					stopSwitch()
					return errManagedRuntimeHandedOff
				case switchErr = <-done:
				case <-parent.Done():
					stopSwitch()
					return parent.Err()
				}
			}
			if switchErr != nil && parent.Err() == nil {
				// The switch budget (switchCtx, 10s) is typically exhausted by the failed
				// attempt itself. A terminal result must never be dropped by that expired
				// deadline: send it with a live runtime context (same as the diag/lease path)
				// so the host can rollback and clear its pending switch.
				_ = c.bridge.send(switchFailureMessage(targetID, switchRequest.ID, switchRequest.Revision,
					managedCode(switchErr), c.rollbackAllowed(node, registration)))
				switchDiag(c.bridge, "runner_result_failed")
				_ = c.publishCatalogContext(parent)
				stopSwitch()
				continue
			}
			stopSwitch()
			return switchErr
		case e := <-refreshResult:
			if parent.Err() != nil {
				return parent.Err()
			}
			if ctx.Err() != nil {
				return errors.New("TUNNEL_STOPPED")
			}
			refreshing = false
			retryAt = time.Now().Add(15 * time.Second)
			if e == nil {
				cat := c.store.Snapshot()
				fresh, nodeErr := managedNodeByID(cat, node.NodeID)
				if nodeErr != nil {
					return nodeErr
				}
				if fresh.Access.GrantID != node.Access.GrantID || fresh.Access.Generation != node.Access.Generation || fresh.Access.Password != node.Access.Password || fresh.DTLSSPKISHA256 != node.DTLSSPKISHA256 || fresh.PeerIP != node.PeerIP || fresh.DTLSPort != node.DTLSPort {
					return errors.New("TUNNEL_RECONFIGURE_REQUIRED")
				}
				c.publishCatalog()
				leaseMessage := bridgeMessage{"type": "lease", "access_expires_at": fresh.Access.ExpiresAt, "server_time": managedServerTime(cat)}
				if fresh.Access.Unlimited {
					leaseMessage["access_expires_at"] = nil
					leaseMessage["access_unlimited"] = true
				}
				c.bridge.send(leaseMessage)
			} else {
				code := managedCode(e)
				if code == "DEVICE_REVOKED" || code == "GRANT_REVOKED" || code == "SUBSCRIPTION_EXPIRED" {
					noteRevokedCancelSource(diagnostics, parentCancel)
					return e
				}
				c.bridge.send(bridgeMessage{"type": "state", "state": "CatalogReady", "warning": code})
			}
		case updatedConfig := <-tunnel.Configs:
			normalized, err := managedWGConfig(updatedConfig, port)
			if err != nil || normalized != config {
				return errors.New("VPN_CONFIG_CHANGED")
			}
		case <-tick.C:
			lifecycle := c.bridge.lifecycleSnapshot()
			wake.observe(lifecycle.Revision, lifecycle.Sleeping, time.Now(), disp.noteDeviceSleep, disp.noteDeviceWake)
			if !tunnel.Running() && !lifecycle.Sleeping && wake.generation != 0 {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.bridge.admitLifecycleRestart(ctx, lifecycle.Revision, tunnel.Restart)
			}
			if wake.generation != 0 && !lifecycle.Sleeping {
				ready, total := disp.wakeStatus(wake.generation)
				if wake.rescue(time.Now(), wake.generation, ready) {
					// Preserve the local relay, VPN config, managed session and ownership.
					// Rejoin every old worker before creating any replacement (cap36).
					tunnel.Stop()
					if ctx.Err() != nil {
						return ctx.Err()
					}
					c.bridge.admitLifecycleRestart(ctx, lifecycle.Revision, tunnel.Restart)
					// A superseding sleep keeps groups stopped until a later wake. Physical
					// recovery/stop cancels ctx, so this runtime cannot restart afterwards.
				}
				reportCtx, reportCancel := context.WithTimeout(ctx, time.Second)
				reportErr := c.bridge.sendContext(reportCtx, bridgeMessage{"type": "wake_status", "runtime_epoch": runtimeEpoch, "lifecycle_revision": lifecycle.Revision, "generation": wake.generation, "ready": ready, "total": total})
				reportCancel()
				if reportErr != nil {
					return reportErr
				}
			}
			reportCtx, reportCancel := context.WithTimeout(ctx, time.Second)
			_ = c.bridge.sendContext(reportCtx, channelsStatusMessage(runtimeEpoch, lifecycle.Revision, wake.generation, disp.ActiveWorkers(), n))
			reportCancel()
			cat := c.store.Snapshot()
			if cat == nil {
				return errors.New("BAD_CATALOG")
			}
			now := time.Now()
			selected, nodeErr := managedNodeByID(cat, node.NodeID)
			if nodeErr != nil {
				return nodeErr
			}
			if !selected.Access.ValidAt(now) {
				return errors.New("LEASE_EXPIRED")
			}
			refresh, e := wlbs.UTC(cat.RefreshAfter)
			if e != nil {
				return errors.New("BAD_CATALOG")
			}
			if !refreshing && !now.Before(refresh) && !now.Before(retryAt) {
				refreshing = true
				refreshWorkers.Add(1)
				go func() {
					defer refreshWorkers.Done()
					rctx, rcancel := context.WithTimeout(ctx, 65*time.Second)
					defer rcancel()
					e := c.synchronize(rctx, true)
					select {
					case refreshResult <- e:
					case <-ctx.Done():
					}
				}()
			}
		}
	}
}

func managedServerTime(cat *wlbs.Catalog) string {
	if cat.ServerTime != "" {
		return cat.ServerTime
	}
	return cat.IssuedAt
}
