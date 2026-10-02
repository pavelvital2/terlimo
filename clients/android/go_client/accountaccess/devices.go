package accountaccess

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// §§18–19 connected devices (mobile-v1). The client only reads the device list and deletes
// one explicitly chosen device; it never derives eligibility, never invents a limit increase
// and never auto-deletes another device. Device management is offered by the host only after
// a confirmed Telegram proof, and every accepted change is re-read from the server.

const (
	CodeDeviceRemoved             = "DEVICE_REMOVED"
	CodeDeviceManagementForbidden = "DEVICE_MANAGEMENT_FORBIDDEN"
	CodeDeviceNotFound            = "DEVICE_NOT_FOUND"
)

// DeviceEntry is one strict member of GET /devices.
type DeviceEntry struct {
	DeviceID  string
	Name      *string
	Platform  string
	IsCurrent bool
	Status    string
	BoundAt   string
	RevokedAt *string
}

// DevicesResponse is the strict GET /api/mobile/v1/devices 200 body.
type DevicesResponse struct {
	RequestID   string
	ServerTime  string
	Devices     []DeviceEntry
	DeviceLimit int
	SlotsUsed   int
	Revision    string
}

// DeviceDeleteResponse is the strict DELETE /api/mobile/v1/devices/{id} 200 body. `pending`
// means accepted but not yet applied at the gateway; the host must not treat it as a completed
// revoke and must not blindly repeat the DELETE.
type DeviceDeleteResponse struct {
	RequestID                  string
	ServerTime                 string
	Status                     string
	OperationID                string
	SlotReleased               bool
	AccessApplicationState     string
	ResidualAccessLeaseSeconds *int
}

var devicePathIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// deviceStatuses is the canonical devices.json enum. platform is const "android".
var deviceStatuses = map[string]bool{
	"active": true, "pending": true, "revoked": true, "deactivated": true,
}

// optionalBoundedString accepts an optional string|null bounded to maxLen.
func optionalBoundedString(value *string, maxLen int) bool {
	return value == nil || len(*value) <= maxLen
}

// ValidDevicePathID reports whether a device id is a valid single path segment: one
// unreserved 1..128-char segment, no dot-dot and no path tricks.
func ValidDevicePathID(id string) bool {
	if id == "" || id == "." || id == ".." || strings.Contains(id, "..") {
		return false
	}
	return devicePathIDPattern.MatchString(id)
}

var accessApplicationStates = map[string]bool{
	"not_requested": true, "pending": true, "applied": true,
	"retryable_failure": true, "rejected": true,
}

// GetDevices performs GET /api/mobile/v1/devices (Bearer session:read).
func (c *Client) GetDevices(ctx context.Context) (DevicesResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodGet, "/devices", nil, "")
	if err != nil {
		return DevicesResponse{}, nil, err
	}
	if status != http.StatusOK {
		envelope, decodeErr := decodeError(raw, status)
		if decodeErr != nil {
			return DevicesResponse{}, nil, decodeErr
		}
		return DevicesResponse{}, envelope, nil
	}
	devices, err := DecodeDevicesStrict(raw)
	if err != nil {
		return DevicesResponse{}, nil, err
	}
	return devices, nil, nil
}

// DeleteDevice performs DELETE /api/mobile/v1/devices/{id} (Bearer session:write). The
// caller supplies the host-owned Idempotency-Key; it is never generated or rotated here.
func (c *Client) DeleteDevice(ctx context.Context, deviceID, idempotencyKey string) (DeviceDeleteResponse, *ErrorResponse, error) {
	if !ValidDevicePathID(deviceID) {
		return DeviceDeleteResponse{}, nil, fmt.Errorf("device id invalid")
	}
	raw, status, err := c.request(ctx, http.MethodDelete, "/devices/"+deviceID,
		nil, idempotencyKey)
	if err != nil {
		return DeviceDeleteResponse{}, nil, err
	}
	if status != http.StatusOK {
		envelope, decodeErr := decodeError(raw, status)
		if decodeErr != nil {
			return DeviceDeleteResponse{}, nil, decodeErr
		}
		return DeviceDeleteResponse{}, envelope, nil
	}
	result, err := DecodeDeviceDeleteStrict(raw)
	if err != nil {
		return DeviceDeleteResponse{}, nil, err
	}
	return result, nil, nil
}

