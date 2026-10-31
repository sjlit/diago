// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestOutboundCloseSendsByeOnce(t *testing.T) {
	var byes atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		switch req.Method {
		case sip.BYE:
			byes.Add(1)
		case sip.INVITE:
			return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
		}
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := dg.DialURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.Equal(t, CallStateConfirmed, d.State())

	require.NoError(t, d.Close(ctx))
	require.EqualValues(t, 1, byes.Load(), "Close must send BYE")

	// Close is idempotent and must not emit a second BYE.
	require.NoError(t, d.Close(ctx))
	require.EqualValues(t, 1, byes.Load())
}
