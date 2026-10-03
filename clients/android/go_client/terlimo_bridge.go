package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const managedBridgeLimit = 256 * 1024

// This protocol is carried only by inherited child pipes, never a public socket.
type managedStart struct {
	V               int                     `json:"v"`
	Type            string                  `json:"type"`
	AttemptID       string                  `json:"attempt_id"`
	CatalogCycle    string                  `json:"catalog_cycle"`
	Link            string                  `json:"link"`
	PublicKey       string                  `json:"public_key_spki"`
	InstallationID  string                  `json:"installation_id"`
	State           string                  `json:"state_b64"`
	Issuers         map[string]string       `json:"issuers"`
	PhysicalNetwork string                  `json:"physical_network_handle"`
	ProbeURL        string                  `json:"probe_url"`
	ExpectedExitIP  string                  `json:"expected_exit_ip"`
	ProbeByNode     map[string]managedProbe `json:"probe_by_node"`
	// Optional mobile-v1 account-access seed. Empty keeps the existing managed flow
	// unchanged; no fallback URL is ever synthesized.
	MobileBaseURL     string                     `json:"mobile_base_url"`
	MobileEnvironment string                     `json:"mobile_environment"`
	MobileSelection   *mobileSelectionPreference `json:"mobile_selection,omitempty"`
	// Durable receipt namespace blob loaded from the host AtomicFile; never wlbs state.
	AccountAccessStateB64 string `json:"accountaccess_state_b64"`
	// Optional public service-channel seed (JSON object, no secret). Empty keeps the
	// accepted HTTPS transport unchanged; a configured seed routes every mobile API
	// call through the service channel only, with an explicit error instead of a
	// silent direct-HTTPS fallback.
	ServiceSeed string `json:"service_seed"`
	// Durable service seed state (last-good cached seed + user hash override) from the
	// host AtomicFile namespace. It can never install a builtin seed: without the
	// packaged service_seed it is a configuration error, not an HTTPS fallback.
	ServiceSeedStateB64  string `json:"service_seed_state_b64"`
	RecoveryVerifyKeyB64 string `json:"recovery_verify_key_b64"`
	RecoveryCode         string `json:"recovery_code"`
	// Durable onboarding attempt identity (request_key/intent/intent metadata) from the
	// host AtomicFile namespace. It is persisted before the first intent call and
	// survives cancel/restart; no bootstrap secret is ever part of it.
	OnboardingFlowStateB64 string `json:"onboarding_flow_state_b64"`
}

type managedProbe struct {
	ProbeURL       string `json:"probe_url"`
	ExpectedExitIP string `json:"expected_exit_ip"`
}

type bridgeMessage map[string]any
type managedWorkerContextKey struct{}

type managedSwitch struct {
	ID       string
	NodeID   string
	Revision string
}

// managedProbeRequest is one manual probe_node command carried on the bridge
// probe channel. ProbeID is the optional host probe run identifier: HasProbeID
// is false for a legacy command without the field (nothing is echoed back),
// and true only for a validated 1..64 char [A-Za-z0-9_-] token.
type managedProbeRequest struct {
	NodeID     string
	ProbeID    string
	HasProbeID bool
}

// validManagedProbeID bounds the host probe run identifier: 1..64 chars of
// [A-Za-z0-9_-]. A malformed or oversized id is never echoed and never starts
// a probe.
func validManagedProbeID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for index := 0; index < len(id); index++ {
		character := id[index]
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '_', character == '-':
		default:
			return false
		}
	}
	return true
}

// echoProbeID copies the host probe run identifier into one result frame of
// exactly that run. Every emitted node_probe_result of a run carries it; a
// legacy command without the field leaves the frame exactly as an old host
// expects it. It is never called for a malformed id: that fails closed with no
// id echoed.
func echoProbeID(message bridgeMessage, req managedProbeRequest) bridgeMessage {
	if req.HasProbeID {
		message["probe_id"] = req.ProbeID
	}
	return message
}

func (m bridgeMessage) string(key string) string { s, _ := m[key].(string); return s }

func (m bridgeMessage) positiveSafeUint64(key string) (uint64, bool) {
	value, ok := m[key].(float64)
	if !ok || value < 1 || value > 9007199254740991 || value != float64(uint64(value)) {
		return 0, false
	}
	return uint64(value), true
}

