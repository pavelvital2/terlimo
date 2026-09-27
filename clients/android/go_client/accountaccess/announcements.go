package accountaccess

// S5 §11 announcements surface: strict contract consumption of
// GET /announcements and the idempotent POST /announcements/{id}/read on the same
// client/transport seam as the other mobile-v1 routes (contract basis contracts-3e12f6f9:
// schemas/announcement.json + schemas/common.json). Native is a bounded forwarder: it
// never fabricates an announcement, action, date or read marker, and it never mutates
// entitlement. The host owns the Idempotency-Key and forwards it verbatim.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// unmarshalStrictRequiredNonNull is the announcements-local strict decoder: it is
// unmarshalStrictRequired plus an explicit rejection of a literal JSON null for every
// required (non-nullable) key. encoding/json folds null into the zero value of a Go
// scalar without an error, so without this a `title:null`, `unread:null`,
// `unread_count:null`, `read:null` or a null required id would pass a schema that
// declares those fields as string/bool/integer. Nullable/optional fields (valid_until,
// action.label) are intentionally not required and keep their null semantics.
func unmarshalStrictRequiredNonNull(raw []byte, out any, required ...string) error {
	if err := unmarshalStrictRequired(raw, out, required...); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	for _, key := range required {
		if strings.TrimSpace(string(keys[key])) == "null" {
			return fmt.Errorf("required field %q must not be null", key)
		}
	}
	return nil
}

// Announcement action types fixed by announcement.json.
const (
	AnnouncementActionNone           = "none"
	AnnouncementActionRefreshCatalog = "refresh_catalog"
	AnnouncementActionOpenPayments   = "open_payments"
	AnnouncementActionOpenSupport    = "open_support"
)

func validAnnouncementActionType(value string) bool {
	switch value {
	case AnnouncementActionNone, AnnouncementActionRefreshCatalog,
		AnnouncementActionOpenPayments, AnnouncementActionOpenSupport:
		return true
	}
	return false
}

// validAnnouncementID is the contract announcement_id: 1..128 characters, no control or
// whitespace-free requirement is imposed beyond the schema; CR/LF are rejected so a value
// can never be split into headers.
func validAnnouncementID(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n")
}

// announcementPathIDPattern is the single unreserved-segment grammar shared with the
// service-channel wire allowlist (servicechannel.validAnnouncementPathID). The contract
// schema allows any string 1..128, but the mobile-v1 dynamic path gate only permits one
// unreserved segment; ids outside this set can never reach the server and are rejected
// locally/fail-closed instead of being sent and then rejected late.
var announcementPathIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// ValidAnnouncementPathID bounds the /announcements/{id}/read segment exactly like the
// service-channel allowlist: one unreserved segment, no bare "."/"..", no embedded "..",
// no slash, percent, question, hash or backslash.
func ValidAnnouncementPathID(value string) bool {
	if value == "" || value == "." || value == ".." || strings.Contains(value, "..") {
		return false
	}
	return announcementPathIDPattern.MatchString(value)
}

// validAnnouncementPathID is the in-package alias used by MarkAnnouncementRead.
func validAnnouncementPathID(value string) bool { return ValidAnnouncementPathID(value) }

// AnnouncementAction is the optional call-to-action of one announcement.
type AnnouncementAction struct {
	Type  string  `json:"type"`
	Label *string `json:"label"`
}

// UnmarshalJSON enforces the closed action object and the enum.
func (a *AnnouncementAction) UnmarshalJSON(raw []byte) error {
	type plain AnnouncementAction
	var value plain
	if err := unmarshalStrictRequiredNonNull(raw, &value, "type"); err != nil {
		return err
	}
	if !validAnnouncementActionType(value.Type) {
		return fmt.Errorf("invalid announcement action type")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	if label, present := keys["label"]; present {
		// A present label must be a non-null string (never null/number/bool).
		if strings.TrimSpace(string(label)) == "null" {
			return fmt.Errorf("action label must not be null")
		}
		var text string
		if err := json.Unmarshal(label, &text); err != nil {
			return err
		}
		if len(text) > 64 {
			return fmt.Errorf("announcement action label too long")
		}
	}
	*a = AnnouncementAction(value)
	return nil
}

// Announcement is one strict GET /announcements entry.
type Announcement struct {
	AnnouncementID string             `json:"announcement_id"`
	Title          string             `json:"title"`
	Text           string             `json:"text"`
	Action         AnnouncementAction `json:"action"`
	ValidUntil     *string            `json:"valid_until"`
	Unread         bool               `json:"unread"`
	Revision       string             `json:"revision"`
}

// UnmarshalJSON enforces required field presence and bounded scalar lengths/types.
func (a *Announcement) UnmarshalJSON(raw []byte) error {
	type plain Announcement
	var value plain
	if err := unmarshalStrictRequiredNonNull(raw, &value,
		"announcement_id", "title", "text", "action", "unread", "revision"); err != nil {
		return err
	}
	if !validAnnouncementID(value.AnnouncementID) {
		return fmt.Errorf("invalid announcement_id")
	}
	if len(value.Title) > 128 {
		return fmt.Errorf("announcement title too long")
	}
	if len(value.Text) > 4096 {
		return fmt.Errorf("announcement text too long")
	}
	if !ValidRevision(value.Revision) {
		return fmt.Errorf("invalid announcement revision")
	}
	// valid_until is optional and nullable; when present it must be a string, never a
	// number/bool/object, and is bounded so a hostile value cannot be carried far.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	if until, present := keys["valid_until"]; present {
		if strings.TrimSpace(string(until)) != "null" {
			var text string
			if err := json.Unmarshal(until, &text); err != nil {
				return err
			}
			if len(text) > 64 {
				return fmt.Errorf("announcement valid_until too long")
			}
		}
	}
	*a = Announcement(value)
	return nil
}

