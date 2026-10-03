package main

import (
	"context"
	"strconv"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

type optionalSeedEpochKey struct{}

// Called synchronously by the existing Runner, after catalogue publication/finish.
// No new worker, HTTP client, reauthentication, dial or retry belongs to this read.
func (m *managedMobile) updateSeedAtIdle(ctx context.Context, nextCycle time.Time) bool {
	if !m.idleSeedPublished {
		return false
	}
	if m.seedDoer == nil || m.seedKey == "" || m.bridge == nil {
		return true
	}
	key, err := servicechannel.DecodeRecoveryVerifyKey(m.seedKey)
	if err != nil {
		return true
	}
	bearer, subject, generation, ok := m.session.CachedBearer()
	if !ok {
		return true
	}
	m.bridge.mu.Lock()
	// Commands already queued for the existing dispatchers take priority.
	pending := len(m.bridge.selection)+len(m.bridge.explicit)+len(m.bridge.switchNode)+len(m.bridge.payments)+
		len(m.bridge.registration)+len(m.bridge.usage)+len(m.bridge.devices)+len(m.bridge.deviceDelete)+
		len(m.bridge.announcements)+len(m.bridge.announcementRead)+len(m.bridge.refreshManual)+len(m.bridge.wake)+
		len(m.bridge.preference)+len(m.bridge.probe)+len(m.bridge.probeStop) > 0 || m.bridge.seedReceiptBusy.Load()
	if pending {
		m.bridge.mu.Unlock()
		return false
	}
	optional, finish, ok := m.seedDoer.BeginOptional(ctx)
	epoch := m.bridge.seedUpdateEpoch
	m.bridge.mu.Unlock()
	if !ok {
		return false
	}
	defer finish()
	deadline := time.Now().Add(mobileHTTPTimeout)
	if nextCycle.Before(deadline) {
		deadline = nextCycle
	}
	optional, stop := context.WithDeadline(optional, deadline)
	defer stop()
	optional = context.WithValue(optional, optionalSeedEpochKey{}, epoch)
	optional = accountaccess.WithExistingBearer(optional, bearer)
	response, apiError, err := m.client.GetServiceSeed(optional)
	if err != nil || apiError != nil || optional.Err() != nil {
		return true
	}
	tokenAfter, subjectAfter, generationAfter, current := m.session.CachedBearer()
	if !current || tokenAfter != bearer || subjectAfter != subject || generationAfter != generation {
		return true
	}
	environment := m.controller.start.MobileEnvironment
	if environment == "" {
		environment = "test"
	}
	seed, err := servicechannel.VerifyRecoveryCode(response.RecoveryCode, key, environment)
	if err != nil {
		return true
	}
	// Signature is necessary; current authorized response and host epoch fence are too.
	if optional.Err() == nil {
		_ = m.seedDoer.Seeds.CommitRecovery(optional, seed)
	}
	return true
}

func serviceControlCommand(kind string) bool {
	switch kind {
	case "device_sleep", "device_wake", "cancel", "select_node", "explicit_connect",
		"request_telegram_registration", "refresh_telegram_registration", "activate_trial",
		"plans_list", "quote_create", "payment_create", "payment_get", "usage_read",
		"announcements_list", "announcement_read", "devices_list", "device_delete",
		"cancel_catalog", "refresh_manual", "switch_node", "choose_node", "probe_node", "cancel_probe":
		return true
	}
	return false
}

// Metadata is checked and removed before legacy frozen message-shape validators.
func (b *managedBridge) preemptSeedUpdate(m bridgeMessage) bool {
	raw, present := m["seed_update_epoch"]
	var epoch uint64
	if present {
		text, ok := raw.(string)
		if !ok || len(text) > 19 {
			return false
		}
		var err error
		epoch, err = strconv.ParseUint(text, 10, 63)
		if err != nil {
			return false
		}
		delete(m, "seed_update_epoch")
	}
	if !serviceControlCommand(m.string("type")) {
		return !present
	}
	b.mu.Lock()
	b.seedReceiptBusy.Store(true)
	if epoch > b.seedUpdateEpoch {
		b.seedUpdateEpoch = epoch
	}
	if b.preemptSeed != nil {
		b.preemptSeed()
	}
	b.mu.Unlock()
	return true
}

// The receipt stays busy through enqueue; optional registration cannot slip between
// early cancellation and publication to an existing foreground dispatcher.
func (b *managedBridge) finishSeedReceipt() { b.seedReceiptBusy.Store(false) }
