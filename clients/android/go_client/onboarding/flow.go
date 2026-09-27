package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"wg-turn-client/accountaccess"
)

// Flow-level errors. They are fixed, secret-free and distinct from the backend codes.
var (
	ErrInFlight         = errors.New("ONBOARDING_CONNECT_IN_FLIGHT")
	ErrCancelled        = errors.New("ONBOARDING_CONNECT_CANCELLED")
	ErrPendingBudget    = errors.New("ONBOARDING_PENDING_BUDGET")
	ErrStartUnavailable = errors.New("ONBOARDING_START_UNAVAILABLE")
	ErrStartNotReady    = errors.New("ONBOARDING_START_NOT_READY")
	ErrCorrelation      = errors.New("ONBOARDING_CORRELATION_MISMATCH")
)

// KeyState is the durable identity of one onboarding attempt. It survives cancel
// and repeated taps so the same request_key keeps returning the same intent, and it
// carries the immutable selected-gateway pairing sent with every intent request of
// that attempt.
type KeyState struct {
	RequestKey   string `json:"request_key"`
	IntentID     string `json:"intent_id"`
	CredentialID string `json:"credential_id"`
	StartedAt    string `json:"started_at"`
	NotAfter     string `json:"not_after"`
	Terminal     string `json:"terminal"`
	// GatewayKey is the selected gateway id persisted with the request_key; empty
	// means no selection (the server assigns as before).
	GatewayKey string `json:"gateway_key,omitempty"`
}

// Store persists the attempt identity. The production wiring reuses the existing
// persist-namespace pattern; tests use MemoryStore.
type Store interface {
	Load() (KeyState, bool)
	Save(KeyState) error
}

// MemoryStore is a bounded in-process Store.
type MemoryStore struct {
	mu    sync.Mutex
	state KeyState
	set   bool
}

// Load returns the stored state, if any.
func (m *MemoryStore) Load() (KeyState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.set
}

// Save replaces the stored state.
func (m *MemoryStore) Save(state KeyState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.set = state, true
	return nil
}

// IntentClient performs one intent operation (create or retry-poll) over the
// accepted accountaccess.Doer seam. A retry reuses the same request_key with a
// fresh challenge/proof and must return the same intent state. gatewayKey is the
// durable selected-gateway pairing of that request_key; it is sent with every
// intent POST and poll and stays empty for legacy callers without a selection.
type IntentClient interface {
	Intent(ctx context.Context, requestKey, gatewayKey string) (IntentPoll, *APIError, error)
}

// Starter performs the explicit signed onboarding.start RPC after the caller has
// established the trusted assigned-gateway bootstrap. It is the only interface
// whose implementation may start the hour; Flow never invokes it implicitly.
type Starter interface {
	Start(ctx context.Context, ready IntentPoll) (StartReply, *APIError, error)
}

// Options bounds pending polling inside the caller's existing context budget.
type Options struct {
	// MaxPendingPolls is the consecutive pending poll budget (default 20).
	MaxPendingPolls int
	// RetryAfterCap clamps a server retry_after (default 30s).
	RetryAfterCap time.Duration
	// Sleep is injectable for tests (default time.Sleep with context).
	Sleep func(ctx context.Context, d time.Duration) error
}

