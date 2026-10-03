package main

// Optional mobile-v1 account-access path. It activates only when the host supplies a
// non-personal base URL in the managed start frame; the existing managed flow is
// unchanged when the field is absent. The single native Runner owns retry/refresh,
// the coordinator owns /me, /gateways, /access/sync and /operations, and the host only
// parses and displays. The verified /me + /gateways pair is projected into the existing
// wlbs technical container with a typed mobile proof; legacy business fields are never
// synthesized from the old subscription.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/onboarding"
	"wg-turn-client/servicechannel"
	"wg-turn-client/wlbs"
)

// Numbers mirror the existing native profile: bounded 3 attempts with jittered waits and
// the 15 s bridge sign deadline. The retry_after cap (1 h) comes from the real error
// parser (DecodeErrorStrict).
const mobileHTTPTimeout = 15 * time.Second

// mobileSubscriptionRef is the technical, mobile-owned reference. It is never copied
// from a legacy subscription/link value.
func mobileSubscriptionRef(fingerprint string) string { return "mobile:" + fingerprint }

// managedMobile owns the mobile projection/admission boundary. The accountaccess Runner
// stays the single retry/refresh owner; this type only reacts to verified pairs, keeps
// the projected store fresh and stops the real data plane on proven stop decisions.
type managedMobile struct {
	seedDoer          *servicechannel.Doer
	seedKey           string
	idleSeedPublished bool
	runner            *accountaccess.Runner
	controller        *managedController
	bridge            *managedBridge
	fingerprint       string
	explicit          *onboarding.Flow
	// session is the mobile PoP session owner: after a successful hour activation it is
	// refreshed so the next Ensure authenticates with the preferred (access:sync)
	// scopes instead of the pre-hour management-only fallback. Never upgraded in place.
	session         *accountaccess.MobileSession
	client          *accountaccess.Client
	transportCloser io.Closer
	recovery        *mobileRecoveryTransport
	catalogOps      catalogOperations

	mu            sync.Mutex
	probeSnapshot *mobileProbeSnapshot
	probeSerial   uint64
	me            *accountaccess.MeResponse
	latestMe      *accountaccess.MeResponse
	catalog       *accountaccess.CatalogResponse
	browse        *accountaccess.BrowseCatalogResponse
	browseNodes   string
	browseExpires string
	browseSig     chan struct{}
	browseGen     int64
	verified      uint64
	verifiedSig   chan struct{}
	verifiedErr   error
	runCtx        context.Context
	ready         bool
	projectionErr error
	decision      *accountaccess.AdmissionDecision
	pending       string
	timer         *time.Timer

	// manualCycleFinishedCb is an optional observation hook for focused tests: it fires
	// right after a manual refresh cycle finished. It never changes behaviour.
	manualCycleFinishedCb func()
}

func (m *managedMobile) notifyManualCycleFinished() {
	if m.manualCycleFinishedCb != nil {
		m.manualCycleFinishedCb()
	}
}

func newManagedMobile(start managedStart, spkiDER []byte, signer accountaccess.Signer, bridge *managedBridge, controller *managedController) (*managedMobile, error) {
	if start.CatalogCycle != "" && !validCatalogCycle(start.CatalogCycle) {
		return nil, fmt.Errorf("catalog cycle invalid")
	}
	if controller == nil {
		return nil, fmt.Errorf("mobile controller required")
	}
	environment := accountaccess.PoPEnvironment(start.MobileEnvironment)
	if environment == "" {
		environment = accountaccess.EnvironmentTest
	}
	var servicePersist servicechannel.PersistFunc
	if bridge != nil {
		servicePersist = func(ctx context.Context, payload []byte) error {
			return bridge.persistNamespace(ctx, servicechannel.StateNamespace, payload)
		}
	}
	transport, serviceSeed, err := newMobileTransportAndStore(start, servicePersist)
	if err != nil {
		return nil, err
	}
	configureCatalogStages(transport, bridge)
	var transportCloser io.Closer
	if closer, ok := transport.(io.Closer); ok {
		transportCloser = closer
	}
	session, err := accountaccess.NewMobileSession(accountaccess.MobileConfig{
		BaseURL:           start.MobileBaseURL,
		Environment:       environment,
		SPKIDER:           spkiDER,
		HTTP:              transport,
		Signer:            signer,
		DisableEnrollment: start.RecoveryCode != "",
	})
	if err != nil {
		return nil, err
	}
	var stateBlob []byte
	if start.AccountAccessStateB64 != "" {
		stateBlob, err = base64.RawURLEncoding.DecodeString(start.AccountAccessStateB64)
		if err != nil {
			return nil, err
		}
	}
	store, err := accountaccess.NewBridgeReceiptStore(stateBlob, func(ctx context.Context, payload []byte) error {
		return bridge.persistNamespace(ctx, accountaccess.ReceiptNamespace, payload)
	})
	if err != nil {
		return nil, err
	}
	client := &accountaccess.Client{
		BaseURL:         strings.TrimRight(start.MobileBaseURL, "/") + "/api/mobile/v1",
		HTTP:            transport,
		Tokens:          session,
		PaymentContract: 2,
	}
	// Fixed, secret-free observation hooks: the /me wire response boundary and the
	// /gateways request/status boundaries are emitted, with the same attempt base as the
	// other cycle markers, and only through the fixed vocabulary. ME_RESPONSE_* fires at
	// the wire READ (before strict decode); AFTER_ME_READ fires later, after
	// Coordinator.Refresh returns. Failures stay silent; no behavior changes.
	client.OnRequest = func(path string, _ int) {
		if path == "/gateways" {
			diagEmitCycle(cycleStageGWRequestBegin)
		}
	}
	client.OnResponse = func(path string, status int) {
		switch path {
		case "/gateways":
			diagEmitCycle(accountaccess.GatewayRequestStage(status))
		case "/me":
			diagEmitCycle(accountaccess.MeResponseStage(status))
		case "/usage":
			// Bounded HTTP class only; distinguishes non-2xx from a transport failure
			// (no OnResponse) and from a 2xx that then fails strict decode.
			emitUsageStage(usageResponseStage(status))
		}
	}
	coordinator, err := accountaccess.NewCoordinator(accountaccess.Options{
		Client:    client,
		AttemptID: start.AttemptID,
		Store:     store,
		Subject:   session.Subject,
	})
	if err != nil {
		return nil, err
	}
	m := &managedMobile{
		controller:      controller,
		bridge:          bridge,
		session:         session,
		client:          client,
		transportCloser: transportCloser,
		fingerprint:     session.Fingerprint(),
		verifiedSig:     make(chan struct{}, 1),
		browseSig:       make(chan struct{}, 1),
	}
	if doer, ok := transport.(*servicechannel.Doer); ok && start.RecoveryCode == "" {
		m.seedDoer, m.seedKey = doer, start.RecoveryVerifyKeyB64
		doer.Channel.PreserveConnectionSeed = true
	}
	m.catalogOps.initialize(start.CatalogCycle)
	if recovery, ok := transport.(*mobileRecoveryTransport); ok {
		m.recovery = recovery
	}
	identity := onboarding.Identity{
		Environment:    string(environment),
		InstallationID: session.Fingerprint(),
		Signer:         signer,
	}
	intentClient, err := onboarding.NewClient(transport, session, start.MobileBaseURL, identity)
	if err != nil {
		return nil, err
	}
	intentClient.ObserveStage = func(stage string) {
		emitOnboardingToken(onboardingIntentStageTokens[stage])
	}
	starter := &onboarding.BootstrapStarter{
		Identity: identity,
		Dial:     onboardingBootstrapDial(serviceSeed),
	}
	var flowSeed []byte
	if start.OnboardingFlowStateB64 != "" {
		flowSeed, err = base64.RawURLEncoding.DecodeString(start.OnboardingFlowStateB64)
		if err != nil {
			return nil, err
		}
	}
	flowStore := onboarding.NewDurableStore(flowSeed, session.Fingerprint(), string(environment),
		func(ctx context.Context, payload []byte) error {
			if bridge == nil {
				return onboarding.ErrStartUnavailable
			}
			return bridge.persistNamespace(ctx, onboarding.FlowStateNamespace, payload)
		})
	flow, err := onboarding.NewFlow(flowStore, intentClient, starter, onboarding.Options{})
	if err != nil {
		return nil, err
	}
	m.explicit = flow
	runner, err := accountaccess.NewRunner(accountaccess.RunnerConfig{
		Session:     session,
		Coordinator: coordinator,
		Emit: func(ctx context.Context, payload map[string]any) error {
			return bridge.sendContext(ctx, bridgeMessage(payload))
		},
		OnVerified:  m.onVerified,
		OnMe:        m.onCurrentMe,
		DisplayOnly: func(ctx context.Context) bool { return catalogCycleFromContext(ctx) != "" },
		OnBrowse:    m.onBrowse,
		OnAdmission: m.handleAdmission,
		SelectedNodeID: func() string {
			return controller.selectionID()
		},
		OnError: func(code string) {
			// Fixed, secret-free diagnostic on stderr; never a terminal bridge event.
			m.retireProbeAdmission()
			fmt.Fprintln(os.Stderr, "accountaccess:", code)
		},
		OnStage: func(token string, elapsedMS, utcMS int64) {
			// Fixed, secret-free per-attempt chronology on stderr; invalid values stay silent.
			emitCycleStage(token, elapsedMS, utcMS)
		},
		OnManualCycleFinished: m.notifyManualCycleFinished,
		BeginAttempt:          m.beginCatalogAttempt,
		IdleReady:             m.updateSeedAtIdle,
		PreemptIdle: func() {
			if m.seedDoer != nil {
				m.seedDoer.PreemptOptional()
			}
		},
	})
	if err != nil {
		return nil, err
	}
	m.runner = runner
	return m, nil
}

