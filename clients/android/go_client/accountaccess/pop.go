package accountaccess

// Go port of the accepted TERLIMO mobile v1 PoP canonical implementation
// (contracts auth/pop_canonical.py + POP_PAYLOAD_V1.md, revision 1.5.0-candidate.2).
// The bridge signs the exact WLBS-POP-1 transcript; private keys never enter this module.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const popPrefix = "WLBS-POP-1\x00"

// PopError carries the bounded contract code of a canonicalization failure.
type PopError struct{ Code string }

func (e *PopError) Error() string { return e.Code }

func popFailure(code string) error { return &PopError{Code: code} }

// PoPEnvironment is one of the contract environments.
type PoPEnvironment string

const (
	EnvironmentTest       PoPEnvironment = "test"
	EnvironmentProduction PoPEnvironment = "production"
)

// CanonicalJSON reproduces the contract canonical JSON: NFC strings, sorted keys, compact
// separators, integers only (floats are non-canonical), UTF-8 output.
func CanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := canonicalValue(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func canonicalValue(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(norm.NFC.String(typed))
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case int:
		fmt.Fprintf(buffer, "%d", typed)
	case int64:
		fmt.Fprintf(buffer, "%d", typed)
	case uint64:
		fmt.Fprintf(buffer, "%d", typed)
	case float64:
		if typed != float64(int64(typed)) {
			return popFailure("BAD_MESSAGE")
		}
		fmt.Fprintf(buffer, "%d", int64(typed))
	case []string:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := canonicalValue(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := canonicalValue(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		seen := map[string]bool{}
		for key := range typed {
			normalized := norm.NFC.String(key)
			if seen[normalized] {
				return popFailure("BAD_MESSAGE")
			}
			seen[normalized] = true
			keys = append(keys, normalized)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			if err := canonicalValue(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[key] = item
		}
		return canonicalValue(buffer, converted)
	default:
		return popFailure("BAD_MESSAGE")
	}
	return nil
}

// KnownTopLevel is the exact accepted top-level payload field set.
var KnownTopLevel = map[string]bool{
	"env": true, "scope": true, "op": true, "installation_id": true, "ts": true,
	"nonce": true, "request_id": true, "idempotency_key": true, "requested_scopes": true,
	"device_name": true, "known_revision": true, "payment_id": true, "quote_id": true,
	"public_key_spki_b64": true, "platform": true, "name": true, "catalog_revision": true,
	"binding_revision": true, "extensions": true, "critical": true,
}

var perScopeFields = map[string][]string{
	"enrollment":    {"public_key_spki_b64", "platform", "name"},
	"session":       {"requested_scopes"},
	"binding":       {"device_name"},
	"telegram-link": {},
	"catalog":       {"known_revision"},
	"checkout":      {"payment_id"},
	"payment:write": {"quote_id"},
	"access:sync":   {"catalog_revision", "binding_revision"},
}

var requiredActionFields = map[string][]string{
	"enrollment":    {"public_key_spki_b64", "platform"},
	"session":       {"requested_scopes"},
	"binding":       {},
	"telegram-link": {},
	"catalog":       {"known_revision"},
	"checkout":      {"payment_id"},
	"payment:write": {"quote_id"},
	"access:sync":   {"catalog_revision", "binding_revision"},
}

// BusinessProjection returns the stable business identity of an operation.
func BusinessProjection(payload map[string]any) (map[string]any, error) {
	scope, _ := payload["scope"].(string)
	fields, ok := perScopeFields[scope]
	if !ok {
		return nil, popFailure("BAD_MESSAGE")
	}
	for _, required := range requiredActionFields[scope] {
		if _, present := payload[required]; !present {
			return nil, popFailure("BAD_MESSAGE")
		}
	}
	projection := map[string]any{}
	for _, common := range []string{"env", "scope", "op", "installation_id"} {
		value, present := payload[common]
		if !present {
			return nil, popFailure("BAD_MESSAGE")
		}
		projection[common] = value
	}
	for _, field := range fields {
		if value, present := payload[field]; present {
			projection[field] = value
		}
	}
	if extensions, present := payload["extensions"]; present {
		projection["extensions"] = extensions
	}
	return projection, nil
}

// BusinessDigest is SHA-256 over the canonical business projection.
func BusinessDigest(payload map[string]any) (string, error) {
	projection, err := BusinessProjection(payload)
	if err != nil {
		return "", err
	}
	canonical, err := CanonicalJSON(projection)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// DecodeProofPayload decodes a signed payload and proves canonicality by re-encoding.
func DecodeProofPayload(signedPayloadB64 string) ([]byte, map[string]any, error) {
	raw, err := B64URLDecodeStrict(signedPayloadB64, -1)
	if err != nil {
		return nil, nil, popFailure("BAD_MESSAGE")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil, popFailure("BAD_MESSAGE")
	}
	if decoder.More() {
		return nil, nil, popFailure("BAD_MESSAGE")
	}
	normalized, _ := normalizeNumbers(payload).(map[string]any)
	if normalized == nil {
		return nil, nil, popFailure("BAD_MESSAGE")
	}
	canonical, err := CanonicalJSON(normalized)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(canonical, raw) {
		return nil, nil, popFailure("BAD_MESSAGE")
	}
	return raw, normalized, nil
}

func normalizeNumbers(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		text := typed.String()
		if !strings.ContainsAny(text, ".eE") {
			return typed
		}
		return typed
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeNumbers(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = normalizeNumbers(item)
		}
		return typed
	}
	return value
}

// POPMessage builds the exact WLBS-POP-1 transcript signed by the installation key.
func POPMessage(requestID, challengeID, nonceB64 string, payloadBytes []byte) ([]byte, error) {
	requestRaw, err := DecodeHex16(requestID)
	if err != nil {
		return nil, err
	}
	challengeRaw, err := DecodeHex16(challengeID)
	if err != nil {
		return nil, err
	}
	nonceRaw, err := B64URLDecodeStrict(nonceB64, -1)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payloadBytes)
	message := make([]byte, 0, len(popPrefix)+16+16+len(nonceRaw)+32)
	message = append(message, popPrefix...)
	message = append(message, requestRaw...)
	message = append(message, challengeRaw...)
	message = append(message, nonceRaw...)
	message = append(message, sum[:]...)
	return message, nil
}

// DecodeHex16 decodes exactly 16 bytes of lowercase hex.
func DecodeHex16(value string) ([]byte, error) {
	if len(value) != 32 || strings.ToLower(value) != value {
		return nil, popFailure("BAD_MESSAGE")
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 16 {
		return nil, popFailure("BAD_MESSAGE")
	}
	return raw, nil
}

// B64URLDecodeStrict decodes canonical unpadded base64url; size >= 0 requires an exact size.
func B64URLDecodeStrict(value string, size int) ([]byte, error) {
	if value == "" || strings.ContainsAny(value, "=+ \n") {
		return nil, popFailure("BAD_MESSAGE")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, popFailure("BAD_MESSAGE")
	}
	if size >= 0 && len(raw) != size {
		return nil, popFailure("BAD_MESSAGE")
	}
	if B64URLEncode(raw) != value {
		return nil, popFailure("BAD_MESSAGE")
	}
	return raw, nil
}

// B64URLEncode is canonical unpadded base64url.
func B64URLEncode(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// InstallationFingerprint is SHA-256(SPKI DER) lowercase hex.
func InstallationFingerprint(spkiDER []byte) string {
	sum := sha256.Sum256(spkiDER)
	return hex.EncodeToString(sum[:])
}

// NewRequestID returns a 16-byte lowercase hex request id.
func NewRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// CheckCriticalFields rejects unknown critical field names.
func CheckCriticalFields(payload map[string]any, known map[string]bool) error {
	value, present := payload["critical"]
	if !present || value == nil {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		return popFailure("BAD_MESSAGE")
	}
	for _, item := range list {
		name, ok := item.(string)
		if !ok || !known[name] {
			return popFailure("UNKNOWN_CRITICAL_FIELD")
		}
	}
	return nil
}

// CheckTopLevelFields rejects unknown top-level payload fields.
func CheckTopLevelFields(payload map[string]any, known map[string]bool) error {
	for name := range payload {
		if !known[name] {
			return popFailure("BAD_MESSAGE")
		}
	}
	return nil
}

// ErrPopEnvironment marks an environment mismatch.
var ErrPopEnvironment = errors.New("WRONG_ENVIRONMENT")
