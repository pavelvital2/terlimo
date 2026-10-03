package main

import (
	"context"
	"errors"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/wlbs"
)

// This pair is installed only after project/store commit. The ordinary m.me/catalog
// fields are acquisition state and can contain an unaccepted intermediate response.
// Its context retires probes even on same-revision reissues or rejected replacements.
type mobileProbeSnapshot struct {
	me         accountaccess.MeResponse
	catalog    accountaccess.CatalogResponse
	subject    accountaccess.Subject
	generation string
	ctx        context.Context
	cancel     context.CancelFunc
}

func (m *managedMobile) retireProbeAdmissionLocked() {
	m.probeSerial++
	if m.probeSnapshot != nil {
		m.probeSnapshot.cancel()
		m.probeSnapshot = nil
	}
}

func (m *managedMobile) retireProbeAdmission() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retireProbeAdmissionLocked()
}

func (m *managedMobile) probeCandidateLocked(me accountaccess.MeResponse, catalog accountaccess.CatalogResponse) *mobileProbeSnapshot {
	snapshot := &mobileProbeSnapshot{me: me, catalog: catalog}
	if m.session != nil {
		snapshot.generation = m.session.Generation()
		snapshot.subject = m.session.Subject()
	}
	return snapshot
}

func (m *managedMobile) probeSessionMatches(snapshot *mobileProbeSnapshot) bool {
	return m.session == nil || (snapshot.generation != "" &&
		m.session.Generation() == snapshot.generation && m.session.Subject() == snapshot.subject)
}

func (m *managedMobile) acceptProbeAdmission(serial uint64, snapshot *mobileProbeSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if serial != m.probeSerial || !m.probeSessionMatches(snapshot) {
		return
	}
	parent := m.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	snapshot.ctx, snapshot.cancel = ctx, cancel
	m.probeSnapshot = snapshot
}

func (m *managedMobile) probeSnapshotCurrent(snapshot *mobileProbeSnapshot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return snapshot != nil && m.probeSnapshot == snapshot && snapshot.ctx.Err() == nil &&
		m.probeSessionMatches(snapshot)
}

type mobileProbeLease struct {
	node  wlbs.Node
	ctx   context.Context
	close func()
	check func() error
}

// admitProbe is read-only: no selection, store write, refresh, readiness asset or
// admission side effect. Each exact ID gets a local proof of the same accepted pair.
func (m *managedMobile) admitProbe(parent context.Context, id string) (*mobileProbeLease, error) {
	m.mu.Lock()
	snapshot := m.probeSnapshot
	m.mu.Unlock()
	if !m.probeSnapshotCurrent(snapshot) {
		return nil, errors.New("CATALOG_EXPIRED")
	}
	now := m.now()
	decision := accountaccess.DecideAdmission(snapshot.me, snapshot.catalog, id, now)
	if !decision.Admitted {
		return nil, errors.New(decision.Reason)
	}
	catalog, err := projectMobileCatalog(snapshot.me, snapshot.catalog, m.fingerprint, id, now)
	if err != nil {
		return nil, err
	}
	node, err := managedProbeNode(catalog, false, catalog.SubscriptionRef, id, now)
	if err != nil {
		return nil, err
	}
	deadline, _ := wlbs.UTC(catalog.CatalogExpiresAt)
	leaseEnd, _ := wlbs.UTC(node.Access.ExpiresAt)
	if leaseEnd.Before(deadline) {
		deadline = leaseEnd
	}
	if value := snapshot.me.GrantResolution.EffectiveDeadline; value != nil {
		end, err := wlbs.UTC(*value)
		if err != nil {
			return nil, err
		}
		if end.Before(deadline) {
			deadline = end
		}
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	stop := context.AfterFunc(snapshot.ctx, cancel)
	lease := &mobileProbeLease{node: node, ctx: ctx, close: func() { stop(); cancel() }}
	lease.check = func() error {
		if !m.probeSnapshotCurrent(snapshot) {
			return context.Canceled
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		return nil
	}
	if err := lease.check(); err != nil {
		lease.close()
		return nil, err
	}
	return lease, nil
}

// Keep pending-operation semantics for both direct and connected manual probes.
func (c *managedController) admitMobileProbe(ctx context.Context, id string, pending bool) (*mobileProbeLease, error) {
	if pending {
		return nil, errors.New("OPERATION_PENDING")
	}
	if c.mobile == nil {
		return nil, errors.New("MOBILE_STATE_UNAVAILABLE")
	}
	return c.mobile.admitProbe(ctx, id)
}