// onboardingBootstrapDial is the production bootstrap dial constructor. Tests
// override it with the in-process node-compatible listener; production always uses
// the accepted DTLS/WRAP dial.
var onboardingBootstrapDial = func(store *servicechannel.Store) onboarding.BootstrapDialer {
	return onboardingDialer(store)
}

// onboardingDialer is the production BootstrapDialer: the trusted DTLS/WRAP
// bootstrap dial to the assigned gateway endpoint with the one-time ready credential
// as the WRAP classifier. TURN/VK material is the existing public service seed hash
// list (no new secret); without it the starter fails closed instead of inventing a
// transport.
func onboardingDialer(store *servicechannel.Store) onboarding.BootstrapDialer {
	return func(ctx context.Context, endpoint onboarding.GatewayEndpoint, bootstrap onboarding.Bootstrap) (net.Conn, func(), error) {
		if store == nil {
			return nil, nil, onboarding.ErrStartUnavailable
		}
		seed, _, ok := store.Current()
		if !ok || len(seed.VKHashes) == 0 {
			return nil, nil, onboarding.ErrStartUnavailable
		}
		return DialBootstrapTransport(ctx, BootstrapTransportConfig{
			Peer:     &net.UDPAddr{IP: net.ParseIP(endpoint.PeerIP), Port: endpoint.DTLSPort},
			Password: bootstrap.Secret,
			Hashes:   seed.VKHashes,
			Pin:      endpoint.DTLSSPKISHA256,
			StreamID: seed.StreamID,
		})
	}
}

// Explicit onboarding-hour diagnostics carry only fixed, secret-free tokens on stderr
// (`onboarding: TOKEN`). The child never writes backend codes, HTTP statuses, URLs,
// payloads or signatures. The host mirror allowlist (NativeStderrMirror.kt) accepts
// exactly this set: stage tokens report the failing step, terminal tokens the outcome.
const (
	onboardingTokenEntry           = "ENTRY"
	onboardingTokenIntentChallenge = "INTENT_CHALLENGE"
	onboardingTokenIntentSign      = "INTENT_SIGN"
	onboardingTokenIntentPost      = "INTENT_POST"
	onboardingTokenIntentDecode    = "INTENT_DECODE"
	// Fixed challenge-failure classes: distinguish transport, HTTP status class, JSON
	// decode and semantic validation without emitting any raw text/status/body.
	onboardingTokenChallengeTransport   = "CHALLENGE_TRANSPORT"
	onboardingTokenChallengeStatus4xx   = "CHALLENGE_STATUS_CLIENT"
	onboardingTokenChallengeStatus5xx   = "CHALLENGE_STATUS_SERVER"
	onboardingTokenChallengeStatusOther = "CHALLENGE_STATUS_OTHER"
	onboardingTokenChallengeDecode      = "CHALLENGE_DECODE"
	onboardingTokenChallengeSemantic    = "CHALLENGE_SEMANTIC"
	onboardingTokenStartEntry           = "START_ENTRY"
	onboardingTokenStartNotReady        = "START_NOT_READY"
	onboardingTokenStartUnavailable     = "START_UNAVAILABLE"
	// STATE_READY is reserved for a success terminal that stops at the ready poll; the
	// current explicit path always attempts the start, so it reports STARTED.
	onboardingTokenStateReady       = "STATE_READY"
	onboardingTokenStarted          = "STARTED"
	onboardingTokenTimeout          = "ONBOARDING_TIMEOUT"
	onboardingTokenFailed           = "ONBOARDING_FAILED"
	onboardingTokenPendingBudget    = "ONBOARDING_PENDING_BUDGET"
	onboardingTokenErrStartNotReady = "ONBOARDING_START_NOT_READY"
	onboardingTokenErrStartUnavail  = "ONBOARDING_START_UNAVAILABLE"
	onboardingTokenInFlight         = "ONBOARDING_IN_FLIGHT"
	onboardingTokenCancelled        = "ONBOARDING_CANCELLED"
	onboardingTokenCorrelation      = "ONBOARDING_CORRELATION"
	onboardingTokenExpired          = "ONBOARDING_EXPIRED"
	onboardingTokenRevoked          = "ONBOARDING_REVOKED"
	onboardingTokenIntentConflict   = "ONBOARDING_INTENT_CONFLICT"
	onboardingTokenUnknown          = "ONBOARDING_UNKNOWN"
	// Terminal: the server served a display-only browse catalog instead of a
	// credential catalog after a successful onboarding activation.
	onboardingTokenCredentialUnavailable = "ONBOARDING_CREDENTIAL_UNAVAILABLE"
)