func (o Options) withDefaults() Options {
	if o.MaxPendingPolls <= 0 {
		o.MaxPendingPolls = 20
	}
	if o.RetryAfterCap <= 0 {
		o.RetryAfterCap = 30 * time.Second
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	return o
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Result is the outcome of one explicit Connect or Recover call.
type Result struct {
	State State
	Poll  *IntentPoll
	// Recovered marks a `started` observation: the hour is recorded server-side
	// and the old bootstrap credential/secret is never needed again.
	Recovered bool
}

// Flow is the pre-admission explicit Connect state machine. Only Start can
// start the hour; Connect/Recover only read and poll. Nothing here runs on a
// background timer: every call is owned by the caller's explicit context.
type Flow struct {
	mu      sync.Mutex
	store   Store
	intents IntentClient
	starter Starter
	opts    Options
	busy    bool
	gen     uint64
}

// NewFlow wires the durable identity store and the intent client. starter may be
// nil until the trusted bootstrap transport exists; Start then fails closed.
func NewFlow(store Store, intents IntentClient, starter Starter, opts Options) (*Flow, error) {
	if store == nil || intents == nil {
		return nil, errors.New("ONBOARDING_FLOW_CONFIG")
	}
	return &Flow{store: store, intents: intents, starter: starter, opts: opts.withDefaults()}, nil
}

// Cancel aborts the in-flight explicit call idempotently and keeps the durable
// request_key/intent_id so the next explicit Connect continues the same intent.
func (f *Flow) Cancel() {
	f.mu.Lock()
	f.gen++
	f.mu.Unlock()
}

// State reports the durable state without any network call.
func (f *Flow) State() State {
	state, _ := f.store.Load()
	return stateOf(state)
}

func stateOf(state KeyState) State {
	switch {
	case state.Terminal != "":
		return State(state.Terminal)
	case state.StartedAt != "":
		return StateStarted
	case state.IntentID != "" || state.RequestKey != "":
		return StatePending
	default:
		return StateNone
	}
}

// Connect is the explicit user Connect entry for callers without a gateway selection:
// it creates (or reuses) the intent identity, then polls bounded pending/ready. It
// never calls the start RPC.
func (f *Flow) Connect(ctx context.Context) (Result, error) {
	return f.connect(ctx, "")
}

// ConnectWithGateway is the explicit Connect entry with the optional selected gateway
// binding. The pairing is persisted with the request_key and reused on every retry; a
// different gateway for an unfinished intent is ErrIntentConflict (never a silent
// switch), while the accepted expired/unstarted recovery creates a new request_key.
func (f *Flow) ConnectWithGateway(ctx context.Context, gatewayKey string) (Result, error) {
	return f.connect(ctx, gatewayKey)
}

func (f *Flow) connect(ctx context.Context, gatewayKey string) (Result, error) {
	f.mu.Lock()
	if f.busy {
		f.mu.Unlock()
		return Result{State: f.State()}, ErrInFlight
	}
	f.busy = true
	generation := f.gen
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.busy = false
		f.mu.Unlock()
	}()

	state, _ := f.store.Load()
	if state.Terminal != "" {
		// A NEW explicit user Connect (this is the explicit entry; background, recover
		// and resume paths never reach it) may supersede a server-expired attempt that
		// never started its hour. Every other terminal kind and any started/hour
		// identity stay fail-closed; an expiry discovered during this call never
		// restarts within the same call.
		if State(state.Terminal) != StateExpired || state.StartedAt != "" {
			return Result{State: State(state.Terminal)}, nil
		}
		// Drop the dead attempt identity BEFORE any side effect (including dropping
		// its gateway pairing), so the recovery can never resurrect the expired
		// request_key with another gateway; the installation/environment binding
		// lives in the durable envelope and is preserved. The new attempt below gets
		// its own request_key and its own gateway_key.
		state = KeyState{}
	}
	if state.StartedAt != "" {
		return Result{State: StateStarted, Recovered: true}, nil
	}
	if state.RequestKey != "" {
		// Unfinished intent: the durable request_key+gateway_key pairing is immutable.
		// A specified different key is a conflict; the stored pairing is kept until
		// the outcome. An empty incoming key reuses the stored pairing as-is.
		if gatewayKey != "" && gatewayKey != state.GatewayKey {
			return Result{State: stateOf(state)}, ErrIntentConflict
		}
	} else {
		state.GatewayKey = gatewayKey
	}
	if state.RequestKey == "" {
		requestKey, err := newRequestKey()
		if err != nil {
			return Result{State: StateNone}, err
		}
		state.RequestKey = requestKey
		if err := f.store.Save(state); err != nil {
			return Result{State: StateNone}, err
		}
	}

	var last *IntentPoll
	for polls := 0; polls < f.opts.MaxPendingPolls; polls++ {
		if err := ctx.Err(); err != nil {
			return resultFrom(last), err
		}
		if f.cancelled(generation) {
			return resultFrom(last), ErrCancelled
		}
		poll, apiError, err := f.intents.Intent(ctx, state.RequestKey, state.GatewayKey)
		if err != nil {
			return resultFrom(last), err
		}
		if apiError != nil {
			return f.applyAPIError(&state, apiError)
		}
		last = &poll
		state, err = f.correlate(state, poll)
		if err != nil {
			return resultFrom(last), err
		}
		if err := checkReturnedGateway(state, poll); err != nil {
			return resultFrom(last), err
		}
		switch poll.State {
		case StatePending:
			if f.cancelled(generation) {
				return resultFrom(last), ErrCancelled
			}
			if polls+1 >= f.opts.MaxPendingPolls {
				return resultFrom(last), ErrPendingBudget
			}
			wait := time.Duration(poll.RetryAfter) * time.Second
			if wait > f.opts.RetryAfterCap {
				wait = f.opts.RetryAfterCap
			}
			if err := f.opts.Sleep(ctx, wait); err != nil {
				return resultFrom(last), err
			}
		case StateReady:
			if err := f.store.Save(state); err != nil {
				return resultFrom(last), err
			}
			return Result{State: StateReady, Poll: last}, nil
		case StateStarted:
			// Public started is recovery only: record it and return without the
			// bootstrap secret; the normal session refresh path continues.
			state.StartedAt = poll.StartedAt.UTC().Format(time.RFC3339)
			state.NotAfter = poll.NotAfter.UTC().Format(time.RFC3339)
			if poll.CredentialID != "" {
				state.CredentialID = poll.CredentialID
			}
			if err := f.store.Save(state); err != nil {
				return resultFrom(last), err
			}
			return Result{State: StateStarted, Poll: last, Recovered: true}, nil
		case StateFailed:
			state.Terminal = string(StateFailed)
			if err := f.store.Save(state); err != nil {
				return resultFrom(last), err
			}
			return Result{State: StateFailed, Poll: last}, nil
		default:
			return resultFrom(last), errors.New("ONBOARDING_INTENT_MALFORMED")
		}
	}
	return resultFrom(last), ErrPendingBudget
}

// Recover is the public `started` recovery path: it never creates an intent and
// never calls the start RPC. Without a stored intent it reports none.
func (f *Flow) Recover(ctx context.Context) (Result, error) {
	f.mu.Lock()
	if f.busy {
		f.mu.Unlock()
		return Result{State: f.State()}, ErrInFlight
	}
	state, present := f.store.Load()
	if !present || (state.IntentID == "" && state.RequestKey == "") || state.Terminal != "" {
		f.mu.Unlock()
		if state.Terminal != "" {
			return Result{State: State(state.Terminal)}, nil
		}
		return Result{State: StateNone}, nil
	}
	f.busy = true
	generation := f.gen
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.busy = false
		f.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return Result{State: stateOf(state)}, err
	}
	if f.cancelled(generation) {
		return Result{State: stateOf(state)}, ErrCancelled
	}
	poll, apiError, err := f.intents.Intent(ctx, state.RequestKey, state.GatewayKey)
	if err != nil {
		return Result{State: stateOf(state)}, err
	}
	if apiError != nil {
		return f.applyAPIError(&state, apiError)
	}
	state, err = f.correlate(state, poll)
	if err != nil {
		return Result{State: stateOf(state)}, err
	}
	if err := checkReturnedGateway(state, poll); err != nil {
		return Result{State: stateOf(state)}, err
	}
	switch poll.State {
	case StateReady:
		if err := f.store.Save(state); err != nil {
			return Result{}, err
		}
		return Result{State: StateReady, Poll: &poll}, nil
	case StateStarted:
		state.StartedAt = poll.StartedAt.UTC().Format(time.RFC3339)
		state.NotAfter = poll.NotAfter.UTC().Format(time.RFC3339)
		if poll.CredentialID != "" {
			state.CredentialID = poll.CredentialID
		}
		if err := f.store.Save(state); err != nil {
			return Result{}, err
		}
		return Result{State: StateStarted, Poll: &poll, Recovered: true}, nil
	case StatePending:
		return Result{State: StatePending, Poll: &poll}, nil
	default:
		return Result{State: State(state.Terminal)}, nil
	}
}