// AnnouncementsResponse is the strict GET /announcements 200 body.
type AnnouncementsResponse struct {
	RequestID     string         `json:"request_id"`
	ServerTime    string         `json:"server_time"`
	SchemaVersion string         `json:"schema_version"`
	Status        string         `json:"status"`
	Announcements []Announcement `json:"announcements"`
	UnreadCount   int            `json:"unread_count"`
}

// UnmarshalJSON enforces the closed response object and the fixed envelope.
func (r *AnnouncementsResponse) UnmarshalJSON(raw []byte) error {
	type plain AnnouncementsResponse
	var value plain
	if err := unmarshalStrictRequiredNonNull(raw, &value,
		"request_id", "server_time", "schema_version", "status", "announcements", "unread_count"); err != nil {
		return err
	}
	if err := validateEnvelope(value.RequestID, value.ServerTime, value.SchemaVersion, value.Status); err != nil {
		return err
	}
	if value.UnreadCount < 0 {
		return fmt.Errorf("negative unread_count")
	}
	// A JSON null announcements must be rejected, not folded into an empty slice.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	if first := strings.TrimSpace(string(keys["announcements"])); !strings.HasPrefix(first, "[") {
		return fmt.Errorf("announcements must be an array")
	}
	*r = AnnouncementsResponse(value)
	return nil
}

// ReadMarkerResponse is the strict POST /announcements/{id}/read 200 body.
type ReadMarkerResponse struct {
	RequestID      string `json:"request_id"`
	ServerTime     string `json:"server_time"`
	SchemaVersion  string `json:"schema_version"`
	Status         string `json:"status"`
	AnnouncementID string `json:"announcement_id"`
	Read           bool   `json:"read"`
	ReadAt         string `json:"read_at"`
}

// UnmarshalJSON enforces the closed response object and the fixed envelope.
func (r *ReadMarkerResponse) UnmarshalJSON(raw []byte) error {
	type plain ReadMarkerResponse
	var value plain
	if err := unmarshalStrictRequiredNonNull(raw, &value,
		"request_id", "server_time", "schema_version", "status",
		"announcement_id", "read", "read_at"); err != nil {
		return err
	}
	if err := validateEnvelope(value.RequestID, value.ServerTime, value.SchemaVersion, value.Status); err != nil {
		return err
	}
	if !validAnnouncementID(value.AnnouncementID) {
		return fmt.Errorf("invalid announcement_id")
	}
	if !ValidUtcTime(value.ReadAt) {
		return fmt.Errorf("invalid read_at")
	}
	*r = ReadMarkerResponse(value)
	return nil
}

// validateEnvelope proves the mandatory mobile-v1 envelope shared by every response.
func validateEnvelope(requestID, serverTime, schemaVersion, status string) error {
	if !requestIDPattern.MatchString(requestID) {
		return fmt.Errorf("invalid request_id")
	}
	if !ValidUtcTime(serverTime) {
		return fmt.Errorf("invalid server_time")
	}
	if schemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version")
	}
	if status != "ok" {
		return fmt.Errorf("unexpected status")
	}
	return nil
}

// DecodeAnnouncementsStrict strictly decodes a GET /announcements 200 body.
func DecodeAnnouncementsStrict(raw []byte) (AnnouncementsResponse, error) {
	var response AnnouncementsResponse
	if err := decodeStrict(raw, &response); err != nil {
		return AnnouncementsResponse{}, err
	}
	return response, nil
}

// DecodeReadMarkerStrict strictly decodes a POST read-marker 200 body.
func DecodeReadMarkerStrict(raw []byte) (ReadMarkerResponse, error) {
	var response ReadMarkerResponse
	if err := decodeStrict(raw, &response); err != nil {
		return ReadMarkerResponse{}, err
	}
	return response, nil
}

// ListAnnouncements fetches and strictly decodes GET /announcements. Display-only read:
// it never mutates admission, entitlement or the tunnel.
func (c *Client) ListAnnouncements(ctx context.Context) (AnnouncementsResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodGet, "/announcements", nil, "")
	if err != nil {
		return AnnouncementsResponse{}, nil, err
	}
	if status == http.StatusOK {
		response, err := DecodeAnnouncementsStrict(raw)
		if err != nil {
			return AnnouncementsResponse{}, nil, err
		}
		return response, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return AnnouncementsResponse{}, nil, decodeErr
	}
	return AnnouncementsResponse{}, envelope, nil
}

// MarkAnnouncementRead posts the idempotent read marker
// POST /announcements/{id}/read. The host-owned Idempotency-Key is forwarded verbatim;
// a repeat with the same key is the server's idempotent replay, never a native rewrite.
func (c *Client) MarkAnnouncementRead(ctx context.Context, announcementID, idempotencyKey string) (ReadMarkerResponse, *ErrorResponse, error) {
	if !validAnnouncementPathID(announcementID) || !validIdempotencyKey(idempotencyKey) {
		return ReadMarkerResponse{}, nil, ErrInvalidRequest
	}
	raw, status, err := c.request(ctx, http.MethodPost, "/announcements/"+announcementID+"/read", nil, idempotencyKey)
	if err != nil {
		return ReadMarkerResponse{}, nil, err
	}
	if status == http.StatusOK {
		response, err := DecodeReadMarkerStrict(raw)
		if err != nil {
			return ReadMarkerResponse{}, nil, err
		}
		return response, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return ReadMarkerResponse{}, nil, decodeErr
	}
	return ReadMarkerResponse{}, envelope, nil
}