// onboardingTokenSet is the fixed vocabulary; no other value may reach stderr/logcat.
var onboardingTokenSet = map[string]bool{
	onboardingTokenEntry: true, onboardingTokenIntentChallenge: true,
	onboardingTokenIntentSign: true, onboardingTokenIntentPost: true,
	onboardingTokenIntentDecode: true, onboardingTokenStartEntry: true,
	onboardingTokenChallengeTransport: true, onboardingTokenChallengeStatus4xx: true,
	onboardingTokenChallengeStatus5xx: true, onboardingTokenChallengeStatusOther: true,
	onboardingTokenChallengeDecode: true, onboardingTokenChallengeSemantic: true,
	onboardingTokenStartNotReady: true, onboardingTokenStartUnavailable: true,
	onboardingTokenStateReady: true, onboardingTokenStarted: true,
	onboardingTokenTimeout: true, onboardingTokenFailed: true,
	onboardingTokenPendingBudget: true, onboardingTokenErrStartNotReady: true,
	onboardingTokenErrStartUnavail: true, onboardingTokenInFlight: true,
	onboardingTokenCancelled: true, onboardingTokenCorrelation: true,
	onboardingTokenExpired: true, onboardingTokenRevoked: true,
	onboardingTokenIntentConflict: true, onboardingTokenUnknown: true,
	onboardingTokenCredentialUnavailable: true,
}

// onboardingIntentStageTokens maps the contract client's fixed failing-stage names.
var onboardingIntentStageTokens = map[string]string{
	"challenge":              onboardingTokenIntentChallenge,
	"challenge_transport":    onboardingTokenChallengeTransport,
	"challenge_status_4xx":   onboardingTokenChallengeStatus4xx,
	"challenge_status_5xx":   onboardingTokenChallengeStatus5xx,
	"challenge_status_other": onboardingTokenChallengeStatusOther,
	"challenge_decode":       onboardingTokenChallengeDecode,
	"challenge_semantic":     onboardingTokenChallengeSemantic,
	"sign":                   onboardingTokenIntentSign,
	"post":                   onboardingTokenIntentPost,
	"decode":                 onboardingTokenIntentDecode,
}

// onboardingDiagnostics is the stderr sink of the fixed tokens. Production keeps
// os.Stderr; focused tests substitute a buffer without changing behavior.
var onboardingDiagnostics io.Writer = os.Stderr

// emitAccountAccessCode writes one fixed secret-free accountaccess code to stderr for the
// host mirror. Used only for bounded registration-stage diagnostics.
func emitAccountAccessCode(code string) {
	fmt.Fprintln(os.Stderr, "accountaccess:", code)
}

// emitOnboardingToken writes exactly one `onboarding: TOKEN` line and only ever
// emits a value from the fixed vocabulary; anything else collapses to UNKNOWN.
func emitOnboardingToken(token string) {
	if !onboardingTokenSet[token] {
		token = onboardingTokenUnknown
	}
	fmt.Fprintln(onboardingDiagnostics, "onboarding:", token)
}

// onboardingFailureToken normalizes one explicit onboarding error to exactly one fixed
// token. Raw backend codes, HTTP statuses, transport text and payload fragments never pass.
// Sentinels keep their distinct external meaning; an API error whose backend code has no
// mapped token is UNKNOWN, and any other explicit failure is the generic FAILED.
func onboardingFailureToken(err error) string {
	var apiError *onboarding.APIError
	if errors.As(err, &apiError) {
		switch {
		case errors.Is(apiError, onboarding.ErrIntentExpired):
			return onboardingTokenExpired
		case errors.Is(apiError, onboarding.ErrIntentRevoked):
			return onboardingTokenRevoked
		case errors.Is(apiError, onboarding.ErrIntentConflict):
			return onboardingTokenIntentConflict
		default:
			return onboardingTokenUnknown
		}
	}
	switch {
	case err == nil:
		return onboardingTokenUnknown
	case errors.Is(err, onboarding.ErrIntentConflict):
		return onboardingTokenIntentConflict
	case errors.Is(err, onboarding.ErrInFlight):
		return onboardingTokenInFlight
	case errors.Is(err, onboarding.ErrCancelled):
		return onboardingTokenCancelled
	case errors.Is(err, onboarding.ErrPendingBudget):
		return onboardingTokenPendingBudget
	case errors.Is(err, onboarding.ErrStartUnavailable):
		return onboardingTokenErrStartUnavail
	case errors.Is(err, onboarding.ErrStartNotReady):
		return onboardingTokenErrStartNotReady
	case errors.Is(err, onboarding.ErrCorrelation):
		return onboardingTokenCorrelation
	case errors.Is(err, context.Canceled):
		return onboardingTokenCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return onboardingTokenTimeout
	default:
		return onboardingTokenFailed
	}
}

