package accountaccess

import (
	"strings"
	"testing"
)

func TestDeviceCountsInt32Boundary(t *testing.T) {
	me := meBody("7", nil, "not_started")
	for _, counts := range [][2]string{{"101", "102"}, {"2147483647", "2147483647"}, {"0", "101"}} {
		raw := strings.Replace(me, `"effective_device_limit":2,"slots_used":1`, `"effective_device_limit":`+counts[0]+`,"slots_used":`+counts[1], 1)
		if _, err := DecodeMeStrict([]byte(raw)); err != nil {
			t.Fatalf("me boundary %v: %v", counts, err)
		}
		raw = strings.Replace(devicesOK, `"device_limit":2,"slots_used":1`, `"device_limit":`+counts[0]+`,"slots_used":`+counts[1], 1)
		if _, err := DecodeDevicesStrict([]byte(raw)); err != nil {
			t.Fatalf("devices boundary %v: %v", counts, err)
		}
	}
	for _, bad := range []string{"-1", "1.5", "1.0", "2147483648", "9223372036854775808", `"101"`, "null"} {
		for _, field := range []string{"effective_device_limit", "slots_used"} {
			old := `"` + field + `":2`
			if field == "slots_used" {
				old = `"slots_used":1`
			}
			raw := strings.Replace(me, old, `"`+field+`":`+bad, 1)
			if _, err := DecodeMeStrict([]byte(raw)); err == nil {
				t.Fatalf("me accepted %s=%s", field, bad)
			}
		}
		for _, field := range []string{"device_limit", "slots_used"} {
			old := `"` + field + `":2`
			if field == "slots_used" {
				old = `"slots_used":1`
			}
			raw := strings.Replace(devicesOK, old, `"`+field+`":`+bad, 1)
			if _, err := DecodeDevicesStrict([]byte(raw)); err == nil {
				t.Fatalf("devices accepted %s=%s", field, bad)
			}
		}
	}
}
