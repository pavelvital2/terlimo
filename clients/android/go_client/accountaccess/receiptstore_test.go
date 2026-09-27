package accountaccess

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestBridgeReceiptStoreRoundTripAndNamespace(t *testing.T) {
	receipt := Receipt{Subject: Subject{AccountRef: "acc-1", InstallationID: "inst-1"},
		Key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BodyDigest: "digest",
		Request: AccessSyncRequest{CatalogRevision: "7", BindingRevision: "1"}}
	var persisted []byte
	store, err := NewBridgeReceiptStore(nil, func(ctx context.Context, payload []byte) error {
		persisted = append([]byte(nil), payload...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(); err != nil || loaded != nil {
		t.Fatalf("empty store must load nil: %+v %v", loaded, err)
	}
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	var envelope receiptEnvelope
	if err := json.Unmarshal(persisted, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Namespace != ReceiptNamespace || envelope.Version != 1 ||
		envelope.Receipt.Key != receipt.Key || envelope.Receipt.Subject.AccountRef != "acc-1" {
		t.Fatalf("persisted envelope wrong: %+v", envelope)
	}

	// Restart: a fresh store over the captured blob loads the same operation identity.
	restarted, err := NewBridgeReceiptStore(persisted, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := restarted.Load()
	if err != nil || reloaded == nil || reloaded.Key != receipt.Key ||
		reloaded.BodyDigest != receipt.BodyDigest || reloaded.Request.BindingRevision != "1" {
		t.Fatalf("restart reload wrong: %+v %v", reloaded, err)
	}
}

func TestBridgeReceiptStorePersistFailureKeepsCache(t *testing.T) {
	first := Receipt{Subject: Subject{AccountRef: "acc-1", InstallationID: "inst-1"},
		Key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BodyDigest: "one"}
	failing := false
	store, err := NewBridgeReceiptStore(nil, func(ctx context.Context, payload []byte) error {
		if failing {
			return errors.New("persist unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	failing = true
	second := first
	second.Key = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := store.Save(second); err == nil {
		t.Fatal("failed persist must surface an error")
	}
	loaded, err := store.Load()
	if err != nil || loaded == nil || loaded.Key != first.Key {
		t.Fatalf("failed save must not advance the memory copy: %+v %v", loaded, err)
	}
}

func TestBridgeReceiptStoreRejectsForeignNamespace(t *testing.T) {
	blob := []byte(`{"namespace":"wlbs_state_v1","version":1,"receipt":{"key":"x"}}`)
	if _, err := NewBridgeReceiptStore(blob, func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("foreign namespace must be rejected, not adopted")
	}
	if _, err := NewBridgeReceiptStore([]byte(`{"receipt":`), func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("malformed blob must be rejected")
	}
}
