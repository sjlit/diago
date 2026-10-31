// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestCallStateFromDialog(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "a", Host: "b"})
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	d := &sipgo.Dialog{ID: "state", InviteRequest: req, InviteResponse: res}
	require.Equal(t, CallStateNew, callStateFromDialog(d))

	d.InitWithState(sip.DialogStateEstablished)
	require.Equal(t, CallStateAnswered, callStateFromDialog(d))

	d.InitWithState(sip.DialogStateConfirmed)
	require.Equal(t, CallStateConfirmed, callStateFromDialog(d))

	d.InitWithState(sip.DialogStateEnded)
	require.Equal(t, CallStateEnded, callStateFromDialog(d))

	// A provisional response wins over the dialog state: it is early media.
	d.InviteResponse = sip.NewResponseFromRequest(req, 183, "Session Progress", nil)
	d.InitWithState(sip.DialogStateEstablished)
	require.Equal(t, CallStateEarly, callStateFromDialog(d))
}

func TestCallStateNilSafe(t *testing.T) {
	var in *InboundCall
	require.Equal(t, CallStateEnded, in.State())
	var out *OutboundCall
	require.Equal(t, CallStateEnded, out.State())
}