type managedLifecycle struct {
	Revision uint64
	Sleeping bool
}

type managedBridge struct {
	seedReceiptBusy      atomic.Bool
	seedUpdateEpoch      uint64
	preemptSeed          func()
	lifecycle            managedLifecycle
	runtimeEpoch         uint64
	attempt              string
	out                  io.Writer
	writeSlot            chan struct{}
	runEnd               atomic.Int32
	mu                   sync.Mutex
	waiters              map[string]chan bridgeMessage
	selection            chan string
	explicitSel          bool
	selKey               string
	explicit             chan string
	switchNode           chan managedSwitch
	preference           chan string
	probe                chan managedProbeRequest
	probeStop            chan struct{}
	wake                 chan string
	registration         chan string
	payments             chan bridgeMessage
	usage                chan struct{}
	announcements        chan struct{}
	announcementRead     chan managedAnnouncementRead
	devices              chan string
	deviceDelete         chan managedDeviceDelete
	refreshManual        chan struct{}
	manualRefresh        func()
	manualReceipts       int
	catalogRefresh       func(string)
	catalogCancel        func(string)
	pendingCatalog       string
	pendingCatalogCancel string
	cancel               context.CancelFunc
}

// managedAnnouncementRead is one bounded §11 read-marker command carried on the bridge:
// the announcement id and the host-owned Idempotency-Key. Both are validated at the
// bridge boundary before any request is built; nothing raw travels back to the host.
type managedAnnouncementRead struct {
	AnnouncementID string
	IdempotencyKey string
}

// managedDeviceDelete is one bounded §18–19 device deletion carried on the bridge: the
// device id and the host-owned Idempotency-Key, both validated here before any request.
type managedDeviceDelete struct {
	DeviceID       string
	IdempotencyKey string
	// RequestID is the host-owned correlation id echoed back on success/error so the host
	// can match a result to the exact delete intent (the wire carries no such identity).
	RequestID string
}

// validDeviceBridgeRequestID bounds the host-owned correlation id.
func validDeviceBridgeRequestID(value string) bool {
	return value == "" || (len(value) <= 128 && !containsCRLF(value))
}

// explicitSelection reports whether the selection currently being consumed arrived
// from an explicit Connect funnel. It is set by the bridge reader before the id is
// published on the selection channel and never persists across selections.
func (b *managedBridge) explicitSelection() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.explicitSel
}

// selectionGatewayKey returns the optional gateway_key carried by the selection
// currently being consumed. Empty means the caller falls back to the selected node id
// (which is the stable public gateway id).
func (b *managedBridge) selectionGatewayKey() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selKey
}

// setManualRefresh installs the receipt-time manual refresh hook (the mobile runner
// trigger). With a hook installed a refresh_manual is handled at bridge receipt.
func (b *managedBridge) setManualRefresh(hook func()) {
	b.mu.Lock()
	b.manualRefresh = hook
	b.mu.Unlock()
}

func (b *managedBridge) setCatalogRefreshHooks(refresh, cancel func(string)) {
	b.mu.Lock()
	b.catalogRefresh, b.catalogCancel = refresh, cancel
	pending := b.pendingCatalog
	pendingCancel := b.pendingCatalogCancel
	b.pendingCatalog = ""
	b.pendingCatalogCancel = ""
	b.mu.Unlock()
	if pending != "" {
		refresh(pending)
	}
	if pendingCancel != "" {
		cancel(pendingCancel)
	}
}

func (b *managedBridge) manualReceiptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.manualReceipts
}

func (b *managedBridge) nextRuntimeEpoch() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runtimeEpoch++
	return b.runtimeEpoch
}

func newManagedBridge(out io.Writer, attempt string, cancel context.CancelFunc) *managedBridge {
	return &managedBridge{attempt: attempt, out: out, writeSlot: make(chan struct{}, 1), waiters: make(map[string]chan bridgeMessage), selection: make(chan string, 1), explicit: make(chan string, 1), switchNode: make(chan managedSwitch, 1), preference: make(chan string, 1), probe: make(chan managedProbeRequest, 1), probeStop: make(chan struct{}, 1), wake: make(chan string, 1), registration: make(chan string, 1), payments: make(chan bridgeMessage, 1), usage: make(chan struct{}, 1), announcements: make(chan struct{}, 1), announcementRead: make(chan managedAnnouncementRead, 1), devices: make(chan string, 1), deviceDelete: make(chan managedDeviceDelete, 1), refreshManual: make(chan struct{}, 1), cancel: cancel}
}

