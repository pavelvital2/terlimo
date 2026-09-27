package main

// §§18–19 connected devices bridge vocabulary.
//
// Host -> native actions:
//
//	devices_list   no fields
//	device_delete  device_id, idempotency_key (host-owned)
//
// Native -> host events ({"v":1,"attempt_id":...,"type":<event>,"state":"ok"|"error"}):
//
//	devices_list_result
//	  ok: request_id, server_time, schema_version, device_limit, slots_used,
//	      revision, devices:[{device_id,name,is_current,platform,status,bound_at,revoked_at}]
//	device_delete_result
//	  ok: request_id, server_time, schema_version, status(ok|pending), operation_id,
//	      slot_released, access_application_state, residual_access_lease_seconds
//	  error: a bounded code (device error codes forwarded verbatim)
//
// No handler writes /me, admission or entitlement: the list and each deletion are read
// data only, and any access change is observed only through the next fresh /me read.

import (
	"context"
	"fmt"
	"os"

	"wg-turn-client/accountaccess"
)

const (
	devicesActionList   = "devices_list"
	devicesEventList    = "devices_list_result"
	devicesActionDelete = "device_delete"
	devicesEventDelete  = "device_delete_result"
)

var deviceForwardedCodes = map[string]bool{
	accountaccess.CodeDeviceRemoved:             true,
	accountaccess.CodeDeviceManagementForbidden: true,
	accountaccess.CodeDeviceNotFound:            true,
	accountaccess.CodeRevisionConflict:          true,
	accountaccess.CodeAccessSyncPending:         true,
	accountaccess.CodeServiceUnavailable:        true,
	accountaccess.CodeSessionExpired:            true,
	accountaccess.CodeSessionInvalid:            true,
	"ACCESS_DENIED": true, "NOT_FOUND": true, "INTERNAL": true,
}

func validDeviceBridgeID(value string) bool { return accountaccess.ValidDevicePathID(value) }

// validDeviceBridgeKey bounds the host-owned Idempotency-Key at the bridge boundary.
func validDeviceBridgeKey(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && !containsCRLF(value)
}

func deviceAPIErrorCode(apiError *accountaccess.ErrorResponse) string {
	if apiError != nil && deviceForwardedCodes[apiError.Code] {
		return apiError.Code
	}
	return "DEVICES_FAILED"
}

func (m *managedMobile) handleDevicesList(ctx context.Context, requestID string) {
	if m.client == nil {
		m.sendDeviceListError(requestID, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	devices, apiError, err := m.client.GetDevices(requestCtx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devicesdiag: TRANSPORT")
		m.sendDeviceListError(requestID, paymentTransportCode(err, false))
		return
	}
	if apiError != nil {
		fmt.Fprintln(os.Stderr, "devicesdiag: API_ERROR")
		m.sendDeviceListError(requestID, deviceAPIErrorCode(apiError))
		return
	}
	projected := make([]bridgeMessage, 0, len(devices.Devices))
	for _, d := range devices.Devices {
		entry := bridgeMessage{
			"device_id": d.DeviceID, "platform": d.Platform, "is_current": d.IsCurrent,
			"status": d.Status, "bound_at": d.BoundAt,
		}
		if d.Name != nil {
			entry["name"] = *d.Name
		} else {
			entry["name"] = nil
		}
		if d.RevokedAt != nil {
			entry["revoked_at"] = *d.RevokedAt
		} else {
			entry["revoked_at"] = nil
		}
		projected = append(projected, entry)
	}
	m.sendDevicesEvent(bridgeMessage{
		"type": devicesEventList, "state": "ok", "client_request_id": requestID,
		"request_id": devices.RequestID, "server_time": devices.ServerTime,
		"schema_version": accountaccess.SchemaVersion, "device_limit": devices.DeviceLimit,
		"slots_used": devices.SlotsUsed, "revision": devices.Revision, "devices": projected,
	})
}

func (m *managedMobile) handleDeviceDelete(ctx context.Context, req managedDeviceDelete) {
	if m.client == nil {
		m.sendDeviceDeleteError(req.RequestID, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	result, apiError, err := m.client.DeleteDevice(requestCtx, req.DeviceID, req.IdempotencyKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devicesdiag: TRANSPORT")
		m.sendDeviceDeleteError(req.RequestID, paymentTransportCode(err, false))
		return
	}
	if apiError != nil {
		fmt.Fprintln(os.Stderr, "devicesdiag: API_ERROR")
		m.sendDeviceDeleteError(req.RequestID, deviceAPIErrorCode(apiError))
		return
	}
	message := bridgeMessage{
		"type": devicesEventDelete, "state": "ok",
		"client_request_id": req.RequestID,
		"request_id": result.RequestID, "server_time": result.ServerTime,
		"schema_version": accountaccess.SchemaVersion, "status": result.Status,
		"operation_id": result.OperationID, "slot_released": result.SlotReleased,
		"access_application_state": result.AccessApplicationState,
	}
	if result.ResidualAccessLeaseSeconds != nil {
		message["residual_access_lease_seconds"] = *result.ResidualAccessLeaseSeconds
	} else {
		message["residual_access_lease_seconds"] = nil
	}
	m.sendDevicesEvent(message)
}

func (m *managedMobile) sendDevicesEvent(message bridgeMessage) {
	if m.bridge != nil {
		_ = m.bridge.send(message)
	}
}

func (m *managedMobile) sendDevicesError(event, code string) {
	m.sendDevicesEvent(bridgeMessage{"type": event, "state": "error", "code": code})
}

// sendDeviceListError echoes the host-owned correlation id on the list error event.
func (m *managedMobile) sendDeviceListError(requestID, code string) {
	m.sendDevicesEvent(bridgeMessage{"type": devicesEventList, "state": "error",
		"code": code, "client_request_id": requestID})
}

// sendDeviceDeleteError echoes the host-owned correlation id on the delete error event.
func (m *managedMobile) sendDeviceDeleteError(requestID, code string) {
	m.sendDevicesEvent(bridgeMessage{"type": devicesEventDelete, "state": "error",
		"code": code, "client_request_id": requestID})
}

// compile-time guard that the strict decoders stay the contract source.
var _ = accountaccess.DevicesResponse{}
var _ = accountaccess.DeviceDeleteResponse{}
