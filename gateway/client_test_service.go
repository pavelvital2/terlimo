// TEST-only service-only channel: bounded fixed-destination API relay over the existing
// DTLS/WRAP transport. The service classifier is a public build-time value (not a secret,
// not a right): it only selects this branch. All authority stays with the backend mobile
// API, which is reached verbatim through a dedicated internal relay. No VPN/WG/GETCONF/
// grant path is reachable from this class.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wg-turn-client/internal/wlwire"
)

// serviceRegistrationLinkPath is the only route with gated per-request return-path
// correlation (STEP036 S3-A). Diagnostic only: it never changes response semantics.
const serviceRegistrationLinkPath = "/api/mobile/v1/registration/telegram/link"

// serviceRegLinkTrace is a secret-free classification of one registration-link
// return path. It carries no body, token, nonce or bearer material.
type serviceRegLinkTrace struct {
	RelayCode        string
	RelayErr         string
	WatcherCancelled bool
	ParseOK          bool
	Status           int
	PayloadLen       int
	WriteErr         string
}

func (t serviceRegLinkTrace) classify() string {
	switch {
	case t.WriteErr != "":
		return "write_error"
	case t.RelayCode != "" || t.RelayErr != "":
		if t.WatcherCancelled {
			return "watcher_cancel"
		}
		return "relay_error"
	case !t.ParseOK:
		return "parse_error"
	default:
		return "written_ok"
	}
}

func (t serviceRegLinkTrace) String() string {
	return "class=" + t.classify() +
		" relay_code=" + strconv.Quote(t.RelayCode) +
		" relay_err=" + strconv.Quote(t.RelayErr) +
		" watcher_cancelled=" + strconv.FormatBool(t.WatcherCancelled) +
		" parse_ok=" + strconv.FormatBool(t.ParseOK) +
		" status=" + strconv.Itoa(t.Status) +
		" payload_len=" + strconv.Itoa(t.PayloadLen) +
		" write_err=" + strconv.Quote(t.WriteErr)
}

func serviceErrCode(err *serviceErrorFrame) string {
	if err == nil {
		return ""
	}
	return err.Error.Code
}

func serviceBoundedErr(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 120 {
		text = text[:120]
	}
	return text
}

// serviceTracePath selects the gated [SVCREG] return-path trace. Diagnostic only; it adds
// logging for exact method+path pairs and never changes validation, limits or deadlines.
func serviceTracePath(method, path string) bool {
	switch path {
	case serviceRegistrationLinkPath:
		return method == "POST"
	case "/api/mobile/v1/gateways":
		return method == "GET"
	case "/api/mobile/v1/access/sync":
		return method == "POST"
	case "/api/mobile/v1/usage":
		return method == "GET"
	}
	return false
}

const (
	serviceMaxRequestBody  = 64 * 1024
	serviceMaxResponseBody = 1024 * 1024
	serviceMaxHeaders      = 8 * 1024
	serviceMaxPathQuery    = 2048
	serviceMaxRequests     = 8
	serviceMaxSources      = 4096
	serviceDefaultGlobal   = 16
	serviceDefaultPerSrc   = 2
	serviceDialTimeout     = 3 * time.Second
	serviceIODeadline      = 15 * time.Second
	serviceAuthDeadline    = 22 * time.Second
	serviceWriteDeadline   = 5 * time.Second
)

var serviceOperationID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var serviceRequestID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func serviceEnabled() bool { return os.Getenv("WL_TEST_SERVICE_ENABLED") == "1" }

func serviceSeedValue() string { return strings.TrimSpace(os.Getenv("WL_TEST_SERVICE_SEED")) }

func serviceSocketPath() string {
	if v := strings.TrimSpace(os.Getenv("WL_TEST_SERVICE_BACKEND_SOCKET")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("WL_TEST_BACKEND_SOCKET"))
}

func serviceEnvInt(name string, def, max int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v <= 0 {
		v = def
	}
	if v > max {
		v = max
	}
	return v
}

