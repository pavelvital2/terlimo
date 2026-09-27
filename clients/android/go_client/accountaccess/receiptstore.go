package accountaccess

// Durable receipt persistence through the existing host persist channel. The payload is
// a separate versioned namespace: wlbs state/link/catalog are never touched, and Load
// refuses a foreign or absent namespace instead of adopting unknown bytes.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ReceiptNamespace is the versioned storage namespace for accountaccess receipts.
const ReceiptNamespace = "accountaccess_receipt_v1"

type receiptEnvelope struct {
	Namespace string  `json:"namespace"`
	Version   int     `json:"version"`
	Receipt   Receipt `json:"receipt"`
}

// PersistFunc durably stores bytes through the host bridge and returns only after the
// host acknowledged (persist_result). Failure means the memory copy must not advance.
type PersistFunc func(ctx context.Context, payload []byte) error

// BridgeReceiptStore implements ReceiptStore over one loaded startup blob plus the
// existing persist channel. Save persists first and updates the cache only on ack.
type BridgeReceiptStore struct {
	persist PersistFunc

	mu     sync.Mutex
	cached *Receipt
	loaded bool
	blob   []byte
}

// NewBridgeReceiptStore loads the startup blob (managedStart.accountaccess_state_b64).
func NewBridgeReceiptStore(blob []byte, persist PersistFunc) (*BridgeReceiptStore, error) {
	if persist == nil {
		return nil, fmt.Errorf("accountaccess persist channel required")
	}
	store := &BridgeReceiptStore{persist: persist, blob: blob}
	if len(blob) > 0 {
		receipt, err := decodeReceiptEnvelope(blob)
		if err != nil {
			return nil, err
		}
		store.cached = receipt
	}
	store.loaded = true
	return store, nil
}

func decodeReceiptEnvelope(blob []byte) (*Receipt, error) {
	var envelope receiptEnvelope
	if err := decodeStrict(blob, &envelope); err != nil {
		return nil, fmt.Errorf("accountaccess receipt decode: %w", err)
	}
	if envelope.Namespace != ReceiptNamespace || envelope.Version != 1 {
		return nil, fmt.Errorf("accountaccess receipt namespace/version mismatch")
	}
	return &envelope.Receipt, nil
}

// Load returns the durably loaded receipt for this namespace (nil when absent).
func (s *BridgeReceiptStore) Load() (*Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return nil, fmt.Errorf("accountaccess receipt store not loaded")
	}
	return s.cached, nil
}

// Save persists the receipt and only then advances the in-memory copy, so a failed or
// restarted write keeps the same-operation idempotency.
func (s *BridgeReceiptStore) Save(value Receipt) error {
	payload, err := json.Marshal(receiptEnvelope{Namespace: ReceiptNamespace, Version: 1, Receipt: value})
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := s.persist(ctx, payload); err != nil {
		return err
	}
	s.mu.Lock()
	copy := value
	s.cached = &copy
	s.mu.Unlock()
	return nil
}
