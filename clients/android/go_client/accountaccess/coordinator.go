package accountaccess

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// State is the native refresh state machine: IDLE -> REFRESH_INFLIGHT ->
// (APPLIED | STALE | CONFLICT_BLOCKED | FAILED).
type State string

const (
	StateIdle     State = "IDLE"
	StateInflight State = "REFRESH_INFLIGHT"
	StateApplied  State = "APPLIED"
	StateStale    State = "STALE"
	StateConflict State = "CONFLICT_BLOCKED"
	StateFailed   State = "FAILED"
)

// ErrMalformedCatalog marks a 200 whose registered node set is invalid: the whole
// response is unusable and last-good is retained.
var ErrMalformedCatalog = errors.New("malformed catalog response")

// ErrNoBinding means /me carries no binding: no sync may be started, because a read
// never creates a slot or right.
var ErrNoBinding = errors.New("subject has no binding revision")

// APIStatusError carries a classified non-success contract response so the single
// owner can react to session lifecycle without string matching.
type APIStatusError struct {
	Code         string
	Retryable    bool
	RetryAfterMS *int
}

func (e *APIStatusError) Error() string { return "api " + e.Code }

func apiStatus(apiError *ErrorResponse) error {
	if apiError == nil {
		return nil
	}
	return &APIStatusError{Code: apiError.Code, Retryable: apiError.Retryable, RetryAfterMS: apiError.RetryAfterMS}
}

// ErrMeRequired means no /me snapshot is resolved yet: a new sync must be preceded by a
// successful Refresh. An uncertain persisted operation is resumed without /me because its
// durable body is already persisted.
var ErrMeRequired = errors.New("subject state not resolved: refresh /me before sync")

// ErrRecoveryRevisions means the durable observe=sync_recovery marker requires a rotation
// but carries no valid catalog/binding revision pair: no POST is made (neither with a new
// key nor with the stale persisted body) and the persisted receipt stays exactly as-is
// until a later poll supplies a usable marker.
var ErrRecoveryRevisions = errors.New("sync recovery marker has no usable revision pair")

// Subject is the stable authenticated subject that keys the revision/digest domain.
// It is native-only and never placed on the wire.
type Subject struct {
	AccountRef     string `json:"account_ref"`
	InstallationID string `json:"installation_id"`
}

type baseline struct {
	revision string
	digest   string
}

// AdmissionTokens are bounded GET /gateways nonterminal metadata. They are admission
// tokens for a NEW sync only; they are not a verified catalog.
type AdmissionTokens struct {
	CatalogRevision string
	BindingRevision string

	subject Subject
	tick    uint64
}

func (r Receipt) terminal() bool {
	return r.Response != nil &&
		(r.Response.AccessApplicationState == "applied" || r.Response.AccessApplicationState == "rejected")
}

// Receipt is the persisted operation identity: stable subject + canonical body + durable
// key. Response == nil means the operation may have been sent but its response was never
// durably observed (lost response / restart): it is resumed with the same key/body so the
// server-side idempotency yields exactly one effect.
type Receipt struct {
	Subject    Subject             `json:"subject"`
	Key        string              `json:"key"`
	BodyDigest string              `json:"body_digest"`
	Request    AccessSyncRequest   `json:"request"`
	Response   *AccessSyncResponse `json:"response,omitempty"`
	// RecoveryRequired is the durable observe=sync_recovery predicate: the failed intent
	// may be recovered by exactly one new key built from the marker revisions. It is
	// absent in old persisted receipts and is never set from a generic retryable, dead,
	// exhausted, timed-out or restarted state.
	RecoveryRequired bool `json:"recovery_required,omitempty"`
	// RecoveryRevisions is the marker-supplied revision pair, set together with
	// RecoveryRequired only when both values are contract revisions. A nil pair while
	// RecoveryRequired is true means the marker had no usable revisions: no POST may be
	// made (neither a new key nor a stale-body resend) until a later poll refreshes the
	// hint.
	RecoveryRevisions *AccessSyncRequest `json:"recovery_revisions,omitempty"`
	// RotationOf records the old operation id whose observe marker justified this
	// receipt's new key (provenance only, never placed on the wire).
	RotationOf string `json:"rotation_of,omitempty"`
}