type serviceRequest struct {
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

type serviceResponse struct {
	V         int               `json:"v"`
	RequestID string            `json:"request_id"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	BodyB64   string            `json:"body_b64"`
}

type serviceErrorDetail struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type serviceErrorFrame struct {
	V         int                `json:"v"`
	RequestID string             `json:"request_id"`
	Error     serviceErrorDetail `json:"error"`
}

var serviceAllowedHeaders = map[string]bool{
	"Authorization":   true,
	"Content-Type":    true,
	"X-Request-ID":    true,
	"Idempotency-Key": true,
}

var serviceResponseHeaders = map[string]bool{
	"Content-Type": true,
	"Retry-After":  true,
}

// servicePathAllowed reports whether the exact method+path pair is an existing mobile API
// operation. Paths are matched literally (operations/{uuid} by strict UUID) and nothing
// else; there is no arbitrary URL, host, scheme, CONNECT or redirect support.
// serviceBoundedPathID matches the frozen PathId grammar (bounded opaque, no slash/escape).
var serviceBoundedPathID = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

func servicePathAllowed(method, path string) bool {
	switch path {
	case "/api/mobile/v1/auth/challenge", "/api/mobile/v1/installations",
		"/api/mobile/v1/auth/session", "/api/mobile/v1/access/sync",
		"/api/mobile/v1/onboarding/intents", "/api/mobile/v1/registration/telegram/link",
		"/api/mobile/v1/trial/activate":
		return method == "POST"
	case "/api/mobile/v1/me", "/api/mobile/v1/service-seed", "/api/mobile/v1/gateways", "/api/mobile/v1/plans",
		"/api/mobile/v1/usage", "/api/mobile/v1/devices", "/api/mobile/v1/announcements":
		return method == "GET"
	case "/api/mobile/v1/quotes", "/api/mobile/v1/payments":
		return method == "POST"
	}
	const operations = "/api/mobile/v1/operations/"
	if method == "GET" && strings.HasPrefix(path, operations) {
		return serviceOperationID.MatchString(path[len(operations):])
	}
	const payments = "/api/mobile/v1/payments/"
	if method == "GET" && strings.HasPrefix(path, payments) {
		return serviceOperationID.MatchString(path[len(payments):])
	}
	// Announcement read is POST with a strict operation-style UUID and an exact /read suffix
	// (no trailing slash, extra segment, query or path escape; the caller sanitizes those).
	// Checkout session is POST with a bounded opaque path id (contract PathId 1..128); only a
	// canonical generated UUID resolves in storage, but the transport accepts the bounded form.
	const checkout = "/api/mobile/v1/payments/"
	if method == "POST" && strings.HasPrefix(path, checkout) {
		rest := strings.TrimPrefix(path, checkout)
		if !strings.HasSuffix(rest, "/checkout-session") {
			return false
		}
		return serviceBoundedPathID.MatchString(strings.TrimSuffix(rest, "/checkout-session"))
	}
	const announcements = "/api/mobile/v1/announcements/"
	if method == "POST" && strings.HasPrefix(path, announcements) && strings.HasSuffix(path, "/read") {
		mid := strings.TrimSuffix(strings.TrimPrefix(path, announcements), "/read")
		return serviceOperationID.MatchString(mid)
	}
	// Device removal is the only non-GET operation class: DELETE with a strict operation-style
	// UUID and nothing else (no trailing slash, suffix, query or path escape; the caller
	// sanitizes those before this gate).
	const devices = "/api/mobile/v1/devices/"
	if method == "DELETE" && strings.HasPrefix(path, devices) {
		return serviceOperationID.MatchString(strings.TrimPrefix(path, devices))
	}
	return false
}

func serviceValidateHeaders(headers map[string]string) bool {
	total := 0
	for name, value := range headers {
		if !serviceAllowedHeaders[name] {
			return false
		}
		if len(name) == 0 || len(value) > 1024 {
			return false
		}
		if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return false
		}
		total += len(name) + len(value)
	}
	return total <= serviceMaxHeaders
}

func serviceValidateRequest(body []byte, id wlwire.ID) (serviceRequest, string) {
	var req serviceRequest
	if wlwire.StrictJSON(body, &req) != nil {
		return req, "SERVICE_BAD_FRAME"
	}
	if req.V != 1 || req.Op != "service.http" {
		return req, "SERVICE_BAD_FRAME"
	}
	if req.SessionMode != "" && req.SessionMode != "bounded" {
		return req, "SERVICE_BAD_FRAME"
	}
	if !serviceRequestID.MatchString(req.RequestID) || req.RequestID != hex.EncodeToString(id[:]) {
		return req, "SERVICE_BAD_FRAME"
	}
	if req.Method != "GET" && req.Method != "POST" && req.Method != "DELETE" {
		return req, "SERVICE_BAD_METHOD"
	}
	if req.Path == "" || len(req.Path)+len(req.Query) > serviceMaxPathQuery {
		return req, "SERVICE_BAD_PATH"
	}
	if strings.ContainsAny(req.Path, "\\?#%") || strings.Contains(req.Path, "..") || strings.Contains(req.Path, "//") {
		return req, "SERVICE_BAD_PATH"
	}
	if strings.ContainsAny(req.Query, "\r\n") {
		return req, "SERVICE_BAD_PATH"
	}
	if !servicePathAllowed(req.Method, req.Path) {
		return req, "SERVICE_PATH_DENIED"
	}
	if !serviceValidateHeaders(req.Headers) {
		return req, "SERVICE_BAD_HEADERS"
	}
	if len(req.BodyB64) > serviceEncodedLimit(serviceMaxRequestBody) {
		return req, "SERVICE_BAD_FRAME"
	}
	if raw, err := decodeServiceBody(req.BodyB64, serviceMaxRequestBody); err != nil || len(raw) > serviceMaxRequestBody {
		return req, "SERVICE_BAD_FRAME"
	}
	return req, ""
}

func serviceEncodedLimit(decoded int) int { return (decoded+2)/3*4 + 8 }

func decodeServiceBody(encoded string, limit int) ([]byte, error) {
	raw, err := wlwireB64Decode(encoded)
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("service body over limit")
	}
	return raw, nil
}

var serviceB64 = base64.RawURLEncoding.Strict()

func wlwireB64Decode(s string) ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}
	return serviceB64.DecodeString(s)
}

type serviceLimiter struct {
	global    chan struct{}
	mu        sync.Mutex
	perSource map[string]int
	perSrcMax int
}

var (
	serviceLimiterOnce sync.Once
	serviceLimiterInst *serviceLimiter
)

func serviceGlobalLimiter() *serviceLimiter {
	serviceLimiterOnce.Do(func() {
		serviceLimiterInst = &serviceLimiter{
			global:    make(chan struct{}, serviceEnvInt("WL_TEST_SERVICE_MAX_GLOBAL", serviceDefaultGlobal, 128)),
			perSource: make(map[string]int),
			perSrcMax: serviceEnvInt("WL_TEST_SERVICE_MAX_PER_SOURCE", serviceDefaultPerSrc, 16),
		}
	})
	return serviceLimiterInst
}

func (l *serviceLimiter) acquire(source string) bool {
	select {
	case l.global <- struct{}{}:
	default:
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.perSource) >= serviceMaxSources {
		if _, known := l.perSource[source]; !known {
			<-l.global
			return false
		}
	}
	if l.perSource[source] >= l.perSrcMax {
		<-l.global
		return false
	}
	l.perSource[source]++
	return true
}

func (l *serviceLimiter) release(source string) {
	l.mu.Lock()
	if l.perSource[source] > 1 {
		l.perSource[source]--
	} else {
		delete(l.perSource, source)
	}
	l.mu.Unlock()
	<-l.global
}

// inFlight is used by tests to prove prompt slot release.
func (l *serviceLimiter) inFlight() int { return len(l.global) }

func serviceSource(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil {
		return host
	}
	return c.RemoteAddr().String()
}

func serviceReadFrame(c net.Conn) (wlwire.ID, []byte, error) {
	return serviceReadFrameDiag(c, nil)
}

func serviceReadFrameDiag(c net.Conn, diag *serviceFrameDiag) (wlwire.ID, []byte, error) {
	var assembler wlwire.ServiceAssembler
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, 32+wlwire.ServiceFragment+1)
	for {
		diag.emit("read_begin", "WAIT", "NONE", 0)
		n, err := c.Read(buf)
		if diag != nil {
			diag.reads++
		}
		diag.emit("read_end", serviceFrameDiagErrIfOn(diag, err), "NONE", n)
		if err != nil {
			diag.emit("terminal", serviceFrameDiagErrIfOn(diag, err), "READ_ERROR", n)
			return wlwire.ID{}, nil, err
		}
		if n < 6 || buf[5] != 0 {
			diag.emit("terminal", "INVALID", "DIRECTION_OR_SHORT", n)
			return wlwire.ID{}, nil, wlwire.ErrMessage
		}
		id, body, err := assembler.Add(buf[:n], time.Now())
		if err != nil {
			diag.emit("assembler", "INVALID", "CODEC_ERROR", n)
		} else {
			diag.added(id, buf[:n], body != nil)
		}
		if err != nil || body != nil {
			result := "COMPLETE"
			if err != nil {
				result = "INVALID"
			}
			diag.emit("terminal", result, "CODEC_RETURN", n)
			return id, body, err
		}
	}
}

func serviceWriteFrame(c net.Conn, id wlwire.ID, payload []byte) error {
	frames, err := wlwire.ServiceFrames(id, true, payload)
	if err != nil {
		return err
	}
	_ = c.SetWriteDeadline(time.Now().Add(serviceWriteDeadline))
	defer c.SetWriteDeadline(time.Time{})
	for _, frame := range frames {
		if _, err = c.Write(frame); err != nil {
			return err
		}
	}
	return nil
}

func serviceWriteError(c net.Conn, id wlwire.ID, code string, retryable bool) {
	body, err := json.Marshal(serviceErrorFrame{
		V:         1,
		RequestID: hex.EncodeToString(id[:]),
		Error:     serviceErrorDetail{Code: code, Retryable: retryable},
	})
	if err != nil || len(body) > wlwire.ServiceMaxFrame {
		return
	}
	_ = serviceWriteFrame(c, id, body)
}

func serviceAuthRequest(req serviceRequest) bool {
	return req.Method == "POST" && (req.Path == "/api/mobile/v1/auth/challenge" || req.Path == "/api/mobile/v1/auth/session")
}

// serviceRelay forwards one validated request to the fixed AF_UNIX relay socket and
// returns either a validated response or a bounded transport error frame. The relay target
// is fixed by configuration; the client never chooses a destination.
func serviceRelay(ctx context.Context, req serviceRequest) (serviceResponse, *serviceErrorFrame, string, bool) {
	dialer := net.Dialer{Timeout: serviceDialTimeout}
	return serviceRelayWithDial(ctx, req, dialer.DialContext)
}

// The fixture seam uses the same runtime Unix DialContext and existing budgets.
func serviceRelayWithDial(ctx context.Context, req serviceRequest, dial func(context.Context, string, string) (net.Conn, error)) (serviceResponse, *serviceErrorFrame, string, bool) {
	entered := time.Now()
	deadline := entered.Add(serviceIODeadline)
	if serviceAuthRequest(req) {
		deadline = entered.Add(serviceAuthDeadline)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
		if parentDeadline, ok := ctx.Deadline(); ok {
			deadline = parentDeadline
		}
	}
	var ioStarted time.Time
	var resp serviceResponse
	if ctxErr := ctx.Err(); ctxErr != nil {
		serviceUnixTerminal(ctxErr, req.RequestID, "PRECTX", ctxErr, entered, ioStarted)
		return resp, nil, "SERVICE_UNAVAILABLE", true
	}
	socket := serviceSocketPath()
	if socket == "" || socket[0] != '/' {
		serviceUnixTerminal(ctx.Err(), req.RequestID, "SOCKET_PATH", nil, entered, ioStarted)
		return resp, nil, "SERVICE_UNAVAILABLE", true
	}
	up, err := dial(ctx, "unix", socket)
	if err != nil {
		serviceUnixTerminal(ctx.Err(), req.RequestID, "DIAL", err, entered, ioStarted)
		return resp, nil, "SERVICE_UNAVAILABLE", true
	}
	defer up.Close()
	// Terminal cancellation (including a client disconnect) closes this per-call connection;
	// it cannot be overwritten by a later deadline reset. The relay socket is owned by this
	// single call.
	if !serviceAuthRequest(req) {
		deadline = time.Now().Add(serviceIODeadline)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = up.Close() })
	defer stopCancel()
	if deadlineErr := up.SetDeadline(deadline); deadlineErr == nil {
		ioStarted = time.Now()
	}
	if err = json.NewEncoder(up).Encode(req); err != nil {
		serviceUnixTerminal(ctx.Err(), req.RequestID, "ENCODE", err, entered, ioStarted)
		return resp, nil, "SERVICE_UNAVAILABLE", true
	}
	// Exactly one newline-terminated frame per per-call Unix connection, bounded by
	// ServiceMaxFrame. Buffered trailing bytes are rejected; bytes that arrive later cannot
	// form a second request or a continuation because the socket is closed after the frame.
	reader := bufio.NewReaderSize(up, wlwire.ServiceMaxFrame+1)
	line, err := reader.ReadSlice('\n')
	switch {
	case err == nil:
	case errors.Is(err, bufio.ErrBufferFull):
		return resp, nil, "SERVICE_BAD_RESPONSE", true
	default:
		serviceUnixTerminal(ctx.Err(), req.RequestID, "READSLICE", err, entered, ioStarted)
		return resp, nil, "SERVICE_UNAVAILABLE", true
	}
	line = line[:len(line)-1]
	if len(line) == 0 || len(line) > wlwire.ServiceMaxFrame || reader.Buffered() > 0 {
		return resp, nil, "SERVICE_BAD_RESPONSE", true
	}
	return serviceParseReply(line, req.RequestID)
}

// serviceParseReply accepts exactly one of the two bounded reply shapes (response or
// error) and validates request_id and all shape-specific caps.
func serviceParseReply(raw []byte, requestID string) (serviceResponse, *serviceErrorFrame, string, bool) {
	var resp serviceResponse
	var union map[string]json.RawMessage
	if wlwire.StrictJSON(raw, &union) != nil {
		return resp, nil, "SERVICE_BAD_RESPONSE", true
	}
	_, hasStatus := union["status"]
	_, hasError := union["error"]
	switch {
	case hasStatus && !hasError:
		if len(union) != 5 {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		if wlwire.StrictJSON(raw, &resp) != nil {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		if resp.V != 1 || resp.RequestID != requestID {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		if resp.Status < 100 || resp.Status > 599 {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		total := 0
		for name, value := range resp.Headers {
			if !serviceResponseHeaders[name] || len(value) > 1024 {
				return resp, nil, "SERVICE_BAD_RESPONSE", true
			}
			if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
				return resp, nil, "SERVICE_BAD_RESPONSE", true
			}
			total += len(name) + len(value)
		}
		if total > serviceMaxHeaders {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		if len(resp.BodyB64) > serviceEncodedLimit(serviceMaxResponseBody) {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		if _, err := decodeServiceBody(resp.BodyB64, serviceMaxResponseBody); err != nil {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		return resp, nil, "", false
	case hasError && !hasStatus:
		if len(union) != 3 {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		var errFrame serviceErrorFrame
		if wlwire.StrictJSON(raw, &errFrame) != nil {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		if errFrame.V != 1 || errFrame.RequestID != requestID || errFrame.Error.Code == "" || len(errFrame.Error.Code) > 64 {
			return resp, nil, "SERVICE_BAD_RESPONSE", true
		}
		return resp, &errFrame, "", false
	default:
		return resp, nil, "SERVICE_BAD_RESPONSE", true
	}
}

// clientTestServiceServe handles the service-only class. The admission slot is taken
// before any read/assembly so the public classifier cannot trigger unbounded concurrent
// reassembly, and it is held for the whole session and released on every return path. It
// has no path to WG/peer/GETCONF/grant or the evidence start handler.
func clientTestServiceServe(ctx context.Context, c net.Conn, identity accessIdentity) {
	clientTestServiceServeGen(ctx, c, identity, 0)
}

func serviceReadErrClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, wlwire.ErrMessage):
		return "BAD_HEADER"
	case errors.Is(err, io.EOF):
		return "EOF"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "TIMEOUT"
	}
	return "OTHER"
}

// clientTestServiceServeGen is clientTestServiceServe with a connection generation id for
// gated diagnostic correlation (gen 0 when called outside the accept loop).

// serviceFrameClass maps a validated service request to a fixed, secret-free diagnostic class.
// Unknown operations stay "OTHER"; invalid frames are classed separately with their bounded code.
func serviceFrameClass(path, code string) string {
	if code != "" {
		return "invalid"
	}
	switch {
	case path == "/api/mobile/v1/auth/challenge" || path == "/api/mobile/v1/auth/session":
		return "AUTH"
	case path == "/api/mobile/v1/me":
		return "ME"
	case path == "/api/mobile/v1/gateways":
		return "GATEWAYS"
	case path == "/api/mobile/v1/usage":
		return "USAGE"
	case path == "/api/mobile/v1/access/sync":
		return "ACCESS"
	case path == "/api/mobile/v1/plans" || path == "/api/mobile/v1/quotes" || path == "/api/mobile/v1/payments" ||
		strings.HasPrefix(path, "/api/mobile/v1/payments/"):
		return "PAYMENTS"
	case path == "/api/mobile/v1/installations":
		return "ENROLL"
	case strings.HasPrefix(path, "/api/mobile/v1/onboarding/"):
		return "ONBOARDING"
	case strings.HasPrefix(path, "/api/mobile/v1/registration/"):
		return "REGLINK"
	case path == "/api/mobile/v1/trial/activate":
		return "TRIAL"
	case path == "/api/mobile/v1/devices" || strings.HasPrefix(path, "/api/mobile/v1/devices/"):
		return "DEVICES"
	case path == "/api/mobile/v1/announcements" || strings.HasPrefix(path, "/api/mobile/v1/announcements/"):
		return "ANNOUNCEMENTS"
	case strings.HasPrefix(path, "/api/mobile/v1/operations/"):
		return "OPERATION"
	default:
		return "OTHER"
	}
}

// serviceBoundedMethod keeps the diagnostic method field to the fixed allowlist; anything else
// (including malformed or hostile frames) is reported as INVALID without echoing raw bytes.
func serviceBoundedMethod(method string) string {
	switch method {
	case "GET", "POST", "DELETE":
		return method
	default:
		return "INVALID"
	}
}

// serviceBoundedCode keeps diagnostic reason codes to the known service allowlist; unknown
// backend/relay strings are never echoed raw.
func serviceBoundedCode(code string) string {
	switch code {
	case "":
		return ""
	case "SERVICE_BAD_FRAME", "SERVICE_BAD_METHOD", "SERVICE_BAD_PATH", "SERVICE_PATH_DENIED",
		"SERVICE_BAD_HEADERS", "SERVICE_UNAVAILABLE", "SERVICE_BAD_RESPONSE", "SERVICE_BUSY",
		"EVIDENCE_BAD_ENVELOPE", "EVIDENCE_REGISTRY_UNKNOWN", "EVIDENCE_REGISTRY_AMBIGUOUS",
		"EVIDENCE_ENV_MISMATCH", "EVIDENCE_AUTH_REQUIRED":
		return code
	default:
		return "OTHER"
	}
}

// serviceTimeLog emits one bounded timing mark for a service-only exchange. Fixed classes and
// counters only; never wire tokens, passwords, bodies or raw error strings.
func serviceTimeLog(gen int64, seq int, class, phase, extra string, elapsed time.Duration) {
	if extra != "" {
		log.Printf("[SVCTIME] %s gen=%d seq=%d class=%s %s utc_ms=%d elapsed_ms=%d",
			phase, gen, seq, class, extra, time.Now().UnixMilli(), elapsed.Milliseconds())
		return
	}
	log.Printf("[SVCTIME] %s gen=%d seq=%d class=%s utc_ms=%d elapsed_ms=%d",
		phase, gen, seq, class, time.Now().UnixMilli(), elapsed.Milliseconds())
}

func clientTestServiceServeGen(ctx context.Context, c net.Conn, identity accessIdentity, gen int64) {
	if !identity.isService {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if !serviceEnabled() {
		return
	}
	log.Printf("[SVCREG] service_enter gen=%d source=%s utc=%s", gen, serviceSource(c),
		time.Now().UTC().Format(time.RFC3339Nano))
	defer func() {
		log.Printf("[SVCREG] service_exit gen=%d source=%s utc=%s", gen, serviceSource(c),
			time.Now().UTC().Format(time.RFC3339Nano))
	}()
	serviceSessionStarted := time.Now()
	limiter := serviceGlobalLimiter()
	source := serviceSource(c)
	if !limiter.acquire(source) {
		return
	}
	defer limiter.release(source)
	// Terminal cancellation closes this session-owned connection; it cannot be overwritten by
	// the per-read/per-write deadline resets below. The connection is not shared.
	sessionDiag := newServiceFrameDiag(serviceFrameCapture, gen, 0)
	stopCancel := context.AfterFunc(ctx, func() {
		sessionDiag.emit("session_close", "REQUESTED", "CTX_CANCEL", 0)
		_ = c.Close()
	})
	defer stopCancel()
	for i := 0; i < serviceMaxRequests; i++ {
		if ctx.Err() != nil {
			return
		}
		diag := newServiceFrameDiag(serviceFrameCapture, gen, i+1)
		id, body, err := serviceReadFrameDiag(c, diag)
		if err != nil {
			log.Printf("[SVCREG] read_end gen=%d source=%s class=%s utc=%s",
				gen, serviceSource(c), serviceReadErrClass(err),
				time.Now().UTC().Format(time.RFC3339Nano))
			return
		}
		req, code := serviceValidateRequest(body, id)
		frameClass := serviceFrameClass(req.Path, code)
		frameSeq := i + 1
		serviceTimeLog(gen, frameSeq, frameClass, "recv",
			fmt.Sprintf("method=%s code=%q", serviceBoundedMethod(req.Method), serviceBoundedCode(code)),
			time.Since(serviceSessionStarted))
		traceReg := serviceTracePath(req.Method, req.Path)
		requestID := hex.EncodeToString(id[:])
		startedAt := time.Now()
		if traceReg {
			log.Printf("[SVCREG] begin request_id=%s method=%s path=%s session_bounded=%t validation_code=%q utc=%s",
				requestID, req.Method, req.Path, req.SessionMode == "bounded", code,
				startedAt.UTC().Format(time.RFC3339Nano))
		}
		if code != "" {
			if traceReg {
				log.Printf("[SVCREG] reject request_id=%s method=%s path=%s validation_code=%q close_total_ms=%d utc=%s",
					requestID, req.Method, req.Path, code, time.Since(startedAt).Milliseconds(),
					time.Now().UTC().Format(time.RFC3339Nano))
			}
			serviceTimeLog(gen, frameSeq, frameClass, "write_begin",
				fmt.Sprintf("kind=error code=%q", serviceBoundedCode(code)), time.Since(serviceSessionStarted))
			serviceWriteError(c, id, code, false)
			serviceTimeLog(gen, frameSeq, frameClass, "write_end",
				fmt.Sprintf("kind=error code=%q result=attempted", serviceBoundedCode(code)), time.Since(serviceSessionStarted))
			return
		}
		if ctx.Err() != nil {
			if traceReg {
				log.Printf("[SVCREG] close request_id=%s path=%s reason=ctx_cancelled close_total_ms=%d utc=%s",
					requestID, req.Path, time.Since(startedAt).Milliseconds(),
					time.Now().UTC().Format(time.RFC3339Nano))
			}
			return
		}
		// Abort an in-flight relay call as soon as the peer session goes away; the client is
		// not pipelining a next request while a response is outstanding. The watcher carries
		// no independent deadline: serviceReadFrame already reset the read deadline to zero,
		// and the watcher is unblocked explicitly right after the bounded relay call, so it
		// can neither fire early nor extend the existing relay IO budget.
		_ = c.SetReadDeadline(time.Time{})
		relayCtx, cancelRelay := context.WithCancel(ctx)
		var authTimer *time.Timer
		if serviceAuthRequest(req) {
			// One absolute budget includes Unix dial/IO and delivery; no chunk/retry reset.
			deadline := startedAt.Add(serviceAuthDeadline)
			cancelRelay()
			relayCtx, cancelRelay = context.WithDeadline(ctx, deadline)
			authTimer = time.AfterFunc(time.Until(deadline), func() { _ = c.Close() })
			defer authTimer.Stop() // error paths return from the session
		}
		watchDone := make(chan struct{})
		watcherCancelled := false
		go func() {
			defer close(watchDone)
			if serviceWatcherRead(c, diag) {
				watcherCancelled = true
			}
			cancelRelay()
		}()
		bounded := req.SessionMode == "bounded"
		req.SessionMode = "" // transport mode is not part of the backend HTTP request
		relayStartedAt := time.Now()
		serviceTimeLog(gen, frameSeq, frameClass, "fwd_begin", "", time.Since(serviceSessionStarted))
		resp, relayErr, relayCode, retryable := serviceRelay(relayCtx, req)
		relayDur := time.Since(relayStartedAt)
		serviceTimeLog(gen, frameSeq, frameClass, "fwd_end",
			fmt.Sprintf("relay_code=%q relay_err=%q status=%d dur_ms=%d",
				serviceBoundedCode(relayCode), serviceBoundedCode(serviceErrCode(relayErr)),
				resp.Status, relayDur.Milliseconds()),
			time.Since(serviceSessionStarted))
		diag.emit("watcher_unblock", "REQUESTED", "RELAY_DONE_DEADLINE", 0)
		_ = c.SetReadDeadline(time.Now())
		<-watchDone
		cancelRelay()
		_ = c.SetReadDeadline(time.Time{})
		trace := serviceRegLinkTrace{
			RelayCode:        relayCode,
			RelayErr:         serviceErrCode(relayErr),
			WatcherCancelled: watcherCancelled,
			ParseOK:          relayErr == nil && relayCode == "",
			Status:           resp.Status,
		}
		if traceReg {
			log.Printf("[SVCREG] relay request_id=%s dur_ms=%d %s utc=%s",
				requestID, relayDur.Milliseconds(), trace.String(), time.Now().UTC().Format(time.RFC3339Nano))
		}
		if relayErr != nil {
			if traceReg {
				log.Printf("[SVCREG] reply request_id=%s %s close_total_ms=%d utc=%s",
					requestID, trace.String(), time.Since(startedAt).Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano))
			}
			serviceTimeLog(gen, frameSeq, frameClass, "write_begin",
				fmt.Sprintf("kind=error code=%q", serviceBoundedCode(relayErr.Error.Code)), time.Since(serviceSessionStarted))
			serviceWriteError(c, id, relayErr.Error.Code, relayErr.Error.Retryable)
			serviceTimeLog(gen, frameSeq, frameClass, "write_end",
				fmt.Sprintf("kind=error code=%q result=attempted", serviceBoundedCode(relayErr.Error.Code)), time.Since(serviceSessionStarted))
			return
		}
		if relayCode != "" {
			if traceReg {
				log.Printf("[SVCREG] reply request_id=%s %s close_total_ms=%d utc=%s",
					requestID, trace.String(), time.Since(startedAt).Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano))
			}
			serviceTimeLog(gen, frameSeq, frameClass, "write_begin",
				fmt.Sprintf("kind=error code=%q", serviceBoundedCode(relayCode)), time.Since(serviceSessionStarted))
			serviceWriteError(c, id, relayCode, retryable)
			serviceTimeLog(gen, frameSeq, frameClass, "write_end",
				fmt.Sprintf("kind=error code=%q result=attempted", serviceBoundedCode(relayCode)), time.Since(serviceSessionStarted))
			return
		}
		payload, err := json.Marshal(resp)
		if err != nil || len(payload) > wlwire.ServiceMaxFrame {
			if traceReg {
				trace.ParseOK = false
				log.Printf("[SVCREG] reply request_id=%s %s close_total_ms=%d utc=%s",
					requestID, trace.String(), time.Since(startedAt).Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano))
			}
			serviceTimeLog(gen, frameSeq, frameClass, "write_begin",
				"kind=error code=\"SERVICE_BAD_RESPONSE\"", time.Since(serviceSessionStarted))
			serviceWriteError(c, id, "SERVICE_BAD_RESPONSE", true)
			serviceTimeLog(gen, frameSeq, frameClass, "write_end",
				"kind=error code=\"SERVICE_BAD_RESPONSE\" result=attempted", time.Since(serviceSessionStarted))
			return
		}
		trace.PayloadLen = len(payload)
		if ctx.Err() != nil {
			serviceTimeLog(gen, frameSeq, frameClass, "close",
				"reason=ctx_cancelled_after_relay", time.Since(serviceSessionStarted))
			if traceReg {
				log.Printf("[SVCREG] close request_id=%s path=%s reason=ctx_cancelled_after_relay close_total_ms=%d utc=%s",
					requestID, req.Path, time.Since(startedAt).Milliseconds(),
					time.Now().UTC().Format(time.RFC3339Nano))
			}
			return
		}
		serviceTimeLog(gen, frameSeq, frameClass, "write_begin", "kind=response", time.Since(serviceSessionStarted))
		if err = serviceWriteFrame(c, id, payload); err != nil {
			serviceTimeLog(gen, frameSeq, frameClass, "write_end",
				fmt.Sprintf("kind=response err=%q", serviceBoundedErr(err)), time.Since(serviceSessionStarted))
			if traceReg {
				trace.WriteErr = serviceBoundedErr(err)
				log.Printf("[SVCREG] reply request_id=%s %s close_total_ms=%d utc=%s",
					requestID, trace.String(), time.Since(startedAt).Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano))
			}
			return
		}
		serviceTimeLog(gen, frameSeq, frameClass, "write_end", "kind=response", time.Since(serviceSessionStarted))
		if traceReg {
			log.Printf("[SVCREG] reply request_id=%s %s close_total_ms=%d utc=%s",
				requestID, trace.String(), time.Since(startedAt).Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano))
		}
		if authTimer != nil {
			authTimer.Stop()
		}
		// Keep the original one-shot release behavior unless the client explicitly
		// requests a bounded sequential session. The loop caps that session at eight
		// requests and serviceReadFrame bounds idle time to ten seconds.
		if !bounded {
			return
		}
	}
}

// configureServiceClassifier registers the public service classifier (if configured) and
// fails closed on any collision with an active wrap key. Called before the initial keyset
// refresh so SetPasswords/AddPassword also re-check the collision on every refresh.
func configureServiceClassifier() error {
	seed := serviceSeedValue()
	if seed == "" {
		if serviceEnabled() {
			return errors.New("WL_TEST_SERVICE_ENABLED requires WL_TEST_SERVICE_SEED")
		}
		return serverWrapKeys.SetServiceClassifier("")
	}
	return serverWrapKeys.SetServiceClassifier(seed)
}

// SetServiceClassifier installs or clears the public service classifier.
func (s *wrapKeyStore) SetServiceClassifier(seed string) error {
	if seed == "" {
		s.mu.Lock()
		if s.serviceKey != nil {
			zeroBytes(s.serviceKey)
		}
		s.serviceKey, s.serviceSeed, s.serviceHash = nil, "", ""
		s.mu.Unlock()
		return nil
	}
	key, err := deriveWrapKey(seed)
	if err != nil {
		return err
	}
	hash := wrapKeyID(seed)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.entries {
		if entry.identity.password == seed || entry.identity.id == "pass:"+hash || entry.identity.id == "svc:"+hash {
			zeroBytes(key)
			return errors.New("service classifier collides with an active wrap key")
		}
	}
	if s.serviceKey != nil {
		zeroBytes(s.serviceKey)
	}
	s.serviceKey, s.serviceSeed, s.serviceHash = key, seed, hash
	return nil
}

func (s *wrapKeyStore) serviceAccessIdentity() accessIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.serviceKey == nil {
		return accessIdentity{}
	}
	return accessIdentity{id: "svc:" + s.serviceHash, password: s.serviceSeed, isService: true}
}

// serviceCollisionLocked reports whether any candidate password collides with the service
// classifier. Callers hold s.mu.
func (s *wrapKeyStore) serviceCollisionLocked(passwords ...string) bool {
	if s.serviceHash == "" {
		return false
	}
	for _, password := range passwords {
		if password == "" {
			continue
		}
		if password == s.serviceSeed || wrapKeyID(password) == s.serviceHash {
			return true
		}
	}
	return false
}

// ServiceCollision lets pre-persist callers fail closed before a DB mutation whose keyset
// refresh would otherwise be rejected (avoiding a saved-but-inactive credential).
func (s *wrapKeyStore) ServiceCollision(password string) bool {
	if password == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.serviceCollisionLocked(password)
}
