package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// FlowStateNamespace is the versioned bridge/AtomicFile namespace of the onboarding
// attempt identity. It is separate from wlbs state, the mobile receipt and the service
// seed: the same InstallationStore persist channel is reused, never the wlbs state.
const FlowStateNamespace = "onboarding_flow_v1"

// PersistFunc stores one namespace payload through the existing host persist seam.
type PersistFunc func(ctx context.Context, payload []byte) error

// flowEnvelope binds the durable attempt identity to one installation fingerprint and
// environment. A blob that does not match the current pair is ignored, so a restored
// or foreign file can never resume another installation's attempt.
type flowEnvelope struct {
	Version        int      `json:"v"`
	InstallationID string   `json:"installation_id"`
	Environment    string   `json:"environment"`
	State          KeyState `json:"state"`
}

// NewDurableStore loads the attempt identity from the host seed blob and persists every
// update through persist before returning. Connect persists a fresh request_key before
// the first network call, so a failed persist blocks the call instead of generating a
// new key. Only request_key/intent/credential metadata is stored: never a bootstrap
// secret, proof or any other credential material.
func NewDurableStore(seed []byte, installationID, environment string, persist PersistFunc) Store {
	store := &durableStore{installationID: installationID, environment: environment, persist: persist}
	if len(seed) == 0 {
		return store
	}
	var envelope flowEnvelope
	if json.Unmarshal(seed, &envelope) != nil || envelope.Version != 1 ||
		envelope.InstallationID != installationID || envelope.Environment != environment {
		return store
	}
	store.state, store.set = envelope.State, true
	return store
}

type durableStore struct {
	mu             sync.Mutex
	state          KeyState
	set            bool
	installationID string
	environment    string
	persist        PersistFunc
}

// Load returns the in-memory identity loaded from the durable seed.
func (d *durableStore) Load() (KeyState, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.set
}

// Save persists first and updates the in-memory state only when the write succeeded.
func (d *durableStore) Save(state KeyState) error {
	raw, err := json.Marshal(flowEnvelope{Version: 1, InstallationID: d.installationID,
		Environment: d.environment, State: state})
	if err != nil {
		return err
	}
	if d.persist == nil {
		return errors.New("ONBOARDING_STORE_UNAVAILABLE")
	}
	if err := d.persist(context.Background(), raw); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state, d.set = state, true
	return nil
}
