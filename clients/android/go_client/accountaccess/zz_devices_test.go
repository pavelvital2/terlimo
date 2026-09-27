package accountaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Canonical devices.json: platform const android; status active|pending|revoked|deactivated;
// name optional|null<=64; bound_at/revoked_at optional|null; limit/slots 0..100 independent.
const devicesOK = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z",
 "schema_version":"1.0","status":"ok",
 "devices":[{"device_id":"dev-1","name":"Pixel","platform":"android","is_current":true,"status":"active","bound_at":"2026-09-27T09:00:00Z","revoked_at":null},
            {"device_id":"dev-2","name":null,"platform":"android","is_current":false,"status":"revoked"}],
 "device_limit":2,"slots_used":1,"revision":"7"}`

const deviceDeleteOK = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z",
 "schema_version":"1.0","status":"pending","operation_id":"op-1","slot_released":true,
 "access_application_state":"pending","residual_access_lease_seconds":600}`

func TestDecodeDevicesStrictAcceptsCanonicalAndRejects(t *testing.T) {
	devices, err := DecodeDevicesStrict([]byte(devicesOK))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(devices.Devices) != 2 || devices.Devices[0].Name == nil || *devices.Devices[0].Name != "Pixel" ||
		devices.Devices[1].Name != nil || devices.Devices[1].Status != "revoked" ||
		devices.Devices[1].BoundAt != "" || devices.DeviceLimit != 2 || devices.SlotsUsed != 1 {
		t.Fatalf("decoded wrong: %+v", devices)
	}

	// Allowed forms: pending/deactivated status, device_limit 0, slots independent of limit.
	allowed := []string{
		strings.Replace(devicesOK, `"status":"active"`, `"status":"pending"`, 1),
		strings.Replace(devicesOK, `"status":"revoked"`, `"status":"deactivated"`, 1),
		strings.Replace(devicesOK, `"device_limit":2`, `"device_limit":0`, 1),
		strings.Replace(devicesOK, `"device_limit":2,"slots_used":1`, `"device_limit":1,"slots_used":3`, 1),
	}
	for i, raw := range allowed {
		if _, err := DecodeDevicesStrict([]byte(raw)); err != nil {
			t.Fatalf("allowed case %d rejected: %v", i, err)
		}
	}

	reject := []string{
		strings.Replace(devicesOK, `"platform":"android"`, `"platform":"ios"`, 1),
		strings.Replace(devicesOK, `"name":"Pixel"`, `"name":"`+strings.Repeat("x", 65)+`"`, 1),
		strings.Replace(devicesOK, `"status":"active"`, `"status":"weird"`, 1),
		strings.Replace(devicesOK, `"device_limit":2,`, ``, 1),
		strings.Replace(devicesOK, `"slots_used":1,`, ``, 1),
		strings.Replace(devicesOK, `"is_current":true,`, ``, 1),
		strings.Replace(devicesOK, `"device_id":"dev-2"`, `"device_id":"dev-1"`, 1),
		strings.Replace(devicesOK, `"revision":"7"`, `"revision":"7","extra":1`, 1),
	}
	for i, raw := range reject {
		if _, err := DecodeDevicesStrict([]byte(raw)); err == nil {
			t.Fatalf("case %d: expected rejection", i)
		}
	}
}

func TestDecodeDeviceDeleteStrictAcceptsAndRejects(t *testing.T) {
	result, err := DecodeDeviceDeleteStrict([]byte(deviceDeleteOK))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != "pending" || !result.SlotReleased || result.AccessApplicationState != "pending" ||
		result.ResidualAccessLeaseSeconds == nil || *result.ResidualAccessLeaseSeconds != 600 {
		t.Fatalf("decoded wrong: %+v", result)
	}
	if _, err := DecodeDeviceDeleteStrict([]byte(
		strings.Replace(deviceDeleteOK, `"residual_access_lease_seconds":600`, `"residual_access_lease_seconds":null`, 1))); err != nil {
		t.Fatalf("null residual must be accepted: %v", err)
	}
	reject := []string{
		strings.Replace(deviceDeleteOK, `"slot_released":true,`, ``, 1),
		strings.Replace(deviceDeleteOK, `"status":"pending"`, `"status":"done"`, 1),
		strings.Replace(deviceDeleteOK, `"access_application_state":"pending"`, `"access_application_state":"made_up"`, 1),
		strings.Replace(deviceDeleteOK, `"residual_access_lease_seconds":600`, `"residual_access_lease_seconds":-1`, 1),
		strings.Replace(deviceDeleteOK, `"operation_id":"op-1"`, `"operation_id":""`, 1),
	}
	for i, raw := range reject {
		if _, err := DecodeDeviceDeleteStrict([]byte(raw)); err == nil {
			t.Fatalf("case %d: expected rejection", i)
		}
	}
}

func TestDeviceClientMethodsAndHeaders(t *testing.T) {
	var getAuth, delAuth, delKey, delPath string
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mobile/v1/devices":
			getAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(devicesOK))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/mobile/v1/devices/dev-1":
			delAuth = r.Header.Get("Authorization")
			delKey = r.Header.Get("Idempotency-Key")
			delPath = r.URL.Path
			_, _ = w.Write([]byte(deviceDeleteOK))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer-1")}
	list, apiError, err := client.GetDevices(context.Background())
	if err != nil || apiError != nil || len(list.Devices) != 2 {
		t.Fatalf("GetDevices: %v %v %+v", err, apiError, list)
	}
	if getAuth != "Bearer bearer-1" {
		t.Fatalf("GET auth=%q", getAuth)
	}
	result, apiError, err := client.DeleteDevice(context.Background(), "dev-1", "terlimo-delete-key-0001")
	if err != nil || apiError != nil || result.Status != "pending" {
		t.Fatalf("DeleteDevice: %v %v %+v", err, apiError, result)
	}
	if delAuth != "Bearer bearer-1" || delKey != "terlimo-delete-key-0001" || delPath != "/api/mobile/v1/devices/dev-1" {
		t.Fatalf("DELETE headers path=%q auth=%q key=%q", delPath, delAuth, delKey)
	}
	before := hits
	if _, _, err := client.DeleteDevice(context.Background(), "..", "terlimo-delete-key-0001"); err == nil {
		t.Fatal("expected invalid id rejection")
	}
	if hits != before {
		t.Fatal("invalid id must not hit the wire")
	}
}

func TestDeviceClientForwardsBoundedErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z","schema_version":"1.0","status":"error","code":"DEVICE_REMOVED","retryable":false,"retry_after_ms":null,"message_key":null,"details":{}}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer-1")}
	if _, apiError, err := client.GetDevices(context.Background()); err != nil || apiError == nil || apiError.Code != "DEVICE_REMOVED" {
		t.Fatalf("expected DEVICE_REMOVED, got err=%v apiError=%+v", err, apiError)
	}
}
