package main

import "wg-turn-client/accountaccess"

// Only an ID preference from installation-encrypted host storage, never cached authority.
type mobileSelectionPreference struct {
	NodeID         string `json:"node_id"`
	InstallationID string `json:"installation_id"`
	BaseURL        string `json:"base_url"`
	Environment    string `json:"environment"`
	AccountRef     string `json:"account_ref"`
}

// Consume once on the first fresh pair. A rejected/removed preference cannot revive
// on a later pair in this attempt; its empty catalog selection clears the host copy.
func (m *managedMobile) takeSelectionPreference(me accountaccess.MeResponse, catalog accountaccess.CatalogResponse) string {
	c := m.controller
	c.mu.Lock()
	preference := c.start.MobileSelection
	c.start.MobileSelection = nil
	selected := c.saved.SelectedNodeID
	c.mu.Unlock()
	if preference == nil || selected != "" {
		return ""
	}
	environment := c.start.MobileEnvironment
	if environment == "" {
		environment = "test"
	}
	account := ""
	if me.AccountRef != nil {
		account = *me.AccountRef
	}
	if preference.NodeID == "" || len(preference.NodeID) > 200 ||
		preference.InstallationID != m.fingerprint ||
		preference.BaseURL != c.start.MobileBaseURL || preference.Environment != environment ||
		preference.AccountRef != account {
		return ""
	}
	if !accountaccess.DecideAdmission(me, catalog, preference.NodeID, m.now()).Admitted {
		return ""
	}
	return preference.NodeID
}
