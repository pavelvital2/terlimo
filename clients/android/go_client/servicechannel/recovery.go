package servicechannel

// Recovery v1 uses the existing public Seed and durable namespace. A verified
// candidate is staged separately; only the caller's successful authenticated,
// current-attempt recovery flow may commit it through a guarded host writer.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const recoveryCodeMax = 3500
const recoveryDomain = "TERLIMO-RECOVERY-V1\x00"

var (
	ErrRecoveryUnavailable = errors.New("RECOVERY_UNAVAILABLE")
	ErrRecoveryInvalid     = errors.New("RECOVERY_INVALID")
	ErrRecoverySignature   = errors.New("RECOVERY_SIGNATURE")
	ErrRecoveryEnvironment = errors.New("RECOVERY_ENVIRONMENT")
	ErrRecoveryStale       = errors.New("RECOVERY_STALE")
	ErrRecoveryPersist     = errors.New("RECOVERY_PERSIST")
	ErrRecoveryCancelled   = errors.New("RECOVERY_CANCELLED")
)

// recoveryProof binds admission to the verified public fields and environment.
// It prevents a parsed unsigned Seed, or a modified verified Seed, from reaching
// the separate Recovery trusted-update path. It carries no key or account data.
type recoveryProof struct {
	digest      [sha256.Size]byte
	environment string
}

// DecodeRecoveryVerifyKey consumes only trusted packaged configuration, never a
// signer supplied inside a recovery code. Missing/malformed config disables recovery.
func DecodeRecoveryVerifyKey(encoded string) (ed25519.PublicKey, error) {
	if len(encoded) != base64.RawURLEncoding.EncodedLen(ed25519.PublicKeySize) {
		return nil, ErrRecoveryUnavailable
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, ErrRecoveryUnavailable
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyRecoveryCode verifies the signature over the exact decoded UTF8 payload,
// without JSON reserialization. Environment and verification key are supplied by
// trusted configuration. This function performs no network or persistence.
func VerifyRecoveryCode(code string, trustedKey ed25519.PublicKey, environment string) (Seed, error) {
	if len(trustedKey) != ed25519.PublicKeySize {
		return Seed{}, ErrRecoveryUnavailable
	}
	if environment != "test" && environment != "production" {
		return Seed{}, ErrRecoveryEnvironment
	}
	code = strings.TrimSpace(code)
	if len(code) == 0 || len(code) > recoveryCodeMax {
		return Seed{}, ErrRecoveryInvalid
	}
	for _, b := range []byte(code) {
		if b < 0x21 || b > 0x7e {
			return Seed{}, ErrRecoveryInvalid
		}
	}
	parts := strings.Split(code, ".")
	if len(parts) != 3 || parts[0] != "TR1" {
		return Seed{}, ErrRecoveryInvalid
	}
	payload, err := decodeRecoverySegment(parts[1], seedMaxBytes)
	if err != nil || !utf8.Valid(payload) {
		return Seed{}, ErrRecoveryInvalid
	}
	signature, err := decodeRecoverySegment(parts[2], ed25519.SignatureSize)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Seed{}, ErrRecoveryInvalid
	}
	message := make([]byte, 0, len(recoveryDomain)+len(payload))
	message = append(message, recoveryDomain...)
	message = append(message, payload...)
	if !ed25519.Verify(trustedKey, message, signature) {
		return Seed{}, ErrRecoverySignature
	}
	seed, err := ParseSeed(payload)
	if err != nil {
		return Seed{}, fmt.Errorf("%w: %w", ErrRecoveryInvalid, err)
	}
	// All nine Recovery v1 fields are present and non-null, including zero-valued
	// stream_id and decimal revision. Legacy ParseSeed optional fields stay unchanged.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return Seed{}, ErrRecoveryInvalid
	}
	// encoding/json also matches struct fields case-insensitively. Exact key count
	// plus required literal names closes that alias path (e.g. version + Version).
	if len(fields) != 9 {
		return Seed{}, ErrRecoveryInvalid
	}
	for _, name := range []string{"version", "revision", "environment", "peer_ip", "dtls_port", "dtls_spki_sha256", "service_classifier", "vk_hashes", "stream_id"} {
		raw, present := fields[name]
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return Seed{}, ErrRecoveryInvalid
		}
	}
	if seed.Environment != environment {
		return Seed{}, fmt.Errorf("%w: %w", ErrRecoveryEnvironment, ErrSeedBinding)
	}
	if seed.Revision == "" {
		return Seed{}, ErrRecoveryInvalid
	}
	for _, digit := range seed.Revision {
		if digit < '0' || digit > '9' {
			return Seed{}, ErrRecoveryInvalid
		}
	}
	seed.recoveryProof = &recoveryProof{digest: recoverySeedDigest(seed), environment: environment}
	return seed, nil
}