// explicitConnect runs the explicit-Connect onboarding-hour flow for one explicit
// selection (after VPN consent, from the SessionService select / one-tap retained
// funnels only). gatewayKey is the optional selected gateway id from the host action;
// without it the persisted selection preference applies, and without any selection no
// key is sent (the backend assigns as today). A ready intent performs the signed
// onboarding.start over the trusted assigned-gateway bootstrap; a started poll is
// recovery only and never reuses the old bootstrap secret. It is never invoked from
// resume, background or wake.
func (m *managedMobile) explicitConnect(ctx context.Context, gatewayKey string) (bool, error) {
	// Exactly one ENTRY and exactly one terminal token per call; every stage token is
	// failure-only (or a single START_ENTRY), so repeated pending polls emit nothing.
	emitOnboardingToken(onboardingTokenEntry)
	if m.explicit == nil {
		emitOnboardingToken(onboardingTokenStartUnavailable)
		emitOnboardingToken(onboardingTokenErrStartUnavail)
		return false, onboarding.ErrStartUnavailable
	}
	if gatewayKey == "" && m.controller != nil {
		gatewayKey = m.controller.selectionID()
	}
	result, err := m.explicit.ConnectWithGateway(ctx, gatewayKey)
	if err != nil {
		emitOnboardingToken(onboardingFailureToken(err))
		return false, err
	}
	switch result.State {
	case onboarding.StateReady:
		if result.Poll == nil {
			emitOnboardingToken(onboardingTokenStartNotReady)
			emitOnboardingToken(onboardingTokenErrStartNotReady)
			return false, onboarding.ErrStartNotReady
		}
		emitOnboardingToken(onboardingTokenStartEntry)
		_, err := m.explicit.Start(ctx, *result.Poll)
		if err != nil {
			switch {
			case errors.Is(err, onboarding.ErrStartNotReady):
				emitOnboardingToken(onboardingTokenStartNotReady)
			case errors.Is(err, onboarding.ErrStartUnavailable):
				emitOnboardingToken(onboardingTokenStartUnavailable)
			}
			emitOnboardingToken(onboardingFailureToken(err))
			return false, err
		}
		emitOnboardingToken(onboardingTokenStarted)
		m.refreshSessionAfterActivation()
		return true, nil
	case onboarding.StateStarted:
		emitOnboardingToken(onboardingTokenStarted)
		m.refreshSessionAfterActivation()
		return true, nil
	case onboarding.StateFailed:
		emitOnboardingToken(onboardingTokenFailed)
		return false, nil
	case onboarding.StateExpired:
		emitOnboardingToken(onboardingTokenExpired)
		return false, nil
	case onboarding.StateRevoked:
		emitOnboardingToken(onboardingTokenRevoked)
		return false, nil
	default:
		emitOnboardingToken(onboardingTokenStartNotReady)
		emitOnboardingToken(onboardingTokenErrStartNotReady)
		return false, onboarding.ErrStartNotReady
	}
}

// awaitOnboardingRefresh waits for the post-activation refresh outcome within the
// caller's existing budget: a new verified credential pair continues the single
// Connect; a browse callback (even an identical/deduped one) completes with a fixed
// explicit error; parent cancellation/deadline is returned (never nil).
func (m *managedMobile) awaitOnboardingRefresh(ctx context.Context) error {
	m.mu.Lock()
	verifiedBefore := m.verified
	browseBefore := m.browseGen
	verifiedSig := m.verifiedSig
	browseSig := m.browseSig
	m.mu.Unlock()
	if m.runner != nil {
		m.runner.Trigger("manual")
	}
	for {
		// An already-expired parent is never reported as a successful refresh.
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		if m.verified > verifiedBefore {
			err := m.verifiedErr
			m.mu.Unlock()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		if m.browseGen > browseBefore {
			m.mu.Unlock()
			emitOnboardingToken(onboardingTokenCredentialUnavailable)
			return errors.New("ONBOARDING_CREDENTIAL_UNAVAILABLE")
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-verifiedSig:
		case <-browseSig:
		}
	}
}

// refreshSessionAfterActivation drops the current bearer so the next Ensure re-runs
// the preferred-scope PoP session request (access:sync, no management-only) now that
// the hour is active for this installation. It does not link an account: account_ref
// may stay null (installation hour), so the pre-hour management bearer is never reused.
func (m *managedMobile) refreshSessionAfterActivation() {
	m.retireProbeAdmission()
	if m.session != nil {
		m.session.Refresh()
	}
}

// run owns the mobile lifecycle loop until the managed attempt ends. Existing host
// lifecycle messages keep their meaning: a plain wake and a sleep->wake resume.
func (m *managedMobile) run(ctx context.Context, bridge *managedBridge) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
	defer m.stopTimer()
	defer m.retireProbeAdmission()
	if m.seedDoer != nil {
		bridge.mu.Lock()
		bridge.preemptSeed = m.seedDoer.PreemptOptional
		bridge.mu.Unlock()
	}
	if m.runner != nil {
		// §11/§29: coalesce manual refresh at bridge receipt (before any blocking
		// dispatcher handler); the Runner fence keeps single-flight semantics.
		bridge.setManualRefresh(func() { m.runner.TriggerManual() })
		bridge.setCatalogRefreshHooks(func(cycle string) {
			if m.catalogOps.request(cycle) {
				m.runner.TriggerManual()
			}
		}, m.catalogOps.cancel)
	}
	if m.transportCloser != nil {
		defer m.transportCloser.Close()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case reason := <-bridge.wake:
				m.runner.Trigger(reason)
			case op := <-bridge.registration:
				m.handleRegistration(ctx, op)
			case request := <-bridge.referrals:
				m.handleReferralAction(ctx, request)
			case action := <-bridge.payments:
				m.handlePaymentAction(ctx, action)
			case <-bridge.usage:
				m.handleUsageRead(ctx)
			case <-bridge.announcements:
				m.handleAnnouncementsList(ctx)
			case request := <-bridge.announcementRead:
				m.handleAnnouncementRead(ctx, request)
			case listRequestID := <-bridge.devices:
				m.handleDevicesList(ctx, listRequestID)
			case del := <-bridge.deviceDelete:
				m.handleDeviceDelete(ctx, del)
			case <-bridge.refreshManual:
				// One bounded runner cycle on the active attempt; the same cycle the
				// periodic/refresh seam uses. No new timer and no VPN change.
				if m.runner != nil {
					m.runner.TriggerManual()
				}
			}
		}
	}()
	_ = m.runner.Run(ctx)
}

// emitRegistrationStatus publishes the server-owned registration display subset from the
// last verified /me. It is a pure projection: no eligibility is derived here.
func (m *managedMobile) emitRegistrationStatus(bridge *managedBridge, me accountaccess.MeResponse) {
	if bridge == nil {
		return
	}
	state := me.Registration.State
	if state == "" {
		state = "none"
	}
	message := bridgeMessage{
		"type":               "telegram_registration_status",
		"state":              state,
		"within_hour":        me.Registration.WithinHour,
		"trial_available":    me.Registration.TrialAvailable,
		"purchase_available": me.Registration.PurchaseAvailable,
	}
	if me.Registration.TrialReason != nil {
		message["trial_reason"] = *me.Registration.TrialReason
	}
	_ = m.bridge.send(message)
}