// Start performs the explicit signed onboarding.start RPC. It requires a ready
// poll from the same attempt and a configured Starter; retrying the exact same
// signed request is contract-safe (historical replay). It is never called by
// Connect/Recover and never starts anything on a background path.
func (f *Flow) Start(ctx context.Context, ready IntentPoll) (StartReply, error) {
	if ready.State != StateReady || ready.StartChallenge == nil {
		return StartReply{}, ErrStartNotReady
	}
	if f.starter == nil {
		return StartReply{}, ErrStartUnavailable
	}
	f.mu.Lock()
	if f.busy {
		f.mu.Unlock()
		return StartReply{}, ErrInFlight
	}
	f.busy = true
	generation := f.gen
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.busy = false
		f.mu.Unlock()
	}()

	state, _ := f.store.Load()
	if state.Terminal != "" {
		return StartReply{}, errors.New(state.Terminal)
	}
	if state.IntentID == "" || state.RequestKey == "" {
		return StartReply{}, ErrStartNotReady
	}
	if ready.IntentID != state.IntentID || ready.RequestKey != state.RequestKey {
		return StartReply{}, ErrCorrelation
	}
	// The returned intent gateway binding must match the durable pairing before the
	// bootstrap/start: a mismatch is the immutable-binding conflict, never a start
	// against a foreign gateway. The signed start payload itself stays the accepted
	// legacy shape.
	if err := checkReturnedGateway(state, ready); err != nil {
		return StartReply{}, err
	}
	if f.cancelled(generation) {
		return StartReply{}, ErrCancelled
	}
	reply, apiError, err := f.starter.Start(ctx, ready)
	if err != nil {
		return StartReply{}, err
	}
	if apiError != nil {
		return StartReply{}, apiError
	}
	if reply.IntentID != state.IntentID {
		return StartReply{}, ErrCorrelation
	}
	state.StartedAt = reply.StartedAt.UTC().Format(time.RFC3339)
	state.NotAfter = reply.NotAfter.UTC().Format(time.RFC3339)
	state.CredentialID = reply.CredentialID
	return reply, f.store.Save(state)
}

