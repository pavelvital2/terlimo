package accountaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"wg-turn-client/wlwire"
)

// ServiceSeedResponse is Recovery v1's frozen envelope, not a /me extension.
type ServiceSeedResponse struct {
	RequestID     string `json:"request_id"`
	ServerTime    string `json:"server_time"`
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	RecoveryCode  string `json:"recovery_code"`
}

func DecodeServiceSeed(raw []byte) (ServiceSeedResponse, error) {
	var result ServiceSeedResponse
	var fields map[string]json.RawMessage
	if wlwire.StrictJSON(raw, &result) != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 5 {
		return ServiceSeedResponse{}, fmt.Errorf("service seed envelope invalid")
	}
	for _, name := range []string{"request_id", "server_time", "schema_version", "status", "recovery_code"} {
		if _, ok := fields[name]; !ok {
			return ServiceSeedResponse{}, fmt.Errorf("service seed envelope invalid")
		}
	}
	if !requestIDPattern.MatchString(result.RequestID) || result.SchemaVersion != SchemaVersion || result.Status != "ok" || len(result.RecoveryCode) == 0 || len(result.RecoveryCode) > 3500 {
		return ServiceSeedResponse{}, fmt.Errorf("service seed envelope invalid")
	}
	if _, err := parseUtc(result.ServerTime); err != nil {
		return ServiceSeedResponse{}, fmt.Errorf("service seed server_time invalid")
	}
	return result, nil
}

// One authorized read through the existing transport; no retries or periodic job.
// Scheduling is a joint seam: Channel serializes I/O, so an uncoordinated goroutine
// here must not be allowed to delay a catalogue cycle.
func (c *Client) GetServiceSeed(ctx context.Context) (ServiceSeedResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, http.MethodGet, "/service-seed", nil, "")
	if err != nil {
		return ServiceSeedResponse{}, nil, err
	}
	if status != http.StatusOK {
		apiError, decodeErr := decodeError(raw, status)
		return ServiceSeedResponse{}, apiError, decodeErr
	}
	result, err := DecodeServiceSeed(raw)
	return result, nil, err
}