// handleRegistration serves the two fixed S3-A host commands. "request_telegram_registration"
// fetches one fresh one-time deep link; "refresh_telegram_registration" forces a refresh and
// returns the latest server-owned status. Errors are fixed codes, never raw text.
func (m *managedMobile) handleRegistration(ctx context.Context, op string) {
	if m.client == nil {
		emitAccountAccessCode("REG_LINK_CLIENT_NIL")
		m.sendRegistrationError("MOBILE_STATE_UNAVAILABLE")
		return
	}
	if op == "activate_trial" {
		m.activateTrial(ctx)
		return
	}
	if op == "refresh_telegram_registration" {
		// Telegram confirmation does not upgrade an existing UNLINKED bearer.
		// Re-authenticate through the ordinary runner Ensure -> /me path.
		if m.session != nil {
			m.session.Refresh()
		}
		m.mu.Lock()
		baseline := m.verified
		me := m.me
		m.mu.Unlock()
		m.runner.Trigger("registration")
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				if me != nil {
					m.emitRegistrationStatus(m.bridge, *me)
				}
				return
			case <-m.verifiedSig:
				m.mu.Lock()
				current, verified := m.me, m.verified
				m.mu.Unlock()
				if verified > baseline && current != nil {
					m.emitRegistrationStatus(m.bridge, *current)
					return
				}
			}
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	emitAccountAccessCode("REG_LINK_BEGIN")
	link, apiError, err := m.client.RequestRegistrationLink(requestCtx)
	if err != nil {
		// Behavior-neutral, secret-free classification of the transport/service
		// failure. The original error is returned to the host unchanged below.
		if requestCtx.Err() != nil {
			emitAccountAccessCode("REG_LINK_CTX")
			emitAccountAccessCode(registrationErrorClass(err))
		} else {
			emitAccountAccessCode("REG_LINK_TRANSPORT")
			emitAccountAccessCode(registrationErrorClass(err))
		}
		m.sendRegistrationError("TRANSPORT")
		return
	}
	if apiError != nil {
		switch apiError.Code {
		case "REGISTRATION_DISABLED":
			emitAccountAccessCode("REG_LINK_API_DISABLED")
			m.sendRegistrationError("REGISTRATION_DISABLED")
		case "ACCESS_DENIED":
			emitAccountAccessCode("REG_LINK_API_DENIED")
			m.sendRegistrationError("ACCESS_DENIED")
		default:
			emitAccountAccessCode("REG_LINK_API_OTHER")
			m.sendRegistrationError("REGISTRATION_FAILED")
		}
		return
	}
	emitAccountAccessCode("REG_LINK_OK")
	if link.State == "registered" {
		emitAccountAccessCode("REG_LINK_REGISTERED")
		m.sendRegistrationError("REGISTRATION_ALREADY_DONE")
		return
	}
	if m.bridge != nil {
		emitAccountAccessCode("REG_LINK_PENDING")
		message := bridgeMessage{"type": "telegram_registration", "state": "pending",
			"deep_link": link.DeepLink}
		if link.ExpiresAt != nil {
			message["expires_at"] = *link.ExpiresAt
		}
		_ = m.bridge.send(message)
	}
}

// registrationErrorClass maps a registration transport failure to a fixed, secret-free
// diagnostic class emitted on the main accountaccess marker budget. It never changes the
// returned error or any behavior; unknown errors collapse to REG_LINK_OTHER.
func registrationErrorClass(err error) string {
	if err == nil {
		return "REG_LINK_OTHER"
	}
	var serviceErr *servicechannel.ServiceError
	if errors.As(err, &serviceErr) {
		switch serviceErr.Code {
		case "SERVICE_UNAVAILABLE":
			return "REG_LINK_SERVICE_UNAVAILABLE"
		case "SERVICE_PATH_DENIED":
			return "REG_LINK_SERVICE_PATH_DENIED"
		default:
			return "REG_LINK_SERVICE_OTHER"
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, servicechannel.ErrTransportTimeout):
		return "REG_LINK_TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "REG_LINK_CANCELED"
	case errors.Is(err, servicechannel.ErrBadResponse) || errors.Is(err, servicechannel.ErrResponseRejected):
		return "REG_LINK_REPLY_FAILED"
	case errors.Is(err, servicechannel.ErrTransportFailed):
		return "REG_LINK_TRANSPORT_FAILED"
	case errors.Is(err, servicechannel.ErrSeedMissing) || errors.Is(err, servicechannel.ErrSeedInvalid) ||
		errors.Is(err, servicechannel.ErrSeedSource) || errors.Is(err, servicechannel.ErrSeedEndpoint) ||
		errors.Is(err, servicechannel.ErrSeedStale) || errors.Is(err, servicechannel.ErrSeedBinding):
		return "REG_LINK_SEED"
	case errors.Is(err, servicechannel.ErrPathRejected):
		return "REG_LINK_PATH_REJECTED"
	case errors.Is(err, servicechannel.ErrRequestRejected):
		return "REG_LINK_REQUEST_REJECTED"
	case errors.Is(err, servicechannel.ErrOriginRejected):
		return "REG_LINK_ORIGIN_REJECTED"
	default:
		return "REG_LINK_OTHER"
	}
}

func (m *managedMobile) sendRegistrationError(code string) {
	if m.bridge != nil {
		_ = m.bridge.send(bridgeMessage{"type": "telegram_registration", "state": "error", "code": code})
	}
}

// activateTrial performs the one explicit 7-day trial activation. The server owns eligibility
// and replay; failures are fixed codes and never a client-side guess. A replay reports the
// original interval and can_activate=false so the UI shows used/expired and offers no repeat.
func (m *managedMobile) activateTrial(ctx context.Context) {
	if m.client == nil {
		m.sendTrialStatus(bridgeMessage{"type": "trial_status", "state": "error", "code": "MOBILE_STATE_UNAVAILABLE"})
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	activation, apiError, err := m.client.ActivateTrial(requestCtx)
	if err != nil {
		m.sendTrialStatus(bridgeMessage{"type": "trial_status", "state": "error", "code": "TRANSPORT"})
		return
	}
	if apiError != nil {
		code := "TRIAL_FAILED"
		switch apiError.Code {
		case "REGISTRATION_REQUIRED", "TRIAL_NOT_ELIGIBLE", "CHANNEL_MEMBERSHIP_REQUIRED",
			"SUBSCRIPTION_ACTIVE", "TRIAL_ALREADY_USED", "TRIAL_CHECK_UNAVAILABLE", "ACCESS_DENIED":
			code = apiError.Code
		}
		m.sendTrialStatus(bridgeMessage{"type": "trial_status", "state": "error", "code": code})
		return
	}
	message := bridgeMessage{"type": "trial_status", "state": "active",
		"can_activate": false, "replay": activation.Replay,
		"starts_at": activation.StartsAt, "ends_at": activation.EndsAt}
	m.sendTrialStatus(message)
	// The authoritative /me refresh follows through the runner; the explicit status above is
	// display-only for this tap.
	m.runner.Trigger("trial")
}

func (m *managedMobile) sendTrialStatus(message bridgeMessage) {
	if m.bridge != nil {
		_ = m.bridge.send(message)
	}
}

// readySnapshot reports whether a projectable verified pair already landed. It is
// the read side of the pre-admission wait loop, which must stay able to consume an
// explicit first-connect command while no catalogue exists yet.
func (m *managedMobile) readySnapshot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

// verifiedSignal wakes a waiter on every verified pair (projectable or not).
func (m *managedMobile) verifiedSignal() <-chan struct{} {
	return m.verifiedSig
}

// waitReady blocks until the first verified pair was projected into the store or the
// attempt context ends. A verified pair without a projectable right (for example
// data_access=none) is an expected state: the single runner keeps its refresh cadence
// and a later snapshot can still be admitted.
func (m *managedMobile) onCurrentMe(me accountaccess.MeResponse) {
	m.mu.Lock()
	// A new subject/right response must not leave the previous pair probe-usable.
	m.retireProbeAdmissionLocked()
	m.latestMe = &me
	m.mu.Unlock()
	// A confirmed different subject retires the dormant preference even if the
	// following metadata request fails. No rights are inferred from this check.
	if c := m.controller; c != nil {
		c.mu.Lock()
		if p := c.start.MobileSelection; p != nil && !m.selectionScopeMatches(p, me) {
			c.start.MobileSelection = nil
		}
		c.mu.Unlock()
	}
}

// This chooses the explicit Connect preparation path only. The refreshed
// credential catalog and normal admission checks still authorize VPN start.
func (m *managedMobile) hasCurrentDataAccess() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	me := m.latestMe
	if me == nil {
		me = m.me
	}
	return me != nil && (me.GrantResolution.DataAccess == "subscription_data" || me.GrantResolution.DataAccess == "onboarding_hour")
}

