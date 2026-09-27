package main

// S5 §11 announcements bridge vocabulary.
//
// Host -> native actions:
//
//	announcements_list   no fields
//	announcement_read    announcement_id (1..128), idempotency_key (16..128)
//
// Native -> host events, shaped {"v":1,"attempt_id":...,"type":<event>,"state":"ok"|"error"}
// either with the contract payload or with a single bounded "code":
//
//	announcements_list
//	  ok:    request_id, server_time, schema_version,
//	         announcements:[{announcement_id,title,text,unread,revision,valid_until(string|null),
//	                          action:{type,label?}}], unread_count
//	announcement_read
//	  ok:    request_id, server_time, schema_version, announcement_id, read, read_at
//
// Both are display-only reads: they never mutate /me, admission, entitlement or the
// tunnel, and they reuse the existing installation-PoP session. The host owns the
// Idempotency-Key; native forwards it verbatim and never generates or rotates one.
// Bounded error codes: MOBILE_STATE_UNAVAILABLE, TRANSPORT, INVALID_REQUEST plus the
// schemas/errors.json codes forwarded verbatim.

import (
	"context"

	"wg-turn-client/accountaccess"
)

const (
	announcementsActionList = "announcements_list"
	announcementsEventList  = "announcements_list"
	announcementsActionRead = "announcement_read"
	announcementsEventRead  = "announcement_read"
)

// validAnnouncementBridgeID bounds the announcement id at the bridge boundary with the
// exact same unreserved-segment grammar the accountaccess client and the service-channel
// wire allowlist use, so an id that could not be forwarded is refused here (fail-closed)
// and never sent to be rejected late. It also rejects CR/LF by construction.
func validAnnouncementBridgeID(value string) bool {
	return accountaccess.ValidAnnouncementPathID(value)
}

// validAnnouncementBridgeKey bounds the host-owned Idempotency-Key at the bridge
// boundary: 16..128 characters, no CR/LF.
func validAnnouncementBridgeKey(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && !containsCRLF(value)
}

func containsCRLF(value string) bool {
	for _, r := range value {
		if r == '\r' || r == '\n' {
			return true
		}
	}
	return false
}

// handleAnnouncementsList performs one bounded authenticated GET /announcements and
// answers with exactly one announcements_list event.
func (m *managedMobile) handleAnnouncementsList(ctx context.Context) {
	if m.client == nil {
		m.sendAnnouncementsError(announcementsEventList, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	list, apiError, err := m.client.ListAnnouncements(requestCtx)
	if err != nil {
		m.sendAnnouncementsError(announcementsEventList, paymentTransportCode(err, false))
		return
	}
	if apiError != nil {
		m.sendAnnouncementsError(announcementsEventList, paymentAPIErrorCode(apiError, false))
		return
	}
	items := make([]bridgeMessage, 0, len(list.Announcements))
	for _, announcement := range list.Announcements {
		action := bridgeMessage{"type": announcement.Action.Type}
		if announcement.Action.Label != nil {
			action["label"] = *announcement.Action.Label
		}
		item := bridgeMessage{
			"announcement_id": announcement.AnnouncementID,
			"title":           announcement.Title,
			"text":            announcement.Text,
			"unread":          announcement.Unread,
			"revision":        announcement.Revision,
			"action":          action,
		}
		// Honest nullable date: a null valid_until travels as JSON null, never replaced
		// by server_time or a fabricated expiry.
		if announcement.ValidUntil != nil {
			item["valid_until"] = *announcement.ValidUntil
		} else {
			item["valid_until"] = nil
		}
		items = append(items, item)
	}
	m.sendAnnouncementsEvent(bridgeMessage{
		"type":           announcementsEventList,
		"state":          "ok",
		"request_id":     list.RequestID,
		"server_time":    list.ServerTime,
		"schema_version": list.SchemaVersion,
		"announcements":  items,
		"unread_count":   list.UnreadCount,
	})
}

// handleAnnouncementRead posts one idempotent read marker and answers with exactly one
// announcement_read event. A repeat with the same host-owned key is the server's
// idempotent replay, never a native rewrite.
func (m *managedMobile) handleAnnouncementRead(ctx context.Context, request managedAnnouncementRead) {
	if m.client == nil {
		m.sendAnnouncementsError(announcementsEventRead, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	marker, apiError, err := m.client.MarkAnnouncementRead(requestCtx, request.AnnouncementID, request.IdempotencyKey)
	if err != nil {
		m.sendAnnouncementsError(announcementsEventRead, paymentTransportCode(err, false))
		return
	}
	if apiError != nil {
		m.sendAnnouncementsError(announcementsEventRead, paymentAPIErrorCode(apiError, false))
		return
	}
	m.sendAnnouncementsEvent(bridgeMessage{
		"type":            announcementsEventRead,
		"state":           "ok",
		"request_id":      marker.RequestID,
		"server_time":     marker.ServerTime,
		"schema_version":  marker.SchemaVersion,
		"announcement_id": marker.AnnouncementID,
		"read":            marker.Read,
		"read_at":         marker.ReadAt,
	})
}

func (m *managedMobile) sendAnnouncementsEvent(message bridgeMessage) {
	if m.bridge != nil {
		_ = m.bridge.send(message)
	}
}

func (m *managedMobile) sendAnnouncementsError(event, code string) {
	m.sendAnnouncementsEvent(bridgeMessage{"type": event, "state": "error", "code": code})
}

// compile-time guard that the strict decoder stays the single source of the contract.
var _ = accountaccess.AnnouncementsResponse{}
