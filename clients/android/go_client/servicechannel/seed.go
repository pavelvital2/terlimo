// Package servicechannel is the client-side seam of the service-only mobile API
// channel. It carries the existing mobile-v1 HTTP calls (challenge, session,
// installations, me, gateways, access/sync, operations) over a dedicated bounded
// service connection instead of direct HTTPS, without changing accountaccess or the
// accepted WLBS data plane. The public seed carries no PSK, admin secret, client
// certificate or private key: only the public service classifier, the pinned DTLS
// endpoint and the existing VK hash list.
package servicechannel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"wg-turn-client/wlwire"
)

// Seed is the public, non-personal service endpoint seed. ServiceClassifier is the
// public build-time classifier the node side installs through SetServiceClassifier:
// it is not a secret and not a right, it only selects the public WRAP/service
// transport class. The WRAP key is derived from it with the same deriveWrapKey
// mechanism the node transport uses. Revision/Environment are the trusted-update
// ordering fence and the deployment binding.
type Seed struct {
	Version           int      `json:"version"`
	Revision          string   `json:"revision,omitempty"`
	Environment       string   `json:"environment,omitempty"`
	PeerIP            string   `json:"peer_ip"`
	DTLSPort          int      `json:"dtls_port"`
	DTLSSPKISHA256    string   `json:"dtls_spki_sha256"`
	ServiceClassifier string   `json:"service_classifier"`
	VKHashes          []string `json:"vk_hashes"`
	StreamID          int      `json:"stream_id"`
	// recoveryProof is attached only by trusted Recovery v1 verification. It never
	// enters the durable/public JSON and does not grant SourceUser endpoint rights.
	recoveryProof *recoveryProof
}

// Source names the origin of one seed update. The slots stay separate so a user
// hash override can never rewrite the trusted endpoint, pin or classifier, and a
// cached update can never be replayed out of order.
type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceCached  Source = "cached"
	SourceUser    Source = "user"
)

var (
	// ErrSeedInvalid marks a seed that is malformed or carries a non-public field.
	ErrSeedInvalid = errors.New("SERVICE_SEED_INVALID")
	// ErrSeedSource marks an unknown seed source.
	ErrSeedSource = errors.New("SERVICE_SEED_SOURCE")
	// ErrSeedEndpoint marks a user seed that would rewrite the trusted endpoint.
	ErrSeedEndpoint = errors.New("SERVICE_SEED_ENDPOINT")
	// ErrSeedMissing marks a configured service channel without a usable seed.
	ErrSeedMissing = errors.New("SERVICE_SEED_MISSING")
	// ErrSeedStale marks an update whose revision is not strictly newer than the
	// last-good trusted revision (out-of-order or replayed update).
	ErrSeedStale = errors.New("SERVICE_SEED_STALE")
	// ErrSeedBinding marks an update or cached state bound to a foreign environment
	// or node.
	ErrSeedBinding = errors.New("SERVICE_SEED_BINDING")
)

const (
	seedVersion     = 1
	seedMaxBytes    = 8 * 1024
	seedMaxHashes   = 4
	seedMaxHashLen  = 128
	seedMaxClassLen = 128
	seedMaxRevLen   = 19
)

// StateNamespace is the versioned durable namespace for the last-good cached seed and
// the user hash override. It never carries builtin authority: builtin always comes
// from the packaged host seed, and the accountaccess receipt namespace is untouched.
const StateNamespace = "service_seed_v1"

const stateVersion = 1

var spkiEncodedSize = base64.RawURLEncoding.EncodedLen(sha256.Size)

// ParseSeed decodes and validates one seed document. Unknown fields are rejected, so
// a document that tries to smuggle a PSK, secret or certificate fails closed instead
// of being silently carried.
func ParseSeed(raw []byte) (Seed, error) {
	if len(raw) == 0 || len(raw) > seedMaxBytes {
		return Seed{}, fmt.Errorf("%w: size", ErrSeedInvalid)
	}
	var seed Seed
	if wlwire.StrictJSON(raw, &seed) != nil {
		return Seed{}, fmt.Errorf("%w: schema", ErrSeedInvalid)
	}
	if err := seed.Validate(); err != nil {
		return Seed{}, err
	}
	return seed, nil
}