func (b *managedBridge) lifecycleSnapshot() managedLifecycle {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lifecycle
}

// Admit restart atomically with lifecycle input. Work only schedules cancellable
// workers; it must not perform network I/O or wait while holding this lock.
func (b *managedBridge) admitLifecycleRestart(ctx context.Context, revision uint64, start func()) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx.Err() != nil || b.lifecycle.Revision != revision || b.lifecycle.Sleeping {
		return false
	}
	start()
	return true
}

func bridgeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("RANDOM_UNAVAILABLE")
	}
	return hex.EncodeToString(b[:])
}

func (b *managedBridge) send(m bridgeMessage) error {
	return b.sendContext(context.Background(), m)
}

// switchDiag emits one fixed, secret-free boundary marker for an explicit manual switch.
// The host logs stage/reason tokens only; unknown stages collapse on the host side.
func switchDiag(b *managedBridge, stage string) {
	_ = b.send(bridgeMessage{"type": "switch_diag", "stage": stage})
}

// switchFailureMessage builds the terminal failed switch_result frame. The runner sends it with
// a live sender context because the 10 s switch budget is usually exhausted when it is produced.
func switchFailureMessage(targetID, switchID, revision, code string, rollbackAllowed bool) bridgeMessage {
	return bridgeMessage{"type": "switch_result", "node_id": targetID, "switch_id": switchID,
		"catalog_revision": revision, "status": "failed", "code": code, "rollback_allowed": rollbackAllowed}
}

type bridgeDeadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

func (b *managedBridge) sendContext(ctx context.Context, m bridgeMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.string("type") == "catalog" || m.string("type") == "catalog_stage" {
		if cycle := catalogCycleFromContext(ctx); cycle != "" {
			m["catalog_cycle"] = cycle
		}
	}
	m["v"] = 1
	m["attempt_id"] = b.attempt
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > managedBridgeLimit {
		return errors.New("BRIDGE_MESSAGE_INVALID")
	}
	select {
	case b.writeSlot <- struct{}{}:
		defer func() { <-b.writeSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if deadline, ok := ctx.Deadline(); ok {
		if writer, supported := b.out.(bridgeDeadlineWriter); supported {
			if err := writer.SetWriteDeadline(deadline); err != nil {
				return err
			}
			defer writer.SetWriteDeadline(time.Time{})
		}
	}
	framed := append(raw, '\n')
	n, err := b.out.Write(framed)
	if err == nil && n != len(framed) {
		return io.ErrShortWrite
	}
	if err == nil && m.string("type") == "catalog" {
		markCatalogPublished(ctx)
	}
	return err
}

func (b *managedBridge) request(ctx context.Context, m bridgeMessage, idField, resultType string) (bridgeMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := bridgeID()
	m[idField] = id
	ch := make(chan bridgeMessage, 1)
	b.mu.Lock()
	b.waiters[id] = ch
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.waiters, id); b.mu.Unlock() }()
	if err := b.sendContext(ctx, m); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.string("type") != resultType || r.string("error") != "" {
			return nil, errors.New("HOST_OPERATION_FAILED")
		}
		return r, nil
	}
}

const (
	runEndNone int32 = iota
	runEndEOF
	runEndScanError
	runEndCtxAlreadyCanceled
	runEndExplicitCancel
)

// noteRunEnd records the FIRST observed read-loop end cause only; later observations never
// overwrite it. No raw error text is stored (fixed enum for the bounded exit diagnostic).
func (b *managedBridge) noteRunEnd(code int32) { b.runEnd.CompareAndSwap(runEndNone, code) }

// RunEndToken returns the fixed source token of the read loop end, or "NONE".
func (b *managedBridge) RunEndToken() string {
	switch b.runEnd.Load() {
	case runEndEOF:
		return "STDIN_EOF"
	case runEndScanError:
		return "STDIN_ERROR"
	case runEndCtxAlreadyCanceled:
		return "CTX_ALREADY_CANCELED"
	case runEndExplicitCancel:
		return "EXPLICIT_CANCEL"
	default:
		return "NONE"
	}
}