// DecodeDevicesStrict validates a GET /devices 200 body exactly.
func DecodeDevicesStrict(raw []byte) (DevicesResponse, error) {
	var body struct {
		RequestID     string `json:"request_id"`
		ServerTime    string `json:"server_time"`
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		Devices       []struct {
			DeviceID  string  `json:"device_id"`
			Name      *string `json:"name"`
			Platform  string  `json:"platform"`
			IsCurrent *bool   `json:"is_current"`
			Status    string  `json:"status"`
			BoundAt   *string `json:"bound_at"`
			RevokedAt *string `json:"revoked_at"`
		} `json:"devices"`
		DeviceLimit *int   `json:"device_limit"`
		SlotsUsed   *int   `json:"slots_used"`
		Revision    string `json:"revision"`
	}
	if err := decodeStrict(raw, &body); err != nil {
		return DevicesResponse{}, fmt.Errorf("devices decode: %w", err)
	}
	if body.SchemaVersion != SchemaVersion || body.Status != "ok" {
		return DevicesResponse{}, fmt.Errorf("devices envelope invalid")
	}
	if !requestIDPattern.MatchString(body.RequestID) || !ValidUtcTime(body.ServerTime) {
		return DevicesResponse{}, fmt.Errorf("devices envelope fields invalid")
	}
	if !ValidRevision(body.Revision) {
		return DevicesResponse{}, fmt.Errorf("devices revision invalid")
	}
	// Nonnegative server/Android Int32 counts; independent even after capacity expiry.
	if body.DeviceLimit == nil || *body.DeviceLimit < 0 || *body.DeviceLimit > 1<<31-1 {
		return DevicesResponse{}, fmt.Errorf("device_limit out of range")
	}
	if body.SlotsUsed == nil || *body.SlotsUsed < 0 || *body.SlotsUsed > 1<<31-1 {
		return DevicesResponse{}, fmt.Errorf("slots_used out of range")
	}
	seen := map[string]bool{}
	devices := make([]DeviceEntry, 0, len(body.Devices))
	for _, d := range body.Devices {
		if !ValidDevicePathID(d.DeviceID) || seen[d.DeviceID] {
			return DevicesResponse{}, fmt.Errorf("device id invalid or duplicated")
		}
		seen[d.DeviceID] = true
		if !optionalBoundedString(d.Name, 64) {
			return DevicesResponse{}, fmt.Errorf("device name too long")
		}
		if d.Platform != "android" {
			return DevicesResponse{}, fmt.Errorf("device platform invalid")
		}
		if !deviceStatuses[d.Status] {
			return DevicesResponse{}, fmt.Errorf("device status invalid")
		}
		if !optionalBoundedString(d.BoundAt, 64) || !optionalBoundedString(d.RevokedAt, 64) {
			return DevicesResponse{}, fmt.Errorf("device timestamp invalid")
		}
		if d.IsCurrent == nil {
			return DevicesResponse{}, fmt.Errorf("device is_current missing")
		}
		boundAt := ""
		if d.BoundAt != nil {
			boundAt = *d.BoundAt
		}
		devices = append(devices, DeviceEntry{
			DeviceID: d.DeviceID, Name: d.Name, Platform: d.Platform, IsCurrent: *d.IsCurrent,
			Status: d.Status, BoundAt: boundAt, RevokedAt: d.RevokedAt,
		})
	}
	return DevicesResponse{
		RequestID: body.RequestID, ServerTime: body.ServerTime, Devices: devices,
		DeviceLimit: *body.DeviceLimit, SlotsUsed: *body.SlotsUsed, Revision: body.Revision,
	}, nil
}

// DecodeDeviceDeleteStrict validates a DELETE /devices/{id} 200 body exactly.
func DecodeDeviceDeleteStrict(raw []byte) (DeviceDeleteResponse, error) {
	var body struct {
		RequestID                  string `json:"request_id"`
		ServerTime                 string `json:"server_time"`
		SchemaVersion              string `json:"schema_version"`
		Status                     string `json:"status"`
		OperationID                string `json:"operation_id"`
		SlotReleased               *bool  `json:"slot_released"`
		AccessApplicationState     string `json:"access_application_state"`
		ResidualAccessLeaseSeconds *int   `json:"residual_access_lease_seconds"`
	}
	if err := decodeStrict(raw, &body); err != nil {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete decode: %w", err)
	}
	if body.SchemaVersion != SchemaVersion || (body.Status != "ok" && body.Status != "pending") {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete envelope invalid")
	}
	if !requestIDPattern.MatchString(body.RequestID) || !ValidUtcTime(body.ServerTime) {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete envelope fields invalid")
	}
	if body.OperationID == "" || len(body.OperationID) > 128 {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete operation invalid")
	}
	if body.SlotReleased == nil {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete slot_released missing")
	}
	if !accessApplicationStates[body.AccessApplicationState] {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete application state invalid")
	}
	if body.ResidualAccessLeaseSeconds != nil && *body.ResidualAccessLeaseSeconds < 0 {
		return DeviceDeleteResponse{}, fmt.Errorf("device delete residual lease invalid")
	}
	return DeviceDeleteResponse{
		RequestID: body.RequestID, ServerTime: body.ServerTime, Status: body.Status,
		OperationID: body.OperationID, SlotReleased: *body.SlotReleased,
		AccessApplicationState:     body.AccessApplicationState,
		ResidualAccessLeaseSeconds: body.ResidualAccessLeaseSeconds,
	}, nil
}
