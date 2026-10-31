// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestOptionsStructValidation(t *testing.T) {
	_, err := messageSignalParams([]Header{nil}, nil, nil, nil, nil)
	require.ErrorIs(t, err, ErrInvalidOption)

	_, err = singleSignalParams("Ack", []AckOptions{{}, {}}, AckOptions.signalParams)
	require.ErrorIs(t, err, ErrInvalidOption)
}

func TestAckOptionsHeaders(t *testing.T) {
	var ackReq *sip.Request
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		switch req.Method {
		case sip.ACK:
			ackReq = req
		case sip.INVITE:
			return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
		}
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := dg.NewCall(sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	defer d.release()
	require.NoError(t, d.Invite(ctx))
	require.NoError(t, d.Ack(ctx, AckOptions{Headers: []Header{sip.NewHeader("X-Ack", "1")}}))

	require.NotNil(t, ackReq)
	require.NotNil(t, ackReq.GetHeader("X-Ack"))
}