// Validate checks every seed field against the accepted public seed schema.
func (s Seed) Validate() error {
	if s.Version != seedVersion {
		return fmt.Errorf("%w: version", ErrSeedInvalid)
	}
	if _, err := revisionValue(s.Revision); err != nil {
		return err
	}
	if s.Environment != "" && s.Environment != "test" && s.Environment != "production" {
		return fmt.Errorf("%w: environment", ErrSeedInvalid)
	}
	if net.ParseIP(s.PeerIP) == nil {
		return fmt.Errorf("%w: peer_ip", ErrSeedInvalid)
	}
	if s.DTLSPort < 1 || s.DTLSPort > 65535 {
		return fmt.Errorf("%w: dtls_port", ErrSeedInvalid)
	}
	if err := validatePin(s.DTLSSPKISHA256); err != nil {
		return err
	}
	if s.ServiceClassifier == "" || len(s.ServiceClassifier) > seedMaxClassLen ||
		strings.ContainsAny(s.ServiceClassifier, " \t\r\n\v\f") {
		return fmt.Errorf("%w: service_classifier", ErrSeedInvalid)
	}
	for _, r := range s.ServiceClassifier {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%w: service_classifier", ErrSeedInvalid)
		}
	}
	if err := validateHashes(s.VKHashes); err != nil {
		return err
	}
	if s.StreamID < 0 {
		return fmt.Errorf("%w: stream_id", ErrSeedInvalid)
	}
	return nil
}

// validatePin mirrors managedPinVerifier: one canonical raw-URL base64 SHA-256 pin.
func validatePin(pin string) error {
	if pin == "" || len(pin) != spkiEncodedSize {
		return fmt.Errorf("%w: dtls_spki_sha256", ErrSeedInvalid)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(pin)
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != pin {
		return fmt.Errorf("%w: dtls_spki_sha256", ErrSeedInvalid)
	}
	return nil
}

func validateHashes(hashes []string) error {
	if len(hashes) < 1 || len(hashes) > seedMaxHashes {
		return fmt.Errorf("%w: vk_hashes", ErrSeedInvalid)
	}
	for _, hash := range hashes {
		if hash == "" || len(hash) > seedMaxHashLen || strings.ContainsAny(hash, " \t\r\n") {
			return fmt.Errorf("%w: vk_hash", ErrSeedInvalid)
		}
	}
	return nil
}

// revisionValue parses the canonical decimal revision. An absent revision is 0.
func revisionValue(revision string) (uint64, error) {
	if revision == "" {
		return 0, nil
	}
	if len(revision) > seedMaxRevLen || (len(revision) > 1 && revision[0] == '0') {
		return 0, fmt.Errorf("%w: revision", ErrSeedInvalid)
	}
	value, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: revision", ErrSeedInvalid)
	}
	return value, nil
}