func (m *managedMobile) waitReady(ctx context.Context) error {
	for {
		m.mu.Lock()
		ready := m.ready
		m.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.verifiedSig:
		}
	}
}

// synchronize is the mobile implementation of the managed refresh seam: it asks the
// single runner for a cycle and waits until that cycle produced a verified snapshot.
func (m *managedMobile) synchronize(ctx context.Context, refresh bool) error {
	m.mu.Lock()
	baseline := m.verified
	m.mu.Unlock()
	if refresh {
		m.runner.Trigger("manual")
	}
	for {
		m.mu.Lock()
		if m.verified > baseline {
			err := m.verifiedErr
			m.mu.Unlock()
			return err
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.verifiedSig:
		}
	}
}

// onVerified projects one verified /me + /gateways pair into the technical store. A
// removed selected gateway is stopped and cleared explicitly; a projection dependency
// error never admits and never falls back to the legacy path.
func (m *managedMobile) onVerified(me accountaccess.MeResponse, catalog accountaccess.CatalogResponse) {
	m.mu.Lock()
	m.retireProbeAdmissionLocked()
	probeSerial := m.probeSerial
	probeCandidate := m.probeCandidateLocked(me, catalog)
	m.mu.Unlock()
	publishCtx := m.catalogPublicationContext()
	if err := publishCtx.Err(); err != nil {
		m.mu.Lock()
		m.verifiedErr = err
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.me = &me
	m.catalog = &catalog
	m.verified++
	sig := m.verifiedSig
	m.mu.Unlock()

	selection := m.controller.selectionID()
	if selection != "" && !catalogHasGateway(catalog, selection) {
		decision := accountaccess.DecideAdmission(me, catalog, selection, m.now())
		// This callback already retired the old pair before capturing probeSerial.
		// Removing its selected node stops that runtime, not admission of the new
		// pair's remaining nodes. Never recapture/reset the token: an external
		// retirement must still prevent this candidate from being accepted.
		m.handleAdmissionWithProbeRetirement(decision, decision.Reason != "SELECTED_NODE_REMOVED")
		m.controller.clearSelection()
		m.releasePending()
	}

	preferred := m.takeSelectionPreference(me, catalog)
	proofNode := m.proofNode()
	if proofNode == "" {
		proofNode = preferred
	} else {
		preferred = ""
	}
	err := m.applyVerified(&me, &catalog, proofNode)
	if err == nil && preferred != "" {
		// The fresh projection/store commit succeeded for this exact admitted ID.
		// This is not choose/select/Connect and starts no transport or probe.
		m.controller.mu.Lock()
		if m.controller.saved.SelectedNodeID == "" {
			m.controller.saved.SelectedNodeID = preferred
		}
		m.controller.mu.Unlock()
	}
	if err == nil {
		// Only the successfully projected/store-committed pair may authorize a probe.
		m.acceptProbeAdmission(probeSerial, probeCandidate)
	}
	if err == nil {
		// Publish every verified snapshot, including an unchanged selection. The
		// attempt context fences cancellation; this does not change the VPN phase.
		err = m.controller.publishCatalogSnapshotContext(publishCtx)
		if err == nil {
			err = publishCtx.Err()
		}
	}

	m.mu.Lock()
	m.verifiedErr = err
	if err == nil {
		m.ready = true
	} else {
		// Verified rights may arrive later; a failed projection is never terminal.
		if m.probeSerial == probeSerial {
			m.retireProbeAdmissionLocked()
		}
		m.projectionErr = err
	}
	m.mu.Unlock()
	select {
	case sig <- struct{}{}:
	default:
	}
}

// onBrowse publishes display-only metadata without admitting a saved preference.
// The credential store, lastGood and explicit pairing are not replaced by browse;
// no credential revision/selection is announced. A
// repeated identical list is emitted once; a renewed validity re-emits the list.
func (m *managedMobile) onBrowse(browse accountaccess.BrowseCatalogResponse) {
	m.retireProbeAdmission()
	publishCtx := m.catalogPublicationContext()
	if publishCtx.Err() != nil {
		return
	}
	// Startup intentionally requests browse first. Only an actual membership or
	// subject mismatch clears the preference before the later credential pair.
	m.reconcileBrowseSelection(browse)
	message := browseCatalogMessage(browse)
	raw, err := json.Marshal(message["nodes"])
	if err != nil {
		return
	}
	m.mu.Lock()
	// Signal the callback fact before the display dedup: an identical browse must
	// still complete a post-start wait.
	m.browseGen++
	browseSig := m.browseSig
	dedup := m.browseNodes == string(raw) && m.browseExpires == browse.ValidUntil
	m.browseNodes = string(raw)
	m.browseExpires = browse.ValidUntil
	copied := browse
	m.browse = &copied
	bridge := m.bridge
	m.mu.Unlock()
	if browseSig != nil {
		select {
		case browseSig <- struct{}{}:
		default:
		}
	}
	if dedup && catalogCycleFromContext(publishCtx) == "" {
		return
	}
	if bridge != nil {
		_ = bridge.sendContext(publishCtx, message)
	}
}

// browseCatalogMessage is the host-visible display projection of one browse catalog:
// the existing `catalog` event with catalog_mode="browse" and metadata-only nodes
// (node_id/name/country_code/region). It never carries revision, selected_node_id,
// subscription fields or any access/transport/probe value.
func browseCatalogMessage(browse accountaccess.BrowseCatalogResponse) bridgeMessage {
	nodes := make([]bridgeMessage, 0, len(browse.Gateways))
	for _, gateway := range browse.Gateways {
		node := bridgeMessage{"node_id": gateway.GatewayID, "name": gateway.Name}
		if gateway.Region != nil {
			node["region"] = *gateway.Region
		}
		country := ""
		if gateway.CountryCode != nil {
			country = *gateway.CountryCode
		}
		node["country_code"] = country
		nodes = append(nodes, node)
	}
	return bridgeMessage{"type": "catalog", "catalog_mode": "browse", "nodes": nodes,
		"issued_at": browse.IssuedAt, "catalog_expires_at": browse.ValidUntil}
}

// applyVerified projects for one proof node and atomically replaces the live snapshot.
func (m *managedMobile) applyVerified(me *accountaccess.MeResponse, catalog *accountaccess.CatalogResponse, proofNode string) error {
	projected, err := projectMobileCatalog(*me, *catalog, m.fingerprint, proofNode, m.now())
	if err != nil {
		return err
	}
	return m.controller.store.Apply(projected, "", m.fingerprint, m.now())
}

// bindSelection is the start gate: the selected gateway must carry an admitted
// decision, and only then is the store re-bound to its proof.
func (m *managedMobile) bindSelection(id string) error {
	if id == "" {
		return errors.New("SELECTED_NODE_REMOVED")
	}
	decision, err := m.decide(id)
	if err != nil {
		return err
	}
	if !decision.Admitted {
		return errors.New(decision.Reason)
	}
	m.mu.Lock()
	me, catalog := m.me, m.catalog
	m.mu.Unlock()
	if me == nil || catalog == nil {
		return errors.New("MOBILE_STATE_UNAVAILABLE")
	}
	m.mu.Lock()
	m.pending = id
	m.mu.Unlock()
	if err := m.applyVerified(me, catalog, id); err != nil {
		m.releasePending()
		return err
	}
	m.handleAdmission(decision)
	return nil
}

// decide evaluates the current verified pair for one gateway without any side effect.
func (m *managedMobile) decide(id string) (accountaccess.AdmissionDecision, error) {
	m.mu.Lock()
	me, catalog := m.me, m.catalog
	m.mu.Unlock()
	if me == nil || catalog == nil {
		return accountaccess.AdmissionDecision{}, errors.New("MOBILE_STATE_UNAVAILABLE")
	}
	return accountaccess.DecideAdmission(*me, *catalog, id, m.now()), nil
}

// prepareSwitchTarget enforces target admission before any switch and re-binds the store
// proof to the target. A refused/expired target is never started and never silently
// falls back; the active selection keeps its proof until a successful bind.
func (m *managedMobile) prepareSwitchTarget(ctx context.Context, target wlbs.Node, refreshTarget func(context.Context, string) error) (wlbs.Node, error) {
	if !target.Access.ValidAt(m.now()) {
		if refreshTarget == nil {
			return wlbs.Node{}, errors.New("BAD_CATALOG")
		}
		if err := refreshTarget(ctx, target.NodeID); err != nil {
			return wlbs.Node{}, err
		}
		if err := ctx.Err(); err != nil {
			return wlbs.Node{}, err
		}
		refreshed, err := managedNodeByID(m.controller.store.Snapshot(), target.NodeID)
		if err != nil {
			return wlbs.Node{}, err
		}
		target = refreshed
	}
	if err := m.bindSelection(target.NodeID); err != nil {
		return wlbs.Node{}, err
	}
	return target, nil
}

// handleAdmission is the single stop/timer handler for both the runner callback and the
// synchronous selection gate. StopDataPlane stops the real child; an admitted decision
// arms the proven-deadline timer so the stop does not depend on a wake or an HTTP reply.
func (m *managedMobile) handleAdmission(decision accountaccess.AdmissionDecision) {
	m.handleAdmissionWithProbeRetirement(decision, true)
}

func (m *managedMobile) handleAdmissionWithProbeRetirement(decision accountaccess.AdmissionDecision, retireProbe bool) {
	m.mu.Lock()
	copied := decision
	m.decision = &copied
	if decision.StopDataPlane {
		if retireProbe {
			m.retireProbeAdmissionLocked()
		}
		m.stopTimerLocked()
	}
	deadline := time.Time{}
	if decision.Admitted {
		nodeID := ""
		if decision.Grant != nil {
			nodeID = decision.Grant.NodeID
		}
		deadline = m.provenDeadlineLocked(nodeID)
	}
	m.mu.Unlock()
	if decision.StopDataPlane {
		m.controller.stopMobileDataPlane()
		return
	}
	if decision.Admitted && !deadline.IsZero() {
		m.armDeadline(deadline)
	}
}

// provenDeadlineLocked is the finite stop bound of the admitted right: the minimum over
// every deadline the verified pair actually proves. A nil entitlement deadline (allowed
// only for the indefinite commercial right) is skipped, while the catalog validity and
// the admitted node lease always participate, so the result is never unbounded. Callers
// hold m.mu.
func (m *managedMobile) provenDeadlineLocked(nodeID string) time.Time {
	var earliest time.Time
	if m.me != nil && m.me.GrantResolution.EffectiveDeadline != nil {
		if parsed, err := wlbs.UTC(*m.me.GrantResolution.EffectiveDeadline); err == nil {
			earliest = parsed
		}
	}
	if m.catalog != nil {
		if parsed, err := wlbs.UTC(m.catalog.ValidUntil); err == nil {
			if earliest.IsZero() || parsed.Before(earliest) {
				earliest = parsed
			}
		}
		for _, gateway := range m.catalog.Gateways {
			if gateway.GatewayID != nodeID {
				continue
			}
			if parsed, err := wlbs.UTC(gateway.Access.NotAfter); err == nil {
				if earliest.IsZero() || parsed.Before(earliest) {
					earliest = parsed
				}
			}
			break
		}
	}
	return earliest
}

// mobileDeadlineNow and newDeadlineTimer are the clock and scheduler seams of the
// proven-deadline timer. Production keeps the real time.Now/time.AfterFunc behavior;
// focused tests replace both to drive the stop deterministically.
var (
	mobileDeadlineNow = time.Now
	newDeadlineTimer  = time.AfterFunc
)

func (m *managedMobile) armDeadline(deadline time.Time) {
	delay := deadline.Sub(mobileDeadlineNow())
	if delay < 0 {
		delay = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopTimerLocked()
	m.timer = newDeadlineTimer(delay, func() {
		m.retireProbeAdmission()
		m.controller.stopMobileDataPlane()
	})
}

func (m *managedMobile) stopTimer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopTimerLocked()
}

func (m *managedMobile) stopTimerLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}