// ReceiptStore persists the idempotency receipt across restarts.
type ReceiptStore interface {
	Load() (*Receipt, error)
	Save(Receipt) error
}

// MemoryReceiptStore is the in-memory store used in tests.
type MemoryReceiptStore struct{ Value *Receipt }

func (m *MemoryReceiptStore) Load() (*Receipt, error) { return m.Value, nil }
func (m *MemoryReceiptStore) Save(value Receipt) error {
	copy := value
	m.Value = &copy
	return nil
}

// Options wires the coordinator to its single transport and to the native-owned
// stable authenticated subject.
type Options struct {
	Client    *Client
	AttemptID string
	Store     ReceiptStore
	Subject   func() Subject
}

// Result of a /me refresh. Projection is non-nil only for an accepted, non-duplicate
// snapshot; failures and conflicts never emit.
type Result struct {
	State      State
	Projection *Projection
	Duplicate  bool
	Rejection  string
	Coalesced  bool
	Dropped    bool
}

// CatalogResult of a GET /gateways read. LastGood is always the retained catalog on
// any nonterminal or failed outcome. Browse is the display-only branch: it is never an
// admission snapshot, never touches last-good and never authorizes a sync.
type CatalogResult struct {
	State              State
	Catalog            *CatalogResponse
	Browse             *BrowseCatalogResponse
	Admission          *AdmissionTokens
	Applied            bool
	Duplicate          bool
	AuthoritativeEmpty bool
	Dropped            bool
}

// SyncResult of POST /access/sync.
type SyncResult struct {
	State    State
	Receipt  *Receipt
	Replayed bool
	Dropped  bool
}

// PollResult is the outcome of advancing the current operation receipt with GET
// /operations/{id}. Stale means a late poll of an already replaced operation/subject.
type PollResult struct {
	State    State
	Receipt  *Receipt
	Terminal bool
	Stale    bool
	Dropped  bool
}

// Coordinator is the single native owner of /me fetch, revision/digest resolution,
// catalog last-good admission, sync idempotency and projection emission.
type Coordinator struct {
	opts Options

	// syncMu single-flights SyncAccess: concurrent calls cannot read the same receipt
	// and each mint a separate key; the trailing call observes the already persisted
	// operation and resumes it instead.
	syncMu          sync.Mutex
	mu              sync.Mutex
	state           State
	inflight        bool
	pending         bool
	subject         Subject
	subjectSet      bool
	current         baseline
	hasBaseline     bool
	lastGood        *CatalogResponse
	lastGoodDigest  string
	admission       *AdmissionTokens
	generation      uint64
	previous        *uint64
	sessionTick     uint64
	lastEmittedTick uint64
	receipt         *Receipt
	lastMe          *MeResponse
	lastMeSubject   Subject
	lastMeTick      uint64
	catalogSubject  Subject
	catalogTick     uint64
}

// NewCoordinator creates the coordinator and loads any persisted receipt.
func NewCoordinator(opts Options) (*Coordinator, error) {
	if opts.Client == nil || opts.Subject == nil {
		return nil, fmt.Errorf("accountaccess: client and subject are required")
	}
	coordinator := &Coordinator{opts: opts, state: StateIdle}
	if opts.Store != nil {
		receipt, err := opts.Store.Load()
		if err != nil {
			return nil, err
		}
		coordinator.receipt = receipt
	}
	return coordinator, nil
}

// NewSession records a replaced native authenticated session: the next emission is a
// predecessor-linked replacement with a unique, never-reused generation.
func (c *Coordinator) NewSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceGenerationLocked()
}

// advanceGenerationLocked allocates a unique, never-reused generation with the previous
// generation as its linked predecessor and invalidates in-flight fetch callbacks.
func (c *Coordinator) advanceGenerationLocked() {
	if c.generation > 0 {
		previous := c.generation
		c.previous = &previous
		c.generation++
	}
	c.sessionTick++
}

// State returns the current refresh state.
func (c *Coordinator) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// LastGood returns the retained verified catalog, if any.
func (c *Coordinator) LastGood() *CatalogResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastGood
}

// AdmissionTokens returns the retained nonterminal admission metadata.
func (c *Coordinator) AdmissionTokens() *AdmissionTokens {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admission
}