func (b *managedBridge) read(ctx context.Context, scanner *bufio.Scanner) {
	defer func() {
		if scanner.Err() != nil {
			b.noteRunEnd(runEndScanError)
		} else {
			b.noteRunEnd(runEndEOF)
		}
		b.cancel()
	}()
	for scanner.Scan() {
		if ctx.Err() != nil {
			b.noteRunEnd(runEndCtxAlreadyCanceled)
			return
		}
		var m bridgeMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil || m.string("attempt_id") != b.attempt || m["v"] != float64(1) {
			continue
		}
		if !b.preemptSeedUpdate(m) {
			continue
		}
		switch m.string("type") {
		case "device_sleep", "device_wake":
			if len(m) != 4 {
				b.finishSeedReceipt()
				continue
			}
			b.mu.Lock()
			revision, ok := m.positiveSafeUint64("lifecycle_revision")
			if !ok || revision <= b.lifecycle.Revision {
				b.mu.Unlock()
				b.finishSeedReceipt()
				continue
			}
			resumed := b.lifecycle.Sleeping
			b.lifecycle.Revision = revision
			b.lifecycle.Sleeping = m.string("type") == "device_sleep"
			b.mu.Unlock()
			if m.string("type") == "device_wake" {
				reason := "wake"
				if resumed {
					// The existing sleep->wake resume is the recovery lifecycle event;
					// no new host message is invented.
					reason = "recovery"
				}
				select {
				case b.wake <- reason:
				default:
				}
			}
		case "cancel":
			// Explicit host cancel: the nested defer would otherwise classify the clean
			// scanner end as STDIN_EOF. First-cause wins, so record this branch first.
			b.noteRunEnd(runEndExplicitCancel)
			return
		case "select_node":
			// `explicit_connect` is set only by the SessionService select / one-tap
			// retained paths after the mandatory VPN consent; every other select source
			// clears it so a background resume can never start the onboarding hour.
			// The optional `gateway_key` is the selected gateway id sent by the host.
			explicit, _ := m["explicit_connect"].(bool)
			b.mu.Lock()
			b.explicitSel = explicit
			b.selKey = m.string("gateway_key")
			b.mu.Unlock()
			select {
			case b.selection <- m.string("node_id"):
			default:
			}
		case "explicit_connect":
			// Pre-admission first-connect funnel: a separate host action issued only after
			// the mandatory VPN consent. The optional `gateway_key` is the selected
			// gateway id; without it the selected preference (or the server assignment)
			// applies. Coalesced: a repeated tap is idempotent.
			select {
			case b.explicit <- m.string("gateway_key"):
			default:
			}
		case "request_telegram_registration", "refresh_telegram_registration", "activate_trial":
			// S3-A host commands. The mobile runner owns the HTTP call; the bridge only
			// forwards the fixed op. Coalesced: a repeated tap is idempotent.
			select {
			case b.registration <- m.string("type"):
			default:
			}
		case paymentActionPlansList, paymentActionQuoteCreate, paymentActionPaymentCreate, paymentActionPaymentGet:
			// S5 payment host actions. Each carries its own bounded fields and expects
			// its own result event; nothing is coalesced or dropped silently (a full
			// queue is answered with a bounded BUSY error on the matching event).
			select {
			case b.payments <- m:
			default:
				_ = b.send(bridgeMessage{"type": paymentEventForAction(m.string("type")), "state": "error", "code": "BUSY"})
			}
		case usageActionRead:
			// §07.4 account-traffic read. Coalesced: a repeated request while one is
			// pending is idempotent; the native runner performs a bounded GET /usage.
			select {
			case b.usage <- struct{}{}:
			default:
			}
		case announcementsActionList:
			// §11 announcements read. Coalesced: a repeated request while one is pending
			// is idempotent; the native runner performs a bounded GET /announcements.
			select {
			case b.announcements <- struct{}{}:
			default:
			}
		case announcementsActionRead:
			// §11 idempotent read marker: bounded announcement id plus the host-owned
			// Idempotency-Key, both validated here before any request is built. A
			// malformed field fails closed with a bounded code and starts nothing.
			read := managedAnnouncementRead{
				AnnouncementID: m.string("announcement_id"),
				IdempotencyKey: m.string("idempotency_key"),
			}
			if !validAnnouncementBridgeID(read.AnnouncementID) || !validAnnouncementBridgeKey(read.IdempotencyKey) {
				_ = b.send(bridgeMessage{"type": announcementsEventRead, "state": "error", "code": "INVALID_REQUEST"})
			} else {
				select {
				case b.announcementRead <- read:
				default:
					_ = b.send(bridgeMessage{"type": announcementsEventRead, "state": "error", "code": "BUSY"})
				}
			}
		case devicesActionList:
			// §18–19 device list read. The host-owned correlation id is echoed back so a
			// late list can never apply to a different read/account. Coalesced while pending.
			requestID := m.string("client_request_id")
			if !validDeviceBridgeRequestID(requestID) {
				_ = b.send(bridgeMessage{"type": devicesEventList, "state": "error",
					"code": "INVALID_REQUEST", "client_request_id": requestID})
			} else {
				select {
				case b.devices <- requestID:
				default:
					_ = b.send(bridgeMessage{"type": devicesEventList, "state": "error",
						"code": "BUSY", "client_request_id": requestID})
				}
			}
		case devicesActionDelete:
			// §§18–19 explicit single-device deletion: a bounded device id and the
			// host-owned Idempotency-Key, validated before any request. Never repeated
			// blindly; a malformed field fails closed with a bounded code.
			del := managedDeviceDelete{
				DeviceID:       m.string("device_id"),
				IdempotencyKey: m.string("idempotency_key"),
				RequestID:      m.string("client_request_id"),
			}
			if !validDeviceBridgeID(del.DeviceID) || !validDeviceBridgeKey(del.IdempotencyKey) ||
				!validDeviceBridgeRequestID(del.RequestID) {
				_ = b.send(bridgeMessage{"type": devicesEventDelete, "state": "error",
					"code": "INVALID_REQUEST", "client_request_id": del.RequestID})
			} else {
				select {
				case b.deviceDelete <- del:
				default:
					_ = b.send(bridgeMessage{"type": devicesEventDelete, "state": "error",
						"code": "BUSY", "client_request_id": del.RequestID})
				}
			}
		case "cancel_catalog":
			cycle := m.string("catalog_cycle")
			if !validCatalogCycle(cycle) {
				b.finishSeedReceipt()
				continue
			}
			b.mu.Lock()
			hook := b.catalogCancel
			if hook == nil {
				b.pendingCatalogCancel = cycle
			}
			if b.pendingCatalog == cycle {
				b.pendingCatalog = ""
			}
			b.mu.Unlock()
			if hook != nil {
				hook(cycle)
			}
		case "refresh_manual":
			if _, tagged := m["catalog_cycle"]; tagged {
				cycle := m.string("catalog_cycle")
				if !validCatalogCycle(cycle) {
					b.finishSeedReceipt()
					continue
				}
				b.mu.Lock()
				hook := b.catalogRefresh
				if hook == nil {
					b.pendingCatalog = cycle
				}
				b.mu.Unlock()
				if hook != nil {
					hook(cycle)
				}
				b.finishSeedReceipt()
				continue
			}
			// Bounded manual refresh of an already active attempt. It only wakes the
			// existing mobile runner for one cycle over the same authenticated /me,
			// gateways, access/sync and catalog operations; no new endpoint, timer or
			// poll. When a receipt hook is installed it is invoked right here (the
			// bridge reader), so a blocking dispatcher cannot delay a duplicate past
			// the cycle it belongs to and the Runner fence coalesces at receipt time.
			b.mu.Lock()
			b.manualReceipts++
			receipt := b.manualReceipts
			hook := b.manualRefresh
			b.mu.Unlock()
			fmt.Fprintf(os.Stderr, "refreshstage:receive:%d\n", receipt)
			if hook != nil {
				hook()
			} else {
				select {
				case b.refreshManual <- struct{}{}:
				default:
				}
			}
		case "switch_node":
			switchRequest := managedSwitch{ID: m.string("switch_id"), NodeID: m.string("node_id"), Revision: m.string("catalog_revision")}
			select {
			case b.switchNode <- switchRequest:
				switchDiag(b, "bridge_accepted")
			default:
				switchDiag(b, "bridge_full")
			}
		case "choose_node":
			// Preference updates coalesce to the latest requested ID. The UI
			// awaits persisted catalog acknowledgement before using that choice.
			select {
			case <-b.preference:
			default:
			}
			select {
			case b.preference <- m.string("node_id"):
			default:
			}
		case "probe_node":
			// Optional host probe run identifier. A malformed/oversized field
			// fails closed immediately: no probe starts and no id is echoed, so
			// the host can never attribute this frame to a run.
			request := managedProbeRequest{NodeID: m.string("node_id")}
			if rawID, present := m["probe_id"]; present {
				probeID, ok := rawID.(string)
				if !ok || !validManagedProbeID(probeID) {
					_ = b.send(bridgeMessage{"type": "node_probe_result", "node_id": request.NodeID, "status": "failed"})
					b.finishSeedReceipt()
					continue
				}
				request.ProbeID = probeID
				request.HasProbeID = true
			}
			select {
			case b.probe <- request:
			default:
				_ = b.send(echoProbeID(bridgeMessage{"type": "node_probe_result", "node_id": request.NodeID, "status": "busy"}, request))
			}
		case "cancel_probe":
			select {
			case b.probeStop <- struct{}{}:
			default:
			}
		case "sign_result", "persist_result", "vpn_result":
			id := m.string("request_id")
			if m.string("type") == "sign_result" {
				id = m.string("signing_request_id")
			}
			b.mu.Lock()
			ch := b.waiters[id]
			b.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
		case "captcha_result":
			value := m.string("value")
			if value == "" {
				value = "error:cancelled"
			}
			enqueueCaptchaResult(CaptchaResult{RequestID: m.string("request_id"), Value: value})
		}
		b.finishSeedReceipt()
	}
}