// proofNode is the node the live store snapshot is bound to: an in-flight switch target
// keeps its proof across refreshes until the switch commits or rolls back.
func (m *managedMobile) proofNode() string {
	m.mu.Lock()
	pending := m.pending
	m.mu.Unlock()
	if pending != "" {
		return pending
	}
	return m.controller.selectionID()
}

func (m *managedMobile) releasePending() {
	m.mu.Lock()
	m.pending = ""
	m.mu.Unlock()
}

// restoreSelection re-binds the store to the committed native selection after a failed
// switch, so the existing rollback fence sees the still-running node as eligible.
func (m *managedMobile) restoreSelection() {
	m.releasePending()
	m.mu.Lock()
	me, catalog := m.me, m.catalog
	m.mu.Unlock()
	selection := m.controller.selectionID()
	if selection == "" || me == nil || catalog == nil {
		return
	}
	_ = m.applyVerified(me, catalog, selection)
}

func (m *managedMobile) now() time.Time { return time.Now().UTC() }

// lastProjectionError exposes the most recent verified-pair projection failure for
// focused tests; it never terminates the mobile lifecycle by itself.
func (m *managedMobile) lastProjectionError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.projectionErr
}

// lastDecision exposes the last evaluated decision for focused tests.
func (m *managedMobile) lastDecision() *accountaccess.AdmissionDecision {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.decision == nil {
		return nil
	}
	copied := *m.decision
	return &copied
}

