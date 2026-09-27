package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"wg-turn-client/servicechannel"
)

func TestRegistrationErrorClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "REG_LINK_OTHER"},
		{fmt.Errorf("wrapped: %w", servicechannel.ErrTransportTimeout), "REG_LINK_TIMEOUT"},
		{context.DeadlineExceeded, "REG_LINK_TIMEOUT"},
		{context.Canceled, "REG_LINK_CANCELED"},
		{fmt.Errorf("w: %w", servicechannel.ErrBadResponse), "REG_LINK_REPLY_FAILED"},
		{servicechannel.ErrResponseRejected, "REG_LINK_REPLY_FAILED"},
		{servicechannel.ErrTransportFailed, "REG_LINK_TRANSPORT_FAILED"},
		{fmt.Errorf("w: %w", servicechannel.ErrSeedMissing), "REG_LINK_SEED"},
		{servicechannel.ErrSeedBinding, "REG_LINK_SEED"},
		{servicechannel.ErrPathRejected, "REG_LINK_PATH_REJECTED"},
		{servicechannel.ErrRequestRejected, "REG_LINK_REQUEST_REJECTED"},
		{servicechannel.ErrOriginRejected, "REG_LINK_ORIGIN_REJECTED"},
		{&servicechannel.ServiceError{Code: "SERVICE_UNAVAILABLE"}, "REG_LINK_SERVICE_UNAVAILABLE"},
		{&servicechannel.ServiceError{Code: "SERVICE_PATH_DENIED"}, "REG_LINK_SERVICE_PATH_DENIED"},
		{&servicechannel.ServiceError{Code: "SOMETHING_ELSE"}, "REG_LINK_SERVICE_OTHER"},
		{errors.New("random"), "REG_LINK_OTHER"},
	}
	for _, tc := range cases {
		if got := registrationErrorClass(tc.err); got != tc.want {
			t.Fatalf("class(%v)=%q want %q", tc.err, got, tc.want)
		}
	}
}
