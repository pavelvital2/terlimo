package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"wg-turn-client/servicechannel"
)

func TestCatalogBridgeStagesAreAttemptCorrelated(t *testing.T) {
	var out bytes.Buffer
	bridge := newManagedBridge(&out, "catalog-attempt", func() {})
	doer := &servicechannel.Doer{Channel: servicechannel.NewChannel(nil)}
	configureCatalogStages(doer, bridge)
	p := doer.Channel.Catalog
	if p == nil || p.ConnectTimeout != 20*time.Second || p.DeviceTimeout != 25*time.Second || p.StatusTimeout != 10*time.Second || p.CatalogTimeout != 10*time.Second {
		t.Fatalf("unexpected native policy: %+v", p)
	}
	if err := p.Emit(context.Background(), "checking_device"); err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(out.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame) != 4 || frame["type"] != "catalog_stage" || frame["stage"] != "checking_device" || frame["attempt_id"] != "catalog-attempt" || frame["v"] != float64(1) {
		t.Fatalf("unexpected product event: %v", frame)
	}
}