func decodeRecoverySegment(encoded string, max int) ([]byte, error) {
	if encoded == "" || len(encoded) > base64.RawURLEncoding.EncodedLen(max) {
		return nil, ErrRecoveryInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > max || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, ErrRecoveryInvalid
	}
	return raw, nil
}

func recoverySeedDigest(seed Seed) [sha256.Size]byte {
	// Seed is a closed public struct; marshaling it has no fallible field types.
	raw, _ := json.Marshal(seed)
	return sha256.Sum256(raw)
}

// ValidateRecovery checks verified provenance and ordering against the currently
// trusted seed, without changing any slot. noop means equal revision and identical
// public fields; a user override does not alter this comparison or get cleared.
func (s *Store) ValidateRecovery(candidate Seed) (noop bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.validateRecoveryLocked(candidate)
}

func (s *Store) validateRecoveryLocked(candidate Seed) (bool, error) {
	proof := candidate.recoveryProof
	if proof == nil || proof.digest != recoverySeedDigest(candidate) || proof.environment != candidate.Environment {
		return false, ErrRecoveryInvalid
	}
	if err := candidate.Validate(); err != nil {
		return false, fmt.Errorf("%w: %w", ErrRecoveryInvalid, err)
	}
	if err := s.checkEnvironmentLocked(candidate); err != nil {
		return false, fmt.Errorf("%w: %w", ErrRecoveryEnvironment, err)
	}
	trusted, _, present := s.trustedLocked()
	if present && trusted.Environment != "" && candidate.Environment != trusted.Environment {
		return false, fmt.Errorf("%w: %w", ErrRecoveryEnvironment, ErrSeedBinding)
	}
	revision, _ := revisionValue(candidate.Revision)
	if present && revision <= s.revision {
		if revision == s.revision && recoverySeedDigest(trusted) == proof.digest {
			return true, nil
		}
		return false, fmt.Errorf("%w: %w", ErrRecoveryStale, ErrSeedStale)
	}
	return false, nil
}

// StageRecovery returns a memory-only Store exposing the verified candidate endpoint
// to the existing transport. Neither the source Store nor its durable state changes.
// The caller still must authenticate and correlate the candidate service response.
func (s *Store) StageRecovery(candidate Seed) (*Store, error) {
	candidate = cloneSeed(candidate)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.validateRecoveryLocked(candidate); err != nil {
		return nil, err
	}
	environment := s.environment
	if environment == "" {
		environment = candidate.Environment
	}
	revision, _ := revisionValue(candidate.Revision)
	return &Store{environment: environment, builtin: &candidate, revision: revision}, nil
}

// CommitRecovery atomically persists cached=candidate and user=nil in the existing
// namespace before installing either change. It rechecks admission/order under lock.
//
// REQUIRED caller boundary: invoke only after a normal authenticated service response
// for the current installation/environment/attempt. The actual host persistence writer
// must hold its external current-owner guard through its atomic durable write. This
// mutex and ctx are not a replacement for that writer fence. A successful persist ACK
// completes the transaction; later cancellation cannot undo the acknowledged write.
func (s *Store) CommitRecovery(ctx context.Context, candidate Seed) error {
	candidate = cloneSeed(candidate)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return ErrRecoveryCancelled
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrRecoveryCancelled, err)
	}
	noop, err := s.validateRecoveryLocked(candidate)
	if err != nil {
		return err
	}
	if noop {
		return nil
	}
	if s.persist == nil {
		return fmt.Errorf("%w: writer unavailable", ErrRecoveryPersist)
	}
	next := s.cloneLocked()
	next.cached, next.user = &candidate, nil
	next.revision, _ = revisionValue(candidate.Revision)
	payload, err := next.stateLocked()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecoveryPersist, err)
	}
	if err := s.persist(ctx, payload); err != nil {
		if errors.Is(err, ErrRecoveryCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", ErrRecoveryCancelled, err)
		}
		return fmt.Errorf("%w: %w", ErrRecoveryPersist, err)
	}
	s.installLocked(next)
	return nil
}
