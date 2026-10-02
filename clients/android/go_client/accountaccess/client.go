package accountaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// TokenSource supplies the current session bearer. Same-subject bearer refresh must
// not re-key the revision/digest domain; the coordinator keeps the baseline.
type TokenSource interface {
	Bearer(ctx context.Context) (string, error)
}

// Doer is the injectable HTTP transport (net/http in production, a test server in tests).
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a bounded mobile-v1 HTTP client. It performs no retry of its own:
// the coordinator is the single retry/persist owner.
type Client struct {
	BaseURL string
	HTTP    Doer
	Tokens  TokenSource
	// OnRequest/OnResponse are optional, secret-free observation hooks of the single
	// request builder. OnRequest fires with status 0 immediately before the wire call,
	// after bearer resolution; OnResponse fires with the HTTP status, or 0 when the
	// transport failed before any status existed. They never change behavior, never
	// receive a body/URL/credential and are nil-safe.
	OnRequest  func(path string, status int)
	OnResponse func(path string, status int)
}

// MeResponseStage maps one /me wire outcome to the fixed cyclestage boundary token of the
// host mirror: transport failure (status 0) is TRANSPORT, 2xx/4xx/5xx keep their class and
// any other status is OTHER. It marks the wire response READ boundary (the response
// returned by the single request builder, before strict decode); the later
// AFTER_ME_READ runner marker is emitted after Coordinator.Refresh returns (post-decode),
// so the gap between the two is decode/coordinator post-processing. Carries no status
// text, URL or body.
func MeResponseStage(status int) string {
	switch {
	case status == 0:
		return "ME_RESPONSE_TRANSPORT"
	case status >= 200 && status < 300:
		return "ME_RESPONSE_2XX"
	case status >= 400 && status < 500:
		return "ME_RESPONSE_4XX"
	case status >= 500 && status < 600:
		return "ME_RESPONSE_5XX"
	default:
		return "ME_RESPONSE_OTHER"
	}
}

// GatewayRequestStage maps one /gateways wire outcome to the fixed cyclestage terminal
// token of the host mirror: transport failure (status 0) is TRANSPORT, 2xx/4xx/5xx keep
// their class and any other status is OTHER. It carries no status text, URL or body.
func GatewayRequestStage(status int) string {
	switch {
	case status == 0:
		return "GW_REQUEST_END_TRANSPORT"
	case status >= 200 && status < 300:
		return "GW_REQUEST_END_2XX"
	case status >= 400 && status < 500:
		return "GW_REQUEST_END_4XX"
	case status >= 500 && status < 600:
		return "GW_REQUEST_END_5XX"
	default:
		return "GW_REQUEST_END_OTHER"
	}
}

const maxResponseBytes = 1 << 20

type staticToken string

func (s staticToken) Bearer(context.Context) (string, error) { return string(s), nil }

// StaticToken returns a TokenSource for tests and for a caller-held bearer.
func StaticToken(value string) TokenSource { return staticToken(value) }

func (c *Client) request(ctx context.Context, method, path string, body any, idempotencyKey string) ([]byte, int, error) {
	return c.requestWith(ctx, method, path, body, idempotencyKey, true)
}

// requestWith is the single bounded request builder. withBearer=false is used only for
// the public /plans read: no Authorization header is attached and the TokenSource is
// never consulted on that path.
func (c *Client) requestWith(ctx context.Context, method, path string, body any, idempotencyKey string, withBearer bool) ([]byte, int, error) {
	return c.requestWithQuery(ctx, method, path, body, idempotencyKey, withBearer, "")
}

// Keep the route passed to observation hooks separate from its wire query.
func (c *Client) requestWithQuery(ctx context.Context, method, path string, body any, idempotencyKey string, withBearer bool, query string) ([]byte, int, error) {
	if c.BaseURL == "" || c.HTTP == nil {
		return nil, 0, fmt.Errorf("accountaccess client not configured")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, 0, err
	}
	if query != "" {
		req.URL.RawQuery = query
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withBearer && c.Tokens != nil {
		bearer, err := c.Tokens.Bearer(ctx)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if c.OnRequest != nil {
		c.OnRequest(path, 0)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if c.OnResponse != nil {
			c.OnResponse(path, 0)
		}
		return nil, 0, err
	}
	if c.OnResponse != nil {
		c.OnResponse(path, resp.StatusCode)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("response too large")
	}
	return raw, resp.StatusCode, nil
}

// GetMe fetches and strictly decodes GET /me.
func (c *Client) GetMe(ctx context.Context) (MeResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodGet, "/me", nil, "")
	if err != nil {
		return MeResponse{}, nil, err
	}
	if status != http.StatusOK {
		envelope, decodeErr := decodeError(raw, status)
		if decodeErr != nil {
			return MeResponse{}, nil, decodeErr
		}
		return MeResponse{}, envelope, nil
	}
	me, err := DecodeMeStrict(raw)
	if err != nil {
		return MeResponse{}, nil, err
	}
	return me, nil, nil
}

// GetUsage fetches and strictly decodes GET /usage (account credited traffic, session:read).
// Read-only; it never mutates access, admission or entitlement state.
func (c *Client) GetUsage(ctx context.Context) (UsageResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodGet, "/usage", nil, "")
	if err != nil {
		return UsageResponse{}, nil, err
	}
	if status == http.StatusOK {
		usage, err := DecodeUsageStrict(raw)
		if err != nil {
			return UsageResponse{}, nil, err
		}
		return usage, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return UsageResponse{}, nil, decodeErr
	}
	return UsageResponse{}, envelope, nil
}

