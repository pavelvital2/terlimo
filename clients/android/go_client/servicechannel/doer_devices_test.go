package servicechannel

import (
	"strings"
	"testing"
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
