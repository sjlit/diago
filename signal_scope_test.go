// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"testing"
	"time"

	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/require"
)

// Option scope is enforced by the type system: every API takes the
// per-operation option interface (AnswerOption, RegisterOption, ...) and the
// constructors implement only the interfaces they are valid for. See
// TestOptionInterfaceMatrix for the matrix and signal_options.go for the
// interfaces.
//
// TestSignalOptionValidation covers the uniform constructor value validation.
func TestSignalOptionValidation(t *testing.T) {
	cases := []struct {
		name string
		opt  SignalOption
	}{
		{"WithHeaders rejects nil header", WithHeaders(nil)},
		{"WithContact rejects nil", WithContact(nil)},
		{"WithBody rejects nil", WithBody(nil)},
		{"WithCodecs rejects empty list", WithCodecs()},
		{"WithRTPNAT rejects out of range", WithRTPNAT(2)},
		{"WithMediaBindIP rejects nil", WithMediaBindIP(nil)},
		{"WithMediaExternalIP rejects nil", WithMediaExternalIP(nil)},
		{"WithMediaDTLS rejects empty config", WithMediaDTLS(media.DTLSConfig{})},
		{"WithSDPSessionName rejects empty", WithSDPSessionName("")},
		{"WithMediaSession rejects nil", WithMediaSession(nil)},
		{"WithOnResponse rejects nil", WithOnResponse(nil)},
		{"WithOnMediaUpdate rejects nil", WithOnMediaUpdate(nil)},
		{"WithOnRefer rejects nil", WithOnRefer(nil)},
		{"WithOnReferNotify rejects nil", WithOnReferNotify(nil)},
		{"WithRequestMutator rejects nil", WithRequestMutator(nil)},
		{"WithResponseMutator rejects nil", WithResponseMutator(nil)},
		{"WithDialogTransport rejects empty", WithDialogTransport("")},
		{"WithDialogTransportID rejects empty", WithDialogTransportID("")},
		{"WithOriginator rejects nil", WithOriginator(nil)},
		{"WithAuthCredentials rejects empty", WithAuthCredentials("", "")},
		{"WithRegisterExpiry rejects negative", WithRegisterExpiry(-time.Second)},
		{"WithRegisterRetryInterval rejects negative", WithRegisterRetryInterval(-time.Second)},
		{"WithRegisterAllowHeaders rejects empty list", WithRegisterAllowHeaders()},
		{"WithRegisterAllowHeaders rejects empty value", WithRegisterAllowHeaders("")},
		{"WithRegisterProxyHost rejects empty", WithRegisterProxyHost("")},
		{"WithOnRegistered rejects nil", WithOnRegistered(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newSignalParams(tc.opt)
			require.ErrorIs(t, err, ErrInvalidOption)
		})
	}
}