// RequestRegistrationLink requests a one-time Telegram registration deep link for the
// current installation (POST /registration/telegram/link, empty body). Installation comes
// from the Bearer session; the client never sends an installation id or eligibility.
func (c *Client) RequestRegistrationLink(ctx context.Context) (RegistrationLink, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodPost, "/registration/telegram/link", map[string]any{}, "")
	if err != nil {
		return RegistrationLink{}, nil, err
	}
	if status == http.StatusOK {
		link, err := DecodeRegistrationLinkStrict(raw)
		if err != nil {
			return RegistrationLink{}, nil, err
		}
		return link, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return RegistrationLink{}, nil, decodeErr
	}
	return RegistrationLink{}, envelope, nil
}

// ActivateTrial requests the one explicit 7-day trial activation POST /trial/activate with
// an empty body. The server owns eligibility and replay; the client sends no Telegram id and
// no eligibility. A replay returns the original interval unchanged.
func (c *Client) ActivateTrial(ctx context.Context) (TrialActivation, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodPost, "/trial/activate", map[string]any{}, "")
	if err != nil {
		return TrialActivation{}, nil, err
	}
	if status == http.StatusOK {
		activation, err := DecodeTrialActivationStrict(raw)
		if err != nil {
			return TrialActivation{}, nil, err
		}
		return activation, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return TrialActivation{}, nil, decodeErr
	}
	return TrialActivation{}, envelope, nil
}

// GetGateways returns one strict union member, or a nonterminal 409/503 admission
// envelope. The mode is decided before any credential decoding: an absent catalog_mode
// keeps the existing credential catalog path unchanged, while catalog_mode="browse"
// yields the display-only metadata catalog. A 409 ACCESS_SYNC_PENDING or
// application-failure 503 is never an instruction to clear the last-good catalog, and
// a transport error is never turned into an empty list.
func (c *Client) GetGateways(ctx context.Context) (Gateways, *ErrorResponse, error) {
	return c.getGateways(ctx, false)
}

// GetDisplayGateways explicitly requests metadata only, regardless of data rights.
// A credential response is a contract error, never a fallback for this operation.
func (c *Client) GetDisplayGateways(ctx context.Context) (Gateways, *ErrorResponse, error) {
	return c.getGateways(ctx, true)
}

func (c *Client) getGateways(ctx context.Context, displayOnly bool) (Gateways, *ErrorResponse, error) {
	query := ""
	if displayOnly {
		query = "view=browse"
	}
	raw, status, err := c.requestWithQuery(ctx, http.MethodGet, "/gateways", nil, "", true, query)
	if err != nil {
		return Gateways{}, nil, err
	}
	if status == http.StatusOK {
		gateways, err := DecodeGatewaysStrict(raw)
		if err != nil || (displayOnly && gateways.Browse == nil) {
			return Gateways{}, nil, fmt.Errorf("%w: malformed registered node", ErrMalformedCatalog)
		}
		return gateways, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return Gateways{}, nil, decodeErr
	}
	return Gateways{}, envelope, nil
}

// decodeError classifies a non-success response. A malformed, empty or unexpected
// body is an explicit transport-level error: silent zero values must never be
// mistaken for a successful contract response.
func decodeError(raw []byte, status int) (*ErrorResponse, error) {
	envelope, err := DecodeErrorStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("HTTP %d response is not a schema-valid error envelope: %w", status, err)
	}
	if envelope.Status != "error" {
		return nil, fmt.Errorf("HTTP %d response is not an error envelope", status)
	}
	return &envelope, nil
}

// SyncAccess posts exactly the body built from /me.binding_revision and the current
// authorized catalog revision, with the persisted Idempotency-Key.
func (c *Client) SyncAccess(ctx context.Context, body AccessSyncRequest, idempotencyKey string) (AccessSyncResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodPost, "/access/sync", body, idempotencyKey)
	if err != nil {
		return AccessSyncResponse{}, nil, err
	}
	if status == http.StatusOK {
		var response AccessSyncResponse
		if err := decodeStrict(raw, &response); err != nil {
			return AccessSyncResponse{}, nil, fmt.Errorf("access/sync decode: %w", err)
		}
		if response.SchemaVersion != SchemaVersion || response.OperationID == "" {
			return AccessSyncResponse{}, nil, fmt.Errorf("access/sync envelope invalid")
		}
		switch response.AccessApplicationState {
		case "not_requested", "pending", "applied", "retryable_failure", "rejected":
		default:
			return AccessSyncResponse{}, nil, fmt.Errorf("access/sync state unknown")
		}
		return response, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return AccessSyncResponse{}, nil, decodeErr
	}
	return AccessSyncResponse{}, envelope, nil
}

// GetOperation reads own-operation progress, opting into the additive sync_recovery
// marker. Polling never starts a new effect.
func (c *Client) GetOperation(ctx context.Context, id string) (OperationResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodGet, "/operations/"+id+"?observe=sync_recovery", nil, "")
	if err != nil {
		return OperationResponse{}, nil, err
	}
	if status == http.StatusOK {
		var response OperationResponse
		if err := decodeStrict(raw, &response); err != nil {
			return OperationResponse{}, nil, fmt.Errorf("operation decode: %w", err)
		}
		return response, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return OperationResponse{}, nil, decodeErr
	}
	return OperationResponse{}, envelope, nil
}

// AccessSyncRequest pins the two decimal fences; Idempotency is header-only.
type AccessSyncRequest struct {
	CatalogRevision string `json:"catalog_revision"`
	BindingRevision string `json:"binding_revision"`
}

func (r AccessSyncRequest) Digest() (string, error) {
	return canonicalDigest(map[string]any{
		"binding_revision": r.BindingRevision,
		"catalog_revision": r.CatalogRevision,
	})
}

func base64URLEncode(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func base64URLDecode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}
