package servicechannel

import (
	"context"
	"strings"
	"testing"

	"wg-turn-client/accountaccess"

	"wg-turn-client/wlwire"
)

// TestDoerDevicesAllowlistMatrix pins the §§18–19 device allowlist: GET /devices and the
// dynamic DELETE /devices/{id} segment, plus every traversal/shape rejection.
func TestDoerDevicesAllowlistMatrix(t *testing.T) {
	accepted := []struct {
		method string
		path   string
		class  string
	}{
		{"GET", "/api/mobile/v1/devices", "DEVICES"},
		{"DELETE", "/api/mobile/v1/devices/dev-1", "DEVICES"},
		{"DELETE", "/api/mobile/v1/devices/A.b~c-1", "DEVICES"},
		{"DELETE", "/api/mobile/v1/devices/" + strings.Repeat("a", 128), "DEVICES"},
	}
	for _, item := range accepted {
		if !pathAllowed(item.method, item.path) {
			t.Fatalf("allowlist rejected %s %s", item.method, item.path)
		}
		if class := requestClassForPath(item.path); class != item.class {
			t.Fatalf("class(%s)=%s want %s", item.path, class, item.class)
		}
	}

	rejected := []struct {
		method string
		path   string
	}{
		{"POST", "/api/mobile/v1/devices"},
		{"DELETE", "/api/mobile/v1/devices"},
		{"GET", "/api/mobile/v1/devices/dev-1"},
		{"DELETE", "/api/mobile/v1/devices/"},
		{"DELETE", "/api/mobile/v1/devices/."},
		{"DELETE", "/api/mobile/v1/devices/.."},
		{"DELETE", "/api/mobile/v1/devices/a..b"},
		{"DELETE", "/api/mobile/v1/devices/a/b"},
		{"DELETE", "/api/mobile/v1/devices/" + strings.Repeat("a", 129)},
		{"DELETE", "/api/mobile/v1/devices/dev-1%2Fme"},
		{"DELETE", "/api/mobile/v1/admin/devices/dev-1"},
		{"GET", "/api/mobile/v1/me/devices/dev-1"},
	}
	for _, item := range rejected {
		if pathAllowed(item.method, item.path) {
			t.Fatalf("allowlist accepted %s %s", item.method, item.path)
		}
	}
}

const devicesListJSON = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z","schema_version":"1.0","status":"ok","devices":[],"device_limit":0,"slots_used":0,"revision":"0"}`
const deviceDeleteJSON = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z","schema_version":"1.0","status":"ok","operation_id":"op-1","slot_released":true,"access_application_state":"applied"}`

func devicesFixture(t *testing.T) (*fixtureEstablisher, *Doer) {
	t.Helper()
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		switch {
		case frame.Method == "GET" && frame.Path == "/api/mobile/v1/devices":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeadersForTest(), []byte(devicesListJSON))}
		case frame.Method == "DELETE" && frame.Path == "/api/mobile/v1/devices/dev-1":
			return fixtureResponse{body: responseJSON(t, id, 200, jsonHeadersForTest(), []byte(deviceDeleteJSON))}
		default:
			return fixtureResponse{}
		}
	})
	return fixture, fixtureDoer(t, fixture)
}

func jsonHeadersForTest() map[string]string { return map[string]string{"Content-Type": "application/json"} }

// Full Doer.Do + validateRequest + Channel path for the devices read.
func TestDoerDevicesGetThroughFullPath(t *testing.T) {
	fixture, doer := devicesFixture(t)
	req, err := newTestRequest("GET", testOrigin+"/api/mobile/v1/devices", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test")
	resp, err := doer.Do(req)
	if err != nil {
		t.Fatalf("GET /devices rejected through full Doer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || fixture.dialCount() != 1 {
		t.Fatalf("GET /devices status=%d dials=%d", resp.StatusCode, fixture.dialCount())
	}
}

// DELETE is allowed ONLY on the device path and blocked everywhere else.
func TestDoerDeleteOnlyForDevicePath(t *testing.T) {
	fixture, doer := devicesFixture(t)
	okReq, _ := newTestRequest("DELETE", testOrigin+"/api/mobile/v1/devices/dev-1", nil)
	okReq.Header.Set("Authorization", "Bearer test")
	resp, err := doer.Do(okReq)
	if err != nil {
		t.Fatalf("DELETE /devices/dev-1 rejected: %v", err)
	}
	resp.Body.Close()
	for _, target := range []string{
		testOrigin + "/api/mobile/v1/devices/..",
		testOrigin + "/api/mobile/v1/devices/",
		testOrigin + "/api/mobile/v1/payments/pay-1",
		testOrigin + "/api/mobile/v1/me",
	} {
		req, _ := newTestRequest("DELETE", target, nil)
		req.Header.Set("Authorization", "Bearer test")
		if _, err := doer.Do(req); err == nil {
			t.Fatalf("DELETE must be rejected for %s", target)
		}
	}
	if fixture.dialCount() != 1 {
		t.Fatalf("only the device DELETE may reach the channel, dials=%d", fixture.dialCount())
	}
}

// The unmodified accountaccess client drives both calls through the same full path.
func TestAccountaccessDevicesThroughFullDoer(t *testing.T) {
	_, doer := devicesFixture(t)
	client := &accountaccess.Client{BaseURL: testOrigin, HTTP: doer, Tokens: accountaccess.StaticToken("b")}
	if _, apiError, err := client.GetDevices(context.Background()); err != nil || apiError != nil {
		t.Fatalf("GetDevices through full Doer: err=%v apiError=%+v", err, apiError)
	}
	if _, apiError, err := client.DeleteDevice(context.Background(), "dev-1", "terlimo-delete-key-0001"); err != nil || apiError != nil {
		t.Fatalf("DeleteDevice through full Doer: err=%v apiError=%+v", err, apiError)
	}
}