func (b *managedBridge) sign(ctx context.Context, transcript []byte) ([]byte, error) {
	kind := ""
	if len(transcript) == len("WLBS-POP-1\x00")+16+16+32+32 && strings.HasPrefix(string(transcript), "WLBS-POP-1\x00") {
		kind = "bootstrap"
	}
	if len(transcript) == len("WL-VPN-POP-1\x00")+32+16+32+32 && strings.HasPrefix(string(transcript), "WL-VPN-POP-1\x00") {
		kind = "vpn"
	}
	if kind == "" {
		return nil, errors.New("SIGN_TRANSCRIPT_INVALID")
	}
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	worker, _ := ctx.Value(managedWorkerContextKey{}).(string)
	if worker == "" {
		worker = "bootstrap"
	}
	r, err := b.request(wait, bridgeMessage{"type": "sign", "kind": kind, "worker_id": worker, "deadline_unix_ms": deadline.UnixMilli(), "transcript_b64": base64.RawURLEncoding.EncodeToString(transcript)}, "signing_request_id", "sign_result")
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.string("signature_b64"))
	if err != nil || len(sig) < 8 || len(sig) > 80 {
		return nil, errors.New("SIGNATURE_INVALID")
	}
	return sig, nil
}

func (b *managedBridge) persist(ctx context.Context, state []byte) error {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := b.request(wait, bridgeMessage{"type": "persist", "state_b64": base64.RawURLEncoding.EncodeToString(state)}, "request_id", "persist_result")
	return err
}

// persistNamespace stores bytes under one versioned namespace through the same
// persist/persist_result channel, so wlbs state/link/catalog are never touched.
func (b *managedBridge) persistNamespace(ctx context.Context, namespace string, state []byte) error {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	message := bridgeMessage{"type": "persist", "namespace": namespace, "state_b64": base64.RawURLEncoding.EncodeToString(state)}
	if epoch, ok := ctx.Value(optionalSeedEpochKey{}).(uint64); ok && namespace == "service_seed_v1" {
		message["optional_seed_epoch"] = strconv.FormatUint(epoch, 10)
		if deadline, ok := wait.Deadline(); ok {
			message["deadline_unix_ms"] = deadline.UnixMilli()
		}
	}
	_, err := b.request(wait, message, "request_id", "persist_result")
	return err
}
