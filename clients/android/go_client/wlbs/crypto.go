package wlbs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

func EncodeBinary(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func DecodeBinary(s string, size int) ([]byte, error) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil || EncodeBinary(b) != s || (size >= 0 && len(b) != size) {
		return nil, failure("BAD_MESSAGE")
	}
	return b, nil
}

// StrictJSON rejects duplicate keys at every depth, invalid UTF-8, trailing data,
// and excessive nesting before decoding. Unknown optional fields may be ignored.
func StrictJSON(data []byte, out any) error {
	if len(data) < 1 || len(data) > MaxBody || !utf8.Valid(data) {
		return failure("BAD_MESSAGE")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return failure("BAD_MESSAGE")
		}
		tok, e := d.Token()
		if e != nil {
			return failure("BAD_MESSAGE")
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return failure("BAD_MESSAGE")
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return failure("BAD_MESSAGE")
				}
				seen[s] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return failure("BAD_MESSAGE")
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return failure("BAD_MESSAGE")
			}
		default:
			return failure("BAD_MESSAGE")
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return failure("BAD_MESSAGE")
	}
	if e := json.Unmarshal(data, out); e != nil {
		if IsCountryCodeWireError(e) {
			return errCountryCodeWire
		}
		return failure("BAD_MESSAGE")
	}
	return nil
}

// IsCountryCodeWireError identifies only the Node country presence/type guard;
// it does not classify arbitrary BAD_CATALOG validation failures.
func IsCountryCodeWireError(err error) bool {
	return errors.Is(err, errCountryCodeWire)
}

func ParsePublicKey(spki []byte) (*ecdsa.PublicKey, error) {
	p, e := x509.ParsePKIXPublicKey(spki)
	if e != nil {
		return nil, failure("TRUST_FAILED")
	}
	key, ok := p.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() || !key.Curve.IsOnCurve(key.X, key.Y) {
		return nil, failure("TRUST_FAILED")
	}
	return key, nil
}
func InstallationID(spki []byte) (string, error) {
	if _, e := ParsePublicKey(spki); e != nil {
		return "", e
	}
	d := sha256.Sum256(spki)
	return hex.EncodeToString(d[:]), nil
}
func VerifyTranscript(spki, transcript, signature []byte) error {
	key, e := ParsePublicKey(spki)
	if e != nil {
		return e
	}
	h := sha256.Sum256(transcript)
	if !ecdsa.VerifyASN1(key, h[:], signature) {
		return failure("PROOF_INVALID")
	}
	return nil
}
func LinkTranscript(payload []byte) []byte { return append([]byte("WL-LINK-1\x00"), payload...) }
func BootstrapTranscript(id ID, challenge, nonce, payload []byte) ([]byte, error) {
	if len(challenge) != 16 || len(nonce) != 32 {
		return nil, failure("BAD_MESSAGE")
	}
	t := append([]byte("WLBS-POP-1\x00"), id[:]...)
	t = append(t, challenge...)
	t = append(t, nonce...)
	h := sha256.Sum256(payload)
	return append(t, h[:]...), nil
}

const ExporterLabel = "EXPORTER-WL-VPN-POP-1"

func VPNTranscript(exporter, challenge, nonce, payload []byte) ([]byte, error) {
	if len(exporter) != 32 || len(challenge) != 16 || len(nonce) != 32 {
		return nil, failure("BAD_MESSAGE")
	}
	t := append([]byte("WL-VPN-POP-1\x00"), exporter...)
	t = append(t, challenge...)
	t = append(t, nonce...)
	h := sha256.Sum256(payload)
	return append(t, h[:]...), nil
}
func VerifySPKIPin(certDER []byte, pin string) error {
	expected, e := DecodeBinary(pin, 32)
	if e != nil {
		return failure("TRUST_FAILED")
	}
	cert, e := x509.ParseCertificate(certDER)
	if e != nil {
		return failure("TRUST_FAILED")
	}
	actual := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return failure("TRUST_FAILED")
	}
	return nil
}