func (f *Flow) applyAPIError(state *KeyState, apiError *APIError) (Result, error) {
	switch {
	case errors.Is(apiError, ErrIntentExpired):
		state.Terminal = string(StateExpired)
	case errors.Is(apiError, ErrIntentRevoked):
		state.Terminal = string(StateRevoked)
	}
	if state.Terminal == "" {
		return Result{State: stateOf(*state)}, apiError
	}
	if err := f.store.Save(*state); err != nil {
		return Result{}, err
	}
	return Result{State: State(state.Terminal)}, apiError
}

// checkReturnedGateway enforces the immutable selected-gateway pairing against the
// gateway binding returned by an intent answer. The intent poll/ready wire carries the
// assigned gateway as gateway.node_id, which is the same stable public key the request
// was bound with. A legacy caller without a durable pairing has nothing to compare and
// stays server-assigned. The check runs before any state transition, persisted side
// effect or bootstrap/start, so a mismatching answer can never start the hour or reach
// a foreign gateway.
func checkReturnedGateway(state KeyState, poll IntentPoll) error {
	if state.GatewayKey == "" || poll.Gateway == nil {
		return nil
	}
	if poll.Gateway.NodeID != state.GatewayKey {
		return ErrIntentConflict
	}
	return nil
}

func (f *Flow) correlate(state KeyState, poll IntentPoll) (KeyState, error) {
	if poll.IntentID == "" || poll.RequestKey != state.RequestKey {
		return state, ErrCorrelation
	}
	if state.IntentID != "" && state.IntentID != poll.IntentID {
		return state, ErrCorrelation
	}
	state.IntentID = poll.IntentID
	if poll.CredentialID != "" {
		state.CredentialID = poll.CredentialID
	}
	return state, nil
}

func (f *Flow) cancelled(generation uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen != generation
}

func resultFrom(poll *IntentPoll) Result {
	if poll == nil {
		return Result{State: StateNone}
	}
	return Result{State: poll.State, Poll: poll}
}

func newRequestKey() (string, error) {
	return accountaccess.NewRequestID()
}

// NewRequestKey returns a fresh bounded request_key for one explicit attempt.
func NewRequestKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
