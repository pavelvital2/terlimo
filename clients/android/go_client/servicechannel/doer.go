package servicechannel

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"

	"wg-turn-client/wlwire"
)

const (
	maxRequestBody  = 64 * 1024
	maxResponseBody = 1024 * 1024
	maxHeaders      = 8 * 1024
	maxHeaderValue  = 1024
	maxPathQuery    = 2048
)

var (
	// ErrOriginRejected marks a request that does not target the trusted base URL origin.
	ErrOriginRejected = errors.New("CHANNEL_ORIGIN_REJECTED")
	// ErrPathRejected marks a method/path/query outside the fixed mobile API allowlist.
	ErrPathRejected = errors.New("CHANNEL_PATH_REJECTED")
	// ErrRequestRejected marks a request that cannot be carried by the bounded wire.
	ErrRequestRejected = errors.New("CHANNEL_REQUEST_REJECTED")
	// ErrResponseRejected marks a peer reply that fails the strict envelope contract.
	ErrResponseRejected = errors.New("CHANNEL_RESPONSE_REJECTED")
)

// ServiceError is one bounded transport error frame returned by the service peer.
type ServiceError struct {
	Code      string
	Retryable bool
}

// Error returns the bounded peer code.
func (e *ServiceError) Error() string { return "SERVICE:" + e.Code }

