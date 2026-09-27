package main

// S5 §07.4 account-traffic bridge vocabulary.
//
// Host -> native action:
//
//	usage_read   no fields
//
// Native -> host event, shaped {"v":1,"attempt_id":...,"type":"usage_result","state":
// "ok"|"error"} either with the contract payload or with a single bounded "code":
//
//	usage_result
//	  ok:    request_id, server_time, schema_version, timezone, as_of,
//	         coverage_start (string|null),
//	         buckets:[{period,rx_bytes,tx_bytes,complete}]
//
// The read is display-only: it never mutates /me, admission, entitlement or the tunnel.
// It reuses the existing installation-PoP session (scope session:read); no bearer, key or
// raw transport text ever reaches the host. Bounded error codes: MOBILE_STATE_UNAVAILABLE
// and TRANSPORT plus the schemas/errors.json codes forwarded verbatim.

import (
	"context"
	"errors"

	"wg-turn-client/servicechannel"
)

const (
	usageActionRead = "usage_read"
	usageEventRead  = "usage_result"
)

// handleUsageRead performs one bounded authenticated GET /usage and answers with exactly
// one usage_result event. It mirrors the payment read path: display-only, no state write.
func (m *managedMobile) handleUsageRead(ctx context.Context) {
	if m.client == nil {
		m.sendUsageError(usageEventRead, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	usage, apiError, err := m.client.GetUsage(requestCtx)
	if err != nil {
		// Bounded cause on stderr for the next run; public code stays TRANSPORT.
		emitUsageStage(usageCauseToken(err))
		m.sendUsageError(usageEventRead, paymentTransportCode(err, false))
		return
	}
	if apiError != nil {
		m.sendUsageError(usageEventRead, paymentAPIErrorCode(apiError, false))
		return
	}
	buckets := make([]bridgeMessage, 0, len(usage.Buckets))
	for _, bucket := range usage.Buckets {
		buckets = append(buckets, bridgeMessage{
			"period": bucket.Period, "rx_bytes": bucket.RXBytes,
			"tx_bytes": bucket.TXBytes, "complete": bucket.Complete,
		})
	}
	message := bridgeMessage{
		"type": usageEventRead, "state": "ok",
		"request_id": usage.RequestID, "server_time": usage.ServerTime,
		"schema_version": usage.SchemaVersion, "timezone": usage.Timezone,
		"buckets": buckets,
	}
	// Honest nullable times: a null as_of/coverage_start travels as JSON null and is never
	// replaced by server_time or a fabricated history start.
	if usage.AsOf != nil {
		message["as_of"] = *usage.AsOf
	} else {
		message["as_of"] = nil
	}
	if usage.CoverageStart != nil {
		message["coverage_start"] = *usage.CoverageStart
	} else {
		message["coverage_start"] = nil
	}
	m.sendUsageEvent(message)
}

// usageCauseToken classifies a failed /usage read into a bounded, secret-free diagnostic
// token emitted on stderr (usagestage:). The public usage_result error code stays the
// frozen, host-accepted set (TRANSPORT/MOBILE_STATE_UNAVAILABLE); this token only locates
// the first failing layer for the next run. Raw error text never travels.
func usageCauseToken(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, servicechannel.ErrSeedMissing):
		return "FAIL_LOCAL_SEED_MISSING"
	case errors.Is(err, servicechannel.ErrOriginRejected):
		return "FAIL_LOCAL_ORIGIN_REJECTED"
	case errors.Is(err, servicechannel.ErrPathRejected):
		return "FAIL_LOCAL_PATH_REJECTED"
	case errors.Is(err, servicechannel.ErrRequestRejected):
		return "FAIL_LOCAL_REQUEST_REJECTED"
	case errors.Is(err, servicechannel.ErrResponseRejected):
		return "FAIL_SERVICE_RESPONSE_REJECTED"
	}
	var serviceErr *servicechannel.ServiceError
	if errors.As(err, &serviceErr) {
		switch serviceErr.Code {
		case "SERVICE_PATH_DENIED":
			return "FAIL_SERVICE_PATH_DENIED"
		case "SERVICE_UNAVAILABLE":
			return "FAIL_SERVICE_UNAVAILABLE"
		case "TRANSPORT_TIMEOUT":
			return "FAIL_TRANSPORT_TIMEOUT"
		case "TRANSPORT_FAILED":
			return "FAIL_TRANSPORT_FAILED"
		case "TRUST_FAILED":
			return "FAIL_TRUST_FAILED"
		case "VK_API_UNAVAILABLE":
			return "FAIL_VK_API_UNAVAILABLE"
		}
		return "FAIL_SERVICE_ERROR"
	}
	return "FAIL_TRANSPORT"
}

func (m *managedMobile) sendUsageEvent(message bridgeMessage) {
	if m.bridge != nil {
		_ = m.bridge.send(message)
	}
}

func (m *managedMobile) sendUsageError(event, code string) {
	m.sendUsageEvent(bridgeMessage{"type": event, "state": "error", "code": code})
}