// endpointBinding fingerprints the trusted node identity a user hash override is
// bound to. It is public data (endpoint, public pin, public classifier), never a
// secret, and is used only to detect that the trusted endpoint moved.
func endpointBinding(seed Seed) string {
	fingerprint := fmt.Sprintf("%s|%d|%s|%s|%d", seed.PeerIP, seed.DTLSPort, seed.DTLSSPKISHA256, seed.ServiceClassifier, seed.StreamID)
	digest := sha256.Sum256([]byte(fingerprint))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func sameEndpoint(a, b Seed) bool {
	return a.PeerIP == b.PeerIP && a.DTLSPort == b.DTLSPort && a.DTLSSPKISHA256 == b.DTLSSPKISHA256 &&
		a.ServiceClassifier == b.ServiceClassifier && a.StreamID == b.StreamID
}

// PersistFunc durably stores the service seed state through the host persist channel
// and returns only after the host acknowledged (persist_result). Failure means the
// in-memory slots must not advance.
type PersistFunc func(ctx context.Context, payload []byte) error

// Config is the store binding: the environment the durable cache is bound to and the
// optional host persistence hook. Nil persistence keeps the store memory-only.
type Config struct {
	Environment string
	Persist     PersistFunc
}

// userState is the user hash override slot plus the trusted node binding it was
// created for. The override is effective only while the binding still matches the
// trusted endpoint; it can never move pin/endpoint/classifier.
type userState struct {
	binding string
	hashes  []string
}

// Store owns the three separate seed slots: builtin (packaged), cached (last-good
// durable) and user (hash-only override). Trusted updates are ordered by revision and
// bound to the environment; a fully validated candidate replaces the last-good state
// only after persistence acknowledged, or the last-good state stays untouched.
type Store struct {
	mu          sync.RWMutex
	environment string
	persist     PersistFunc
	builtin     *Seed
	cached      *Seed
	user        *userState
	revision    uint64
}

// NewStore returns an empty store bound to the given environment and persistence hook.
func NewStore(config Config) *Store {
	return &Store{environment: config.Environment, persist: config.Persist}
}

// Update atomically installs one candidate seed for the given source. On any error the
// previous last-good state is preserved, and a durable update is persisted before the
// in-memory slots advance.
func (s *Store) Update(source Source, raw []byte) error {
	switch source {
	case SourceBuiltin, SourceCached, SourceUser:
	default:
		return ErrSeedSource
	}
	candidate, err := ParseSeed(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if source == SourceUser {
		return s.updateUserLocked(candidate)
	}
	return s.updateTrustedLocked(source, candidate)
}

// updateTrustedLocked installs a builtin/cached trusted seed. The revision must be
// strictly newer than the last-good trusted revision unless no trusted seed is
// installed yet, and the environment binding must match.
func (s *Store) updateTrustedLocked(source Source, candidate Seed) error {
	if err := s.checkEnvironmentLocked(candidate); err != nil {
		return err
	}
	revision, err := revisionValue(candidate.Revision)
	if err != nil {
		return err
	}
	if s.hasTrustedLocked() && revision <= s.revision {
		return fmt.Errorf("%w: revision %q", ErrSeedStale, candidate.Revision)
	}
	next := s.cloneLocked()
	if source == SourceBuiltin {
		next.builtin = &candidate
	} else {
		next.cached = &candidate
	}
	next.revision = revision
	return s.commitLocked(next, source != SourceBuiltin)
}

// updateUserLocked installs a hash-only override for the current trusted endpoint.
func (s *Store) updateUserLocked(candidate Seed) error {
	trusted, _, ok := s.trustedLocked()
	if !ok {
		return fmt.Errorf("%w: no trusted endpoint", ErrSeedEndpoint)
	}
	if !sameEndpoint(trusted, candidate) {
		return ErrSeedEndpoint
	}
	if err := s.checkEnvironmentLocked(candidate); err != nil {
		return err
	}
	if candidate.Environment != "" && trusted.Environment != "" && candidate.Environment != trusted.Environment {
		return fmt.Errorf("%w: user environment", ErrSeedBinding)
	}
	next := s.cloneLocked()
	next.user = &userState{binding: endpointBinding(trusted), hashes: append([]string(nil), candidate.VKHashes...)}
	return s.commitLocked(next, true)
}

// ClearUser removes the user hash override and restores the last-good builtin/cached
// hash set. The removal is persisted before the in-memory slot is cleared.
func (s *Store) ClearUser() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user == nil {
		return nil
	}
	next := s.cloneLocked()
	next.user = nil
	return s.commitLocked(next, true)
}

// Current returns a copy of the effective seed, its winning source and whether any
// trusted seed is installed. The user override wins only while its node binding still
// matches the trusted endpoint.
func (s *Store) Current() (Seed, Source, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	trusted, source, ok := s.trustedLocked()
	if !ok {
		return Seed{}, "", false
	}
	trusted.VKHashes = append([]string(nil), trusted.VKHashes...)
	if s.user != nil && s.user.binding == endpointBinding(trusted) {
		trusted.VKHashes = append([]string(nil), s.user.hashes...)
		source = SourceUser
	}
	return trusted, source, true
}

// State renders the durable slots (cached + user override) as one versioned
// namespace envelope. Builtin is packaged authority and is never part of the blob.
func (s *Store) State() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stateLocked()
}

