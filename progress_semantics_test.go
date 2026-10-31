// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProgressSemantics locks the v2 split: Trying sends 100, Progress sends
// 183 without SDP (no early media negotiation), and Answer still completes.
func TestProgressSemantics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type seen struct {
		code   int
		hasSDP bool
	}
	var mu sync.Mutex
	var responses []seen

	uaSrv, _ := sipgo.NewUA()
	defer uaSrv.Close()
	dgSrv, err := newAgent(withUserAgent(uaSrv), withTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))
	require.NoError(t, err)
	err = dgSrv.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		if err := d.Trying(ctx); err != nil {
			return err
		}
		if err := d.Progress(ctx); err != nil {
			return err
		}
		// Gap between the provisional and the final response, so the client
		// observes the early media session before the answer replaces it.
		time.Sleep(100 * time.Millisecond)
		return d.Answer(ctx)
	})
	require.NoError(t, err)
	// Ephemeral bind: use the port reported by the listener to avoid clashes
	// with other tests/processes sharing fixed ports.
	srvPort := dgSrv.transports[0].BindPort
	require.NotZero(t, srvPort)

	uaCli, _ := sipgo.NewUA()
	defer uaCli.Close()
	dgCli, err := newAgent(withUserAgent(uaCli), withTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))
	require.NoError(t, err)

	d, err := dgCli.DialURI(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: srvPort},
		WithOnResponse(func(res *sip.Response) error {
			mu.Lock()
			responses = append(responses, seen{code: res.StatusCode, hasSDP: res.Body() != nil})
			mu.Unlock()
			return nil
		}),
	)
	require.NoError(t, err)
	defer func() {
		d.Close(ctx)
		d.release()
	}()

	mu.Lock()
	defer mu.Unlock()

	var codes []int
	for _, r := range responses {
		codes = append(codes, r.code)
		if r.code == sip.StatusSessionInProgress {
			assert.False(t, r.hasSDP, "Progress must not attach SDP")
		}
	}
	assert.Contains(t, codes, sip.StatusTrying)
	assert.Contains(t, codes, sip.StatusSessionInProgress)
	assert.Contains(t, codes, sip.StatusOK)
}