func catalogHasGateway(catalog accountaccess.CatalogResponse, id string) bool {
	if id == "" {
		return false
	}
	for _, gateway := range catalog.Gateways {
		if gateway.GatewayID == id {
			return true
		}
	}
	return false
}

// projectMobileCatalog maps the verified mobile pair into the technical wlbs container.
// Only technical fields and mobile proof are set; no legacy subscription/link business
// value is copied or synthesized. lease_seq and target_workers are required by the
// accepted contract and surface as explicit dependency errors when absent.
func projectMobileCatalog(me accountaccess.MeResponse, catalog accountaccess.CatalogResponse, fingerprint, proofNode string, now time.Time) (*wlbs.Catalog, error) {
	if len(catalog.Gateways) == 0 {
		return nil, errors.New("MOBILE_CATALOG_EMPTY")
	}
	validUntil, err := wlbs.UTC(catalog.ValidUntil)
	if err != nil {
		return nil, errors.New("CATALOG_INVALID")
	}
	// The proof deadline is always finite. A nil effective_deadline is accepted only
	// for the indefinite commercial entitlement; its bound then comes from the verified
	// pair itself (catalog validity, and any node lease that outlives it), never from an
	// invented date and never unbounded.
	indefinite := false
	entitlementDeadline := time.Time{}
	if me.GrantResolution.EffectiveDeadline != nil {
		parsed, err := wlbs.UTC(*me.GrantResolution.EffectiveDeadline)
		if err != nil {
			return nil, errors.New("RIGHT_DEADLINE_MISSING")
		}
		entitlementDeadline = parsed
	} else if accountaccess.IndefiniteSubscriptionData(me) {
		indefinite = true
		entitlementDeadline = validUntil
	} else {
		return nil, errors.New("RIGHT_DEADLINE_MISSING")
	}
	if proofNode == "" || !catalogHasGateway(catalog, proofNode) {
		proofNode = catalog.Gateways[0].GatewayID
	}
	nodes := make([]wlbs.Node, 0, len(catalog.Gateways))
	var proof wlbs.MobileCatalogProof
	for _, gateway := range catalog.Gateways {
		if gateway.TargetWorkers == nil || *gateway.TargetWorkers < 1 {
			return nil, errors.New("TARGET_WORKERS_MISSING")
		}
		if gateway.Access.LeaseSeq == "" {
			return nil, errors.New("LEASE_SEQ_MISSING")
		}
		notAfter, err := wlbs.UTC(gateway.Access.NotAfter)
		if err != nil {
			return nil, errors.New("GRANT_INVALID")
		}
		if indefinite && notAfter.After(entitlementDeadline) {
			// An indefinite right takes the outer bound of the whole verified node set,
			// so the finite proof stays an upper fence for every lease. The real stop
			// timer still takes the minimum (catalog validity / selected lease).
			entitlementDeadline = notAfter
		}
		country := ""
		if gateway.CountryCode != nil {
			country = *gateway.CountryCode
		}
		node := wlbs.Node{
			Endpoint: wlbs.Endpoint{
				NodeID:         gateway.GatewayID,
				PeerIP:         gateway.Transport.PeerIP,
				DTLSPort:       gateway.Transport.DTLSPort,
				DTLSSPKISHA256: gateway.Transport.DTLSSPKISHA256,
			},
			Name:        gateway.Name,
			CountryCode: country,
			WGPort:      gateway.Transport.WGPort,
			Protocol:    "wdtt-v17",
			AuthMode:    "installation-pop-v1",
			MaxWorkers:  *gateway.TargetWorkers,
			Access: wlbs.Access{
				GrantID:    gateway.Access.GrantID,
				DeviceID:   fingerprint,
				Password:   gateway.Access.Password,
				VKHashes:   append([]string(nil), gateway.Access.VKHashes...),
				Generation: gateway.Access.Generation,
				LeaseSeq:   gateway.Access.LeaseSeq,
				ExpiresAt:  gateway.Access.NotAfter,
			},
		}
		nodes = append(nodes, node)
		if gateway.GatewayID == proofNode {
			proof = wlbs.MobileCatalogProof{
				NodeID:              gateway.GatewayID,
				Subject:             fingerprint,
				CatalogRevision:     catalog.Revision,
				GrantID:             gateway.Access.GrantID,
				Generation:          gateway.Access.Generation,
				LeaseSeq:            gateway.Access.LeaseSeq,
				NotAfter:            notAfter,
				CatalogValidUntil:   validUntil,
				EntitlementDeadline: entitlementDeadline,
				TargetWorkers:       *gateway.TargetWorkers,
				VerifiedAt:          now,
			}
		}
	}
	proof.EntitlementDeadline = entitlementDeadline
	subscriptionExpiresAt := ""
	if me.Entitlement.ValidUntil != nil {
		subscriptionExpiresAt = *me.Entitlement.ValidUntil
	}
	projected := &wlbs.Catalog{
		V:                     2,
		Status:                "ok",
		SubscriptionRef:       mobileSubscriptionRef(fingerprint),
		RegistrationID:        fingerprint,
		SubscriptionStatus:    me.Entitlement.Status,
		SubscriptionExpiresAt: subscriptionExpiresAt,
		SlotsLimit:            me.Entitlement.EffectiveDeviceLimit,
		SlotsUsed:             me.Entitlement.SlotsUsed,
		Revision:              catalog.Revision,
		IssuedAt:              catalog.IssuedAt,
		RefreshAfter:          catalog.IssuedAt,
		CatalogExpiresAt:      catalog.ValidUntil,
		ServerTime:            catalog.ServerTime,
		Nodes:                 nodes,
	}
	return projected.WithMobileProof(proof), nil
}