// LoadState installs the durable slots from one namespace envelope. Malformed,
// foreign-environment and unknown-namespace blobs are rejected without touching the
// last-good state. A valid but stale cached revision is ignored (the builtin wins);
// a user override whose binding no longer matches stays stored but inactive.
func (s *Store) LoadState(blob []byte) error {
	if len(blob) == 0 {
		return nil
	}
	var envelope stateEnvelope
	if wlwire.StrictJSON(blob, &envelope) != nil {
		return fmt.Errorf("%w: state schema", ErrSeedInvalid)
	}
	if envelope.Namespace != StateNamespace || envelope.Version != stateVersion {
		return fmt.Errorf("%w: state namespace", ErrSeedInvalid)
	}
	if envelope.Environment != "" && s.environment != "" && envelope.Environment != s.environment {
		return fmt.Errorf("%w: state environment", ErrSeedBinding)
	}
	var cached *Seed
	if envelope.Cached != nil {
		if err := envelope.Cached.Validate(); err != nil {
			return err
		}
		if envelope.Environment != "" && envelope.Cached.Environment != "" && envelope.Cached.Environment != envelope.Environment {
			return fmt.Errorf("%w: cached environment", ErrSeedBinding)
		}
		cached = envelope.Cached
	}
	var user *userState
	if envelope.User != nil {
		if envelope.User.Binding == "" {
			return fmt.Errorf("%w: user binding", ErrSeedInvalid)
		}
		if err := validateHashes(envelope.User.VKHashes); err != nil {
			return err
		}
		user = &userState{binding: envelope.User.Binding, hashes: append([]string(nil), envelope.User.VKHashes...)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached != nil {
		if err := s.checkEnvironmentLocked(*cached); err != nil {
			return err
		}
		if s.hasTrustedLocked() {
			revision, err := revisionValue(cached.Revision)
			if err != nil {
				return err
			}
			if revision <= s.revision {
				// Out-of-order durable cache: keep the newer last-good trusted seed.
				cached = nil
			}
		}
	}
	next := s.cloneLocked()
	if cached != nil {
		revision, err := revisionValue(cached.Revision)
		if err != nil {
			return err
		}
		next.cached = cached
		next.revision = revision
	}
	if user != nil {
		next.user = user
	}
	s.installLocked(next)
	return nil
}

// checkEnvironmentLocked enforces the environment binding when both sides declare one.
func (s *Store) checkEnvironmentLocked(candidate Seed) error {
	if s.environment != "" && candidate.Environment != "" && candidate.Environment != s.environment {
		return fmt.Errorf("%w: environment", ErrSeedBinding)
	}
	return nil
}

func (s *Store) hasTrustedLocked() bool { return s.builtin != nil || s.cached != nil }

// trustedLocked picks the effective trusted seed. Ordering guarantees the newer slot
// has the strictly greater revision; equality is only possible for one installed slot.
func (s *Store) trustedLocked() (Seed, Source, bool) {
	switch {
	case s.builtin != nil && s.cached != nil:
		builtinRevision, _ := revisionValue(s.builtin.Revision)
		cachedRevision, _ := revisionValue(s.cached.Revision)
		if cachedRevision >= builtinRevision {
			return *s.cached, SourceCached, true
		}
		return *s.builtin, SourceBuiltin, true
	case s.cached != nil:
		return *s.cached, SourceCached, true
	case s.builtin != nil:
		return *s.builtin, SourceBuiltin, true
	}
	return Seed{}, "", false
}

func (s *Store) cloneLocked() *Store {
	next := &Store{environment: s.environment, persist: s.persist, revision: s.revision}
	if s.builtin != nil {
		copied := cloneSeed(*s.builtin)
		next.builtin = &copied
	}
	if s.cached != nil {
		copied := cloneSeed(*s.cached)
		next.cached = &copied
	}
	if s.user != nil {
		next.user = &userState{binding: s.user.binding, hashes: append([]string(nil), s.user.hashes...)}
	}
	return next
}

func cloneSeed(seed Seed) Seed {
	seed.VKHashes = append([]string(nil), seed.VKHashes...)
	return seed
}

// commitLocked persists the next durable state (when requested) and only then swaps
// the in-memory slots. A failed persist leaves every last-good slot untouched.
func (s *Store) commitLocked(next *Store, durable bool) error {
	if durable && s.persist != nil {
		payload, err := next.stateLocked()
		if err != nil {
			return err
		}
		if err := s.persist(context.Background(), payload); err != nil {
			return err
		}
	}
	s.installLocked(next)
	return nil
}

func (s *Store) installLocked(next *Store) {
	s.builtin, s.cached, s.user, s.revision = next.builtin, next.cached, next.user, next.revision
}

type stateEnvelope struct {
	Namespace   string     `json:"namespace"`
	Version     int        `json:"version"`
	Environment string     `json:"environment,omitempty"`
	Cached      *Seed      `json:"cached,omitempty"`
	User        *stateUser `json:"user,omitempty"`
}

type stateUser struct {
	Binding  string   `json:"binding"`
	VKHashes []string `json:"vk_hashes"`
}

func (s *Store) stateLocked() ([]byte, error) {
	environment := s.environment
	envelope := stateEnvelope{Namespace: StateNamespace, Version: stateVersion, Environment: environment}
	if s.cached != nil {
		copied := cloneSeed(*s.cached)
		envelope.Cached = &copied
		if environment == "" {
			envelope.Environment = copied.Environment
		}
	}
	if s.user != nil {
		envelope.User = &stateUser{Binding: s.user.binding, VKHashes: append([]string(nil), s.user.hashes...)}
	}
	return json.Marshal(envelope)
}