type requestFrame struct {
	V           int               `json:"v"`
	Op          string            `json:"op"`
	SessionMode string            `json:"session_mode,omitempty"`
	RequestID   string            `json:"request_id"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Query       string            `json:"query"`
	Headers     map[string]string `json:"headers"`
	BodyB64     string            `json:"body_b64"`
}

type responseFrame struct {
	V         int               `json:"v"`
	RequestID string            `json:"request_id"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	BodyB64   string            `json:"body_b64"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type errorFrame struct {
	V         int         `json:"v"`
	RequestID string      `json:"request_id"`
	Error     errorDetail `json:"error"`
}

var requestHeaderAllowed = map[string]bool{
	"Authorization":   true,
	"Content-Type":    true,
	"X-Request-ID":    true,
	"Idempotency-Key": true,
}

var responseHeaderAllowed = map[string]bool{
	"Content-Type": true,
	"Retry-After":  true,
}

var serviceOperationID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Doer implements accountaccess.Doer over the service channel. It accepts the
// absolute URLs the existing client builds (base_url + path) only for the trusted
// configured origin and only for the fixed mobile API allowlist; no redirect,
// arbitrary host, proxy or unknown header is carried.
type Doer struct {
	Base    *url.URL
	Seeds   *Store
	Channel *Channel
	Observe func(string)
}

func (d *Doer) stage(name string) {
	if d != nil && d.Observe != nil {
		func() { defer func() { _ = recover() }(); d.Observe(name) }()
	}
}

// Close releases the bounded service connection owned by this mobile attempt.
func (d *Doer) Close() error {
	if d == nil || d.Channel == nil {
		return nil
	}
	return d.Channel.Close()
}

// NewDoer validates the trusted base URL and wires the seed store and channel.
func NewDoer(baseURL string, seeds *Store, channel *Channel) (*Doer, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrOriginRejected
	}
	if parsed.Scheme != "https" && !loopbackHost(parsed.Hostname()) {
		return nil, ErrOriginRejected
	}
	if seeds == nil || channel == nil {
		return nil, ErrRequestRejected
	}
	return &Doer{Base: parsed, Seeds: seeds, Channel: channel}, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Do performs exactly one bounded service exchange. There is no retry here and no
// direct-HTTPS fallback when a service seed is configured.
func (d *Doer) Do(req *http.Request) (*http.Response, error) {
	if d == nil || req == nil || req.URL == nil || d.Base == nil || d.Seeds == nil || d.Channel == nil {
		return nil, ErrRequestRejected
	}
	dispatched := false
	defer func() {
		if !dispatched {
			d.stage("LOCAL_REJECTED")
		}
	}()
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		return nil, ErrRequestRejected
	}
	if req.URL.User != nil || req.URL.Opaque != "" || req.URL.Fragment != "" || !sameOrigin(d.Base, req.URL) {
		return nil, ErrOriginRejected
	}
	path := req.URL.EscapedPath()
	query := req.URL.RawQuery
	if path == "" || len(path)+len(query) > maxPathQuery ||
		strings.ContainsAny(path, "\\?#%") || strings.Contains(path, "..") || strings.Contains(path, "//") {
		return nil, ErrPathRejected
	}
	if strings.ContainsAny(query, "\r\n") {
		return nil, ErrPathRejected
	}
	if !pathAllowed(req.Method, path) {
		return nil, ErrPathRejected
	}
	headers, err := requestHeaders(req)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(req)
	if err != nil {
		return nil, err
	}
	seed, _, ok := d.Seeds.Current()
	if !ok {
		return nil, ErrSeedMissing
	}
	id, err := newServiceID()
	if err != nil {
		return nil, ErrTransportFailed
	}
	frame := requestFrame{
		V:           1,
		Op:          "service.http",
		SessionMode: "bounded",
		RequestID:   RequestID(id),
		Method:      req.Method,
		Path:        path,
		Query:       query,
		Headers:     headers,
		BodyB64:     base64.RawURLEncoding.EncodeToString(raw),
	}
	payload, err := json.Marshal(frame)
	if err != nil || len(payload) > wlwire.ServiceMaxFrame {
		return nil, ErrRequestRejected
	}
	// Self-check the emitted frame against the same bounded contract the service
	// validator applies, so a locally invalid request never reaches the peer.
	if _, err := validateRequest(payload, id); err != nil {
		return nil, err
	}
	dispatched = true
	exchangeCtx := WithRequestClass(req.Context(), requestClassForPath(path))
	body, err := d.Channel.Exchange(exchangeCtx, seed, id, payload)
	if err != nil {
		return nil, err
	}
	d.stage("SERVICE_REPLY_BEGIN")
	reply, serviceErr, err := parseServiceReply(frame.RequestID, body)
	if err != nil {
		d.stage("SERVICE_REPLY_FAILED")
		_ = d.Channel.Close()
		return nil, err
	}
	if serviceErr != nil {
		d.stage("SERVICE_ERROR")
		_ = d.Channel.Close()
		return nil, serviceErr
	}
	d.stage("SERVICE_REPLY_OK")
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", reply.status, http.StatusText(reply.status)),
		StatusCode:    reply.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        reply.headers,
		Body:          io.NopCloser(bytes.NewReader(reply.body)),
		ContentLength: int64(len(reply.body)),
		Request:       req,
	}, nil
}

// requestClassForPath maps an allowlisted mobile path to a fixed trace class. It never
// carries payload; unknown paths collapse to OTHER.
func requestClassForPath(path string) string {
	switch path {
	case "/api/mobile/v1/auth/challenge", "/api/mobile/v1/auth/session", "/api/mobile/v1/installations":
		return "AUTH"
	case "/api/mobile/v1/me":
		return "ME"
	case "/api/mobile/v1/usage":
		return "USAGE"
	case "/api/mobile/v1/gateways":
		return "GATEWAYS"
	case "/api/mobile/v1/access/sync":
		return "ACCESS_SYNC"
	case "/api/mobile/v1/registration/telegram/link":
		return "REG_LINK"
	case "/api/mobile/v1/trial/activate":
		return "TRIAL"
	case "/api/mobile/v1/plans":
		return "PLANS"
	case "/api/mobile/v1/quotes":
		return "QUOTES"
	case "/api/mobile/v1/payments":
		return "PAYMENTS"
	case "/api/mobile/v1/announcements":
		return "ANNOUNCEMENTS"
	default:
		if strings.HasPrefix(path, mobilePaymentsPrefix) {
			if strings.HasSuffix(path, "/checkout-session") {
				return "CHECKOUT"
			}
			return "PAYMENTS"
		}
		if strings.HasPrefix(path, mobileAnnouncementsPrefix) && strings.HasSuffix(path, "/read") {
			return "ANNOUNCEMENT_READ"
		}
		return "OTHER"
	}
}

// pathAllowed mirrors the accepted service request allowlist exactly: only the
// existing mobile-v1 operations are reachable. The future connect-intent path
// (/api/mobile/v1/onboarding/intents) is present but is not auto-invoked.
func pathAllowed(method, path string) bool {
	switch path {
	case "/api/mobile/v1/auth/challenge", "/api/mobile/v1/auth/session",
		"/api/mobile/v1/installations", "/api/mobile/v1/access/sync",
		"/api/mobile/v1/onboarding/intents", "/api/mobile/v1/registration/telegram/link",
		"/api/mobile/v1/trial/activate", "/api/mobile/v1/quotes", "/api/mobile/v1/payments":
		return method == http.MethodPost
	case "/api/mobile/v1/me", "/api/mobile/v1/gateways", "/api/mobile/v1/plans", "/api/mobile/v1/usage",
		"/api/mobile/v1/announcements":
		return method == http.MethodGet
	}
	// Announcement read-marker dynamic path: exactly one bounded opaque id segment and the
	// fixed "/read" suffix. The host sends the idempotency key as the required header.
	if strings.HasPrefix(path, mobileAnnouncementsPrefix) {
		rest := path[len(mobileAnnouncementsPrefix):]
		if method == http.MethodPost && strings.HasSuffix(rest, "/read") {
			return validAnnouncementPathID(strings.TrimSuffix(rest, "/read"))
		}
		return false
	}
	// Payment family dynamic path: /payments/{id} (GET) and
	// /payments/{id}/checkout-session (POST) with exactly one bounded opaque id
	// segment. Any other shape falls through to the operation-id gate and is rejected.
	if strings.HasPrefix(path, mobilePaymentsPrefix) {
		rest := path[len(mobilePaymentsPrefix):]
		switch {
		case method == http.MethodGet:
			return validPaymentPathID(rest)
		case method == http.MethodPost && strings.HasSuffix(rest, "/checkout-session"):
			return validPaymentPathID(strings.TrimSuffix(rest, "/checkout-session"))
		}
		return false
	}
	const operations = "/api/mobile/v1/operations/"
	if method == http.MethodGet && strings.HasPrefix(path, operations) {
		return serviceOperationID.MatchString(path[len(operations):])
	}
	return false
}

const mobilePaymentsPrefix = "/api/mobile/v1/payments/"

// mobileAnnouncementsPrefix bounds the §11 read-marker dynamic path.
const mobileAnnouncementsPrefix = "/api/mobile/v1/announcements/"

var paymentPathIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// validAnnouncementPathID bounds the /announcements/{id}/read segment the same way as a
// payment id: one unreserved segment of 1..128 chars, no dot-dot and no path tricks.
func validAnnouncementPathID(id string) bool {
	if id == "" || id == "." || id == ".." || strings.Contains(id, "..") {
		return false
	}
	return paymentPathIDPattern.MatchString(id)
}

// validPaymentPathID bounds the /payments/{id} dynamic segment: exactly one path
// segment of 1..128 chars from the unreserved URL set [A-Za-z0-9._~-]. The bare "."
// and ".." segments, any embedded "..", slashes and percent escapes are path tricks
// and are rejected. The Do-level path gate independently rejects '%', '\\', '?', '#',
// '//' and any ".." before this allowlist is consulted.
func validPaymentPathID(id string) bool {
	if id == "" || id == "." || id == ".." || strings.Contains(id, "..") {
		return false
	}
	return paymentPathIDPattern.MatchString(id)
}

// sameOrigin compares scheme and canonical host:port.
func sameOrigin(base, target *url.URL) bool {
	return originOf(base) == originOf(target)
}

func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// requestHeaders carries only the fixed safe header set with unchanged values.
func requestHeaders(req *http.Request) (map[string]string, error) {
	out := map[string]string{}
	total := 0
	for name, values := range req.Header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if !requestHeaderAllowed[canonical] || len(values) != 1 {
			return nil, ErrRequestRejected
		}
		value := values[0]
		if len(value) > maxHeaderValue || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return nil, ErrRequestRejected
		}
		total += len(canonical) + len(value)
		out[canonical] = value
	}
	if total > maxHeaders {
		return nil, ErrRequestRejected
	}
	return out, nil
}

// readBody reads at most the accepted decoded request body bound and closes the body.
func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	_ = req.Body.Close()
	if err != nil || len(raw) > maxRequestBody {
		return nil, ErrRequestRejected
	}
	return raw, nil
}

// validateRequest re-checks one encoded request frame against the bounded contract
// the service peer applies (same shapes, caps and allowlists as the shared codec
// server). It is used as a client-side self-check.
func validateRequest(raw []byte, id wlwire.ID) (requestFrame, error) {
	var frame requestFrame
	if wlwire.StrictJSON(raw, &frame) != nil {
		return frame, ErrRequestRejected
	}
	if frame.V != 1 || frame.Op != "service.http" {
		return frame, ErrRequestRejected
	}
	if frame.SessionMode != "" && frame.SessionMode != "bounded" {
		return frame, ErrRequestRejected
	}
	if !requestIDPattern.MatchString(frame.RequestID) || frame.RequestID != hex.EncodeToString(id[:]) {
		return frame, ErrRequestRejected
	}
	if frame.Method != http.MethodGet && frame.Method != http.MethodPost {
		return frame, ErrRequestRejected
	}
	if frame.Path == "" || len(frame.Path)+len(frame.Query) > maxPathQuery {
		return frame, ErrPathRejected
	}
	if strings.ContainsAny(frame.Path, "\\?#%") || strings.Contains(frame.Path, "..") || strings.Contains(frame.Path, "//") {
		return frame, ErrPathRejected
	}
	if strings.ContainsAny(frame.Query, "\r\n") {
		return frame, ErrPathRejected
	}
	if !pathAllowed(frame.Method, frame.Path) {
		return frame, ErrPathRejected
	}
	if !validRequestHeaders(frame.Headers) {
		return frame, ErrRequestRejected
	}
	if len(frame.BodyB64) > encodedLimit(maxRequestBody) {
		return frame, ErrRequestRejected
	}
	if _, err := decodeBody(frame.BodyB64, maxRequestBody); err != nil {
		return frame, ErrRequestRejected
	}
	return frame, nil
}

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validRequestHeaders(headers map[string]string) bool {
	total := 0
	for name, value := range headers {
		if !requestHeaderAllowed[name] || len(name) == 0 || len(value) > maxHeaderValue {
			return false
		}
		if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return false
		}
		total += len(name) + len(value)
	}
	return total <= maxHeaders
}

type serviceReply struct {
	status  int
	headers http.Header
	body    []byte
}

// parseServiceReply accepts exactly one of the two bounded reply shapes (response or
// error) and validates request_id, status, headers and both body caps. Nothing else
// is ever treated as a successful reply.
func parseServiceReply(requestID string, raw []byte) (serviceReply, *ServiceError, error) {
	var reply serviceReply
	var union map[string]json.RawMessage
	if wlwire.StrictJSON(raw, &union) != nil {
		return reply, nil, ErrResponseRejected
	}
	_, hasStatus := union["status"]
	_, hasError := union["error"]
	switch {
	case hasStatus && !hasError:
		if len(union) != 5 {
			return reply, nil, ErrResponseRejected
		}
		var frame responseFrame
		if wlwire.StrictJSON(raw, &frame) != nil {
			return reply, nil, ErrResponseRejected
		}
		if frame.V != 1 || frame.RequestID != requestID {
			return reply, nil, ErrResponseRejected
		}
		if frame.Status < 100 || frame.Status > 599 {
			return reply, nil, ErrResponseRejected
		}
		headers, err := responseHeaders(frame.Headers)
		if err != nil {
			return reply, nil, err
		}
		if len(frame.BodyB64) > encodedLimit(maxResponseBody) {
			return reply, nil, ErrResponseRejected
		}
		body, err := decodeBody(frame.BodyB64, maxResponseBody)
		if err != nil {
			return reply, nil, ErrResponseRejected
		}
		reply.status, reply.headers, reply.body = frame.Status, headers, body
		return reply, nil, nil
	case hasError && !hasStatus:
		if len(union) != 3 {
			return reply, nil, ErrResponseRejected
		}
		var frame errorFrame
		if wlwire.StrictJSON(raw, &frame) != nil {
			return reply, nil, ErrResponseRejected
		}
		if frame.V != 1 || frame.RequestID != requestID || frame.Error.Code == "" || len(frame.Error.Code) > 64 {
			return reply, nil, ErrResponseRejected
		}
		return reply, &ServiceError{Code: frame.Error.Code, Retryable: frame.Error.Retryable}, nil
	default:
		return reply, nil, ErrResponseRejected
	}
}

func responseHeaders(headers map[string]string) (http.Header, error) {
	out := http.Header{}
	total := 0
	for name, value := range headers {
		if !responseHeaderAllowed[name] || len(value) > maxHeaderValue {
			return nil, ErrResponseRejected
		}
		if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return nil, ErrResponseRejected
		}
		total += len(name) + len(value)
		out.Set(name, value)
	}
	if total > maxHeaders {
		return nil, ErrResponseRejected
	}
	return out, nil
}

// encodedLimit is the accepted encoded upper bound for one raw-URL base64 body.
func encodedLimit(decoded int) int { return (decoded+2)/3*4 + 8 }

func decodeBody(encoded string, limit int) ([]byte, error) {
	if encoded == "" {
		return []byte{}, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("service body over limit")
	}
	return raw, nil
}