// Refresh fetches /me with coalescing: a trigger during REFRESH_INFLIGHT sets one
// REFRESH_PENDING flag and yields exactly one trailing refresh.
func (c *Coordinator) Refresh(ctx context.Context) (Result, error) {
	for {
		c.mu.Lock()
		if c.inflight {
			c.pending = true
			state := c.state
			c.mu.Unlock()
			return Result{State: state, Coalesced: true}, nil
		}
		c.inflight = true
		c.state = StateInflight
		tick := c.sessionTick
		c.mu.Unlock()

		result, err := c.fetchOnce(ctx, tick)

		c.mu.Lock()
		c.inflight = false
		trailing := c.pending
		c.pending = false
		c.state = result.State
		c.mu.Unlock()

		if err != nil || !trailing {
			return result, err
		}
	}
}

func (c *Coordinator) fetchOnce(ctx context.Context, tick uint64) (Result, error) {
	// The request subject/session is fixed BEFORE the HTTP call; the response may only
	// be applied if it still belongs to that same fence.
	requestSubject := c.opts.Subject()
	me, apiError, err := c.opts.Client.GetMe(ctx)
	if err != nil {
		return Result{State: StateFailed}, err
	}
	if apiError != nil {
		state := StateFailed
		switch apiError.Code {
		case CodeSessionExpired, CodeSessionInvalid:
			state = StateFailed
		}
		return Result{State: state}, apiStatus(apiError)
	}
	projection := buildProjection(me, 0, nil)
	digest, err := projection.Digest()
	if err != nil {
		return Result{State: StateFailed}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if tick != c.sessionTick || requestSubject != c.opts.Subject() {
		// A replaced native session/subject discarded this callback before emission:
		// an old token's response is never marked with the new subject.
		return Result{State: StateStale, Dropped: true}, nil
	}
	subject := requestSubject
	if !c.subjectSet || subject != c.subject {
		// Actual subject replacement opens a fresh domain baseline and a linked new
		// session generation (unique, predecessor == previous generation); the first
		// revision of the new subject is the new baseline. Snapshot fences of the old
		// subject are invalidated so a sync cannot reuse foreign revisions.
		if c.subjectSet {
			c.advanceGenerationLocked()
		}
		c.lastMe = nil
		c.lastGood = nil
		c.lastGoodDigest = ""
		c.catalogSubject = Subject{}
		c.catalogTick = 0
		c.admission = nil
		c.subject = subject
		c.subjectSet = true
		c.current = baseline{revision: me.Revision, digest: digest}
		c.hasBaseline = true
		return c.accepted(me, projection)
	}
	if !c.hasBaseline {
		c.current = baseline{revision: me.Revision, digest: digest}
		c.hasBaseline = true
		return c.accepted(me, projection)
	}
	order, ok := CompareRevision(me.Revision, c.current.revision)
	if !ok {
		return Result{State: StateFailed}, fmt.Errorf("revision not comparable")
	}
	switch {
	case order < 0:
		return Result{State: StateStale, Rejection: "STALE"}, nil
	case order == 0 && digest == c.current.digest:
		if tick == c.lastEmittedTick {
			// Same domain revision with identical protected bytes in the same session:
			// no-op dedupe. The last-good anchor is retained and a newer server_time is
			// not adopted.
			return Result{State: StateApplied, Duplicate: true}, nil
		}
		// A replaced native authenticated session must emit its linked generation pair
		// even when the protected payload bytes are unchanged.
		return c.accepted(me, projection)
	case order == 0:
		return Result{State: StateConflict, Rejection: "REVISION_CONFLICT"}, nil
	default:
		c.current = baseline{revision: me.Revision, digest: digest}
		return c.accepted(me, projection)
	}
}

func (c *Coordinator) accepted(me MeResponse, projection Projection) (Result, error) {
	snapshot := me
	c.lastMe = &snapshot
	c.lastMeSubject = c.subject
	c.lastMeTick = c.sessionTick
	if c.generation == 0 {
		c.generation = 1
		c.previous = nil
	}
	projection.Generation = c.generation
	projection.Previous = c.previous
	c.lastEmittedTick = c.sessionTick
	return Result{State: StateApplied, Projection: &projection}, nil
}

// RefreshDisplayCatalog reads the explicit metadata projection under the current
// subject/session fence. It never changes last-good, admission tokens or receipts,
// including when the server returns an admission error or a credential catalog.
func (c *Coordinator) RefreshDisplayCatalog(ctx context.Context) (CatalogResult, error) {
	if err := ctx.Err(); err != nil {
		return CatalogResult{State: StateStale, Dropped: true}, err
	}
	c.mu.Lock()
	tick := c.sessionTick
	c.mu.Unlock()
	requestSubject := c.opts.Subject()
	gateways, apiError, err := c.opts.Client.GetDisplayGateways(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if canceled := ctx.Err(); canceled != nil {
		return CatalogResult{State: StateStale, Dropped: true}, canceled
	}
	if tick != c.sessionTick || !c.subjectSet || requestSubject != c.subject || requestSubject != c.opts.Subject() {
		return CatalogResult{State: StateStale, Dropped: true}, nil
	}
	if err != nil {
		return CatalogResult{State: StateFailed}, err
	}
	if apiError != nil {
		return CatalogResult{State: StateFailed}, apiStatus(apiError)
	}
	return CatalogResult{State: StateApplied, Browse: gateways.Browse}, nil
}

// RefreshCatalog reads GET /gateways. Only an authoritative 200 may replace or clear
// the last-good catalog; 409 pending and 503 failure keep it.
func (c *Coordinator) RefreshCatalog(ctx context.Context) (CatalogResult, error) {
	c.mu.Lock()
	tick := c.sessionTick
	c.mu.Unlock()
	requestSubject := c.opts.Subject()
	gateways, apiError, err := c.opts.Client.GetGateways(ctx)
	if err != nil {
		return CatalogResult{State: StateFailed}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if tick != c.sessionTick || !c.subjectSet || requestSubject != c.subject || requestSubject != c.opts.Subject() {
		// The catalog response belongs to another subject/session fence: it never
		// authorizes a snapshot, admission token or sync revision.
		return CatalogResult{State: StateStale, Catalog: c.lastGood, Dropped: true}, nil
	}
	if apiError != nil {
		switch apiError.Code {
		case CodeAccessSyncPending:
			if catalogRevision, bindingRevision, ok := apiError.AdmissionTokens(); ok {
				c.admission = &AdmissionTokens{CatalogRevision: catalogRevision, BindingRevision: bindingRevision,
					subject: c.subject, tick: c.sessionTick}
			}
			return CatalogResult{State: StateStale, Catalog: c.lastGood, Admission: c.admission}, nil
		case CodeServiceUnavailable:
			// Actual application failure or unknown registry is not a removal, and a
			// new sync is equally blocked; retained attempt budget is never reset here.
			if catalogRevision, bindingRevision, ok := apiError.AdmissionTokens(); ok {
				c.admission = &AdmissionTokens{CatalogRevision: catalogRevision, BindingRevision: bindingRevision,
					subject: c.subject, tick: c.sessionTick}
			}
			return CatalogResult{State: StateFailed, Catalog: c.lastGood, Admission: c.admission}, nil
		default:
			return CatalogResult{State: StateFailed, Catalog: c.lastGood}, nil
		}
	}
	if gateways.Browse != nil {
		// Display-only read: it never replaces or clears last-good, never creates
		// admission tokens and never authorizes a sync or a data plane start.
		return CatalogResult{State: StateApplied, Browse: gateways.Browse}, nil
	}
	if gateways.Catalog == nil {
		return CatalogResult{State: StateFailed, Catalog: c.lastGood}, fmt.Errorf("gateways union empty")
	}
	catalog := *gateways.Catalog
	digest, err := catalog.DigestedCatalog()
	if err != nil {
		return CatalogResult{State: StateFailed, Catalog: c.lastGood}, err
	}
	if c.lastGood == nil {
		c.applyCatalog(catalog, digest)
		return CatalogResult{State: StateApplied, Catalog: c.lastGood, Applied: true,
			AuthoritativeEmpty: len(catalog.Gateways) == 0}, nil
	}
	previousDigest, _ := c.lastGood.DigestedCatalog()
	order, ok := CompareRevision(catalog.Revision, c.lastGood.Revision)
	if !ok {
		return CatalogResult{State: StateFailed, Catalog: c.lastGood}, fmt.Errorf("catalog revision not comparable")
	}
	switch {
	case order < 0:
		return CatalogResult{State: StateStale, Catalog: c.lastGood}, nil
	case order == 0 && digest == previousDigest:
		return CatalogResult{State: StateApplied, Catalog: c.lastGood, Duplicate: true}, nil
	case order == 0:
		// Same revision with different protected bytes: the server may re-issue the same
		// revision with a new validity window (a lease rotation inside one revision).
		// Freshness is decided strictly by the response IssuedAt; the snapshot is applied
		// as-is (never max(old,new)), so a shortened or already expired validity fails
		// closed through DecideAdmission. An equal or older IssuedAt can never roll back
		// the retained snapshot, and a malformed/missing timestamp keeps today's conflict.
		issuedAt, issuedErr := parseUtc(catalog.IssuedAt)
		previousIssuedAt, previousErr := parseUtc(c.lastGood.IssuedAt)
		if issuedErr != nil || previousErr != nil {
			return CatalogResult{State: StateConflict, Catalog: c.lastGood}, nil
		}
		if !issuedAt.After(previousIssuedAt) {
			return CatalogResult{State: StateStale, Catalog: c.lastGood}, nil
		}
		c.applyCatalog(catalog, digest)
		return CatalogResult{State: StateApplied, Catalog: c.lastGood, Applied: true,
			AuthoritativeEmpty: len(catalog.Gateways) == 0}, nil
	default:
		c.applyCatalog(catalog, digest)
		return CatalogResult{State: StateApplied, Catalog: c.lastGood, Applied: true,
			AuthoritativeEmpty: len(catalog.Gateways) == 0}, nil
	}
}

func (c *Coordinator) applyCatalog(catalog CatalogResponse, digest string) {
	copied := catalog
	c.lastGood = &copied
	c.lastGoodDigest = digest
	c.catalogSubject = c.subject
	c.catalogTick = c.sessionTick
	c.admission = nil
}

// SyncAccess starts at most one effect per persisted key/body. The binding revision
// comes exactly from /me.binding_revision.
func (c *Coordinator) SyncAccess(ctx context.Context) (SyncResult, error) {
	return c.syncAccess(ctx, false)
}

// SyncAccessForced starts one NEW durable keyed operation for the currently admissible
// body even when the admissible digest is unchanged. The backend renews a mobile node
// lease only for an authenticated POST /access/sync (ensure_grant), so an idempotent
// replay of an applied receipt can never refresh the lease. Every existing fence still
// applies: an uncertain (lost response) operation is resent with its SAME durable key,
// a pending operation is replayed and never replaces its key, a recovery marker keeps
// its single rotation, and the subject/session/binding/catalog revision domain is
// re-resolved from the verified state. Only the same-digest "replay without a new
// operation" shortcut is bypassed; the new key is persisted before the POST. The runner
// calls this from its bounded renewal window (at most once per proven deadline).
func (c *Coordinator) SyncAccessForced(ctx context.Context) (SyncResult, error) {
	return c.syncAccess(ctx, true)
}

func (c *Coordinator) syncAccess(ctx context.Context, forceNewKey bool) (SyncResult, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	c.mu.Lock()
	if c.opts.Store == nil {
		c.mu.Unlock()
		return SyncResult{}, fmt.Errorf("accountaccess: receipt store required")
	}
	subject := c.opts.Subject()
	receipt := c.receipt
	c.mu.Unlock()

	if receipt != nil && receipt.Subject != subject {
		// A persisted operation for another stable subject is never replayed; the new
		// subject starts a new operation. The persisted copy is intentionally left in
		// the store for the old subject and is never reused under a new subject.
		// Refresh/token rotation within the same subject never discards an uncertain
		// operation.
		c.mu.Lock()
		if c.receipt == receipt {
			c.receipt = nil
		}
		c.mu.Unlock()
		receipt = nil
	}
	if receipt != nil {
	receiptCase:
		switch {
		case receipt.Response == nil:
			// Lost response or restart: resume the same durable operation.
			return c.resendSync(ctx, receipt)
		case receipt.Response.AccessApplicationState == "pending":
			// Pending is polled through /operations; it never starts a new effect.
			return SyncResult{State: StateApplied, Receipt: receipt, Replayed: true}, nil
		case receipt.Response.AccessApplicationState == "retryable_failure" ||
			receipt.Response.AccessApplicationState == "not_requested":
			if receipt.RecoveryRequired {
				// The observe marker declared this failed intent recoverable: exactly
				// one rotation, atomically, with no other state change.
				return c.rotateRecovery(ctx, receipt)
			}
			// Generic retryable/not_requested (or an old receipt without the additive
			// marker, restart, timeout, auth refresh, expiry or revision change):
			// probe the old operation once with the read-only opt-in observe GET
			// BEFORE any resend, so a server-side recovery predicate can actually be
			// observed. The probe never changes the persisted key/body and never
			// rotates on its own; it only refreshes the durable marker hint.
			_, probeErr := c.PollAccessOperation(ctx)
			if probeErr != nil {
				// The probe failed (transport/API/decode or a failed marker save):
				// the generic resume behavior is unchanged, with no new error
				// surfaced and the persisted key/body resent exactly as before.
				return c.resendSync(ctx, receipt)
			}
			c.mu.Lock()
			current := c.receipt
			c.mu.Unlock()
			if current == nil || current.Key != receipt.Key || current.Subject != receipt.Subject {
				// The probed operation is no longer the current one: same generic
				// resume as today.
				return c.resendSync(ctx, receipt)
			}
			if current.Response == nil {
				// The probe must never leave the durable operation without a
				// response: resume the current identity, exactly as today.
				return c.resendSync(ctx, current)
			}
			switch current.Response.AccessApplicationState {
			case "pending":
				// The read-only probe advanced the operation to pending: polling
				// owns it now, and a POST must not run for a pending operation.
				return SyncResult{State: StateApplied, Receipt: current, Replayed: true}, nil
			case "applied", "rejected":
				// The read-only probe observed a terminal operation: continue with
				// the existing terminal admissible-body semantics (the same digest
				// replays; a changed admissible body starts one new keyed intent).
				// A stale POST for the terminal operation never happens.
				receipt = current
				break receiptCase
			default:
				if !current.RecoveryRequired {
					// No usable marker (legacy server, required=false, or a stale
					// probe): the same persisted key/body resumes unchanged.
					return c.resendSync(ctx, current)
				}
				if current.RecoveryRevisions == nil {
					// A required marker without a usable revision pair never posts -
					// neither a new key nor the stale persisted body - and the
					// durable receipt stays as probed so a later poll can refresh
					// the hint.
					return SyncResult{}, ErrRecoveryRevisions
				}
				// The probe observed exactly one recoverable marker: one rotation.
				return c.rotateRecovery(ctx, current)
			}
		}
		// Terminal operation: compare the new admissible body below.
	}

	c.mu.Lock()
	currentSubject := c.opts.Subject()
	if c.lastMe == nil || !c.subjectSet || currentSubject != c.subject ||
		c.lastMeSubject != c.subject || c.lastMeTick != c.sessionTick {
		if receipt != nil && receipt.terminal() {
			// A terminal operation replays from its durable identity even without a
			// fresh /me snapshot; it never starts a new effect. A new admissible body
			// still requires resolved /me and catalog fences.
			c.mu.Unlock()
			return SyncResult{State: StateApplied, Receipt: receipt, Replayed: true}, nil
		}
		// No snapshot resolved for the current subject/session: a new sync must not
		// carry foreign revisions; the caller refreshes /me and the catalog first.
		c.mu.Unlock()
		return SyncResult{}, ErrMeRequired
	}
	if c.lastMe.BindingRevision == nil {
		c.mu.Unlock()
		return SyncResult{}, ErrNoBinding
	}
	bindingRevision := *c.lastMe.BindingRevision
	catalogRevision := ""
	if c.admission != nil && c.admission.subject == c.subject && c.admission.tick == c.sessionTick {
		catalogRevision = c.admission.CatalogRevision
	} else if c.lastGood != nil && c.catalogSubject == c.subject && c.catalogTick == c.sessionTick {
		catalogRevision = c.lastGood.Revision
	}
	c.mu.Unlock()
	if !ValidRevision(catalogRevision) {
		return SyncResult{}, fmt.Errorf("no authorized catalog revision for sync")
	}
	body := AccessSyncRequest{CatalogRevision: catalogRevision, BindingRevision: bindingRevision}
	bodyDigest, err := body.Digest()
	if err != nil {
		return SyncResult{}, err
	}
	if receipt != nil && receipt.BodyDigest == bodyDigest && !forceNewKey {
		// Same operation as the terminal receipt: replay without rotating the key.
		return SyncResult{State: StateApplied, Receipt: receipt, Replayed: true}, nil
	}
	// A terminal receipt and a new admissible body, no receipt at all, or the forced
	// on-demand renewal of the same admissible body: a new operation with a fresh
	// durable key, persisted before the network send.
	key, err := newIdempotencyKey()
	if err != nil {
		return SyncResult{}, err
	}
	pending := Receipt{Subject: subject, Key: key, BodyDigest: bodyDigest, Request: body}
	return c.sendSync(ctx, pending)
}

// resendSync resumes an uncertain operation with its persisted durable key and body.
func (c *Coordinator) resendSync(ctx context.Context, receipt *Receipt) (SyncResult, error) {
	return c.sendSync(ctx, *receipt)
}

// rotateRecovery performs the single marker-justified rotation: the new durable key
// carries exactly the marker revision pair (both valid) and is persisted before the POST.
// A marker without a valid pair never posts and never falls back to a stale persisted
// body; the durable receipt stays exactly as-is so a later poll can refresh the hint.
// This is the only rotation source: generic retryable/dead/exhausted, restart, timeout,
// lost response, auth refresh or an expiry/revision change never rotate.
func (c *Coordinator) rotateRecovery(ctx context.Context, receipt *Receipt) (SyncResult, error) {
	if receipt.RecoveryRevisions == nil {
		return SyncResult{}, ErrRecoveryRevisions
	}
	body := *receipt.RecoveryRevisions
	if !ValidRevision(body.CatalogRevision) || !ValidRevision(body.BindingRevision) {
		return SyncResult{}, ErrRecoveryRevisions
	}
	bodyDigest, err := body.Digest()
	if err != nil {
		return SyncResult{}, err
	}
	key, err := newIdempotencyKey()
	if err != nil {
		return SyncResult{}, err
	}
	rotationOf := ""
	if receipt.Response != nil {
		rotationOf = receipt.Response.OperationID
	}
	pending := Receipt{Subject: receipt.Subject, Key: key, BodyDigest: bodyDigest,
		Request: body, RotationOf: rotationOf}
	return c.sendSync(ctx, pending)
}

func (c *Coordinator) sendSync(ctx context.Context, pending Receipt) (SyncResult, error) {
	// Persist the operation identity BEFORE the network send so a lost response or a
	// restart resumes the same effect instead of starting a second one.
	if err := c.opts.Store.Save(pending); err != nil {
		return SyncResult{State: StateFailed}, err
	}
	c.mu.Lock()
	c.receipt = &pending
	c.mu.Unlock()

	response, apiError, err := c.opts.Client.SyncAccess(ctx, pending.Request, pending.Key)
	if err != nil {
		// The uncertain receipt stays durable for a bounded resume.
		return SyncResult{State: StateFailed}, err
	}
	if apiError != nil {
		return SyncResult{State: StateFailed}, apiStatus(apiError)
	}
	c.mu.Lock()
	current := c.receipt
	if current == nil || current.Key != pending.Key || current.Subject != pending.Subject {
		// The operation was replaced while the request was in flight (subject change):
		// the old token's response is never applied to, or saved over, the new one.
		c.mu.Unlock()
		return SyncResult{State: StateStale, Dropped: true}, nil
	}
	c.mu.Unlock()
	stored := Receipt{Subject: pending.Subject, Key: pending.Key, BodyDigest: pending.BodyDigest,
		Request: pending.Request, Response: &response, RotationOf: pending.RotationOf}
	c.mu.Lock()
	c.receipt = &stored
	c.mu.Unlock()
	if err := c.opts.Store.Save(stored); err != nil {
		return SyncResult{State: StateFailed}, err
	}
	state := StateApplied
	if !response.Terminal() {
		state = StateStale
	}
	return SyncResult{State: state, Receipt: &stored}, nil
}

// PollAccessOperation advances the current operation receipt with GET
// /operations/{id}: the real operation state is mapped to the receipt state and
// persisted durably before the in-memory receipt is advanced. A late poll of a
// replaced subject/operation changes nothing.
func (c *Coordinator) PollAccessOperation(ctx context.Context) (PollResult, error) {
	c.mu.Lock()
	if c.opts.Store == nil {
		c.mu.Unlock()
		return PollResult{}, fmt.Errorf("accountaccess: receipt store required")
	}
	receipt := c.receipt
	c.mu.Unlock()
	if receipt == nil || receipt.Response == nil {
		return PollResult{State: StateIdle}, nil
	}
	operationID := receipt.Response.OperationID
	if operationID == "" {
		return PollResult{}, fmt.Errorf("operation receipt has no operation id")
	}
	subject := c.opts.Subject()
	response, apiError, err := c.opts.Client.GetOperation(ctx, operationID)
	if err != nil {
		return PollResult{State: StateFailed}, err
	}
	if apiError != nil {
		return PollResult{State: StateFailed}, apiStatus(apiError)
	}
	if subject != c.opts.Subject() {
		return PollResult{State: StateStale, Stale: true, Dropped: true}, nil
	}
	state, err := mapOperationState(response.AccessApplicationState)
	if err != nil {
		return PollResult{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.receipt
	if current == nil || current.Subject != receipt.Subject || current.Key != receipt.Key ||
		current.Response == nil || current.Response.OperationID != operationID {
		// Late poll of a replaced operation/subject: the current operation is untouched.
		return PollResult{State: StateStale, Stale: true}, nil
	}
	updated := *current
	copied := *current.Response
	copied.AccessApplicationState = state
	updated.Response = &copied
	if state == "retryable_failure" || state == "not_requested" {
		// The observe=sync_recovery marker is keyed to this operation: every fresh poll
		// of the same operation re-derives the durable hint from the latest response, so
		// a later marker with Required=false (or no marker at all, e.g. a legacy server)
		// clears a stale hint instead of keeping a rotation right alive. A generic
		// retryable/dead/exhausted state never marks. Only the durable marker fields
		// change here; the operation identity and the agent state stay exactly as polled.
		required := response.SyncRecovery != nil && response.SyncRecovery.Required
		updated.RecoveryRequired = required
		updated.RecoveryRevisions = nil
		if required && ValidRevision(response.SyncRecovery.CatalogRevision) &&
			ValidRevision(response.SyncRecovery.BindingRevision) {
			pair := AccessSyncRequest{
				CatalogRevision: response.SyncRecovery.CatalogRevision,
				BindingRevision: response.SyncRecovery.BindingRevision,
			}
			updated.RecoveryRevisions = &pair
		}
	}
	if err := c.opts.Store.Save(updated); err != nil {
		// A failed save must not report a false success nor forget the pending
		// operation: the previously durable receipt remains in force.
		return PollResult{State: StateFailed}, err
	}
	c.receipt = &updated
	return PollResult{State: pollState(updated), Receipt: &updated, Terminal: updated.terminal()}, nil
}

func pollState(receipt Receipt) State {
	if receipt.Response != nil && receipt.Response.AccessApplicationState == "pending" {
		return StateStale
	}
	return StateApplied
}

func mapOperationState(value string) (string, error) {
	switch value {
	case "not_requested", "pending", "applied", "retryable_failure", "rejected":
		return value, nil
	}
	return "", fmt.Errorf("operation state %q is not a contract access_application_state", value)
}

// PollOperation reads own-operation progress without starting a new effect.
func (c *Coordinator) PollOperation(ctx context.Context, operationID string) (OperationResponse, error) {
	response, apiError, err := c.opts.Client.GetOperation(ctx, operationID)
	if err != nil {
		return OperationResponse{}, err
	}
	if apiError != nil {
		return OperationResponse{}, apiStatus(apiError)
	}
	return response, nil
}

func newIdempotencyKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
