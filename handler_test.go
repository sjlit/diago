// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerErrorResponseMapping(t *testing.T) {
	t.Run("sentinels", func(t *testing.T) {
		cases := []struct {
			err  error
			code int
		}{
			{ErrDecline, sip.StatusGlobalDecline},
			{ErrTemporarilyUnavailable, sip.StatusTemporarilyUnavailable},
			{ErrBusy, sip.StatusBusyHere},
			{ErrNotFound, sip.StatusNotFound},
			{errors.New("boom"), sip.StatusInternalServerError},
			{errors.Join(errors.New("ctx"), ErrBusy), sip.StatusBusyHere},
		}
		for _, tc := range cases {
			code, _, _ := handlerErrorResponse(tc.err)
			assert.Equal(t, tc.code, code, "error %v", tc.err)
		}
	})

	t.Run("response error", func(t *testing.T) {
		resErr, err := NewResponseError(402, "Payment Required", sip.NewHeader("X-Test", "1"))
		require.NoError(t, err)
		code, reason, headers := handlerErrorResponse(resErr)
		assert.Equal(t, 402, code)
		assert.Equal(t, "Payment Required", reason)
		require.Len(t, headers, 1)

		// A non-final status is rejected at construction.
		_, err = NewResponseError(200, "")
		assert.ErrorIs(t, err, ErrInvalidResponseCode)

		// Empty reason gets a default phrase.
		resErr, err = NewResponseError(480, "")
		require.NoError(t, err)
		code, reason, _ = handlerErrorResponse(resErr)
		assert.Equal(t, sip.StatusTemporarilyUnavailable, code)
		assert.Equal(t, "Temporarily Unavailable", reason)
	})
}

func TestDialogReasonHeader(t *testing.T) {
	assert.Equal(t, `SIP ;cause=486 ;text="Busy Here"`, dialogReasonHeader(486, "Busy Here"))
}

// requireDialogResponseError asserts err carries an INVITE final response and
// returns it. sipgo returns both values and pointers in different paths.
func requireDialogResponseError(t *testing.T, err error) *sip.Response {
	t.Helper()

	var ptr *sipgo.ErrDialogResponse
	if errors.As(err, &ptr) {
		return ptr.Res
	}
	var val sipgo.ErrDialogResponse
	require.ErrorAs(t, err, &val)
	return val.Res
}

func TestIntegrationHandlerErrorMapping(t *testing.T) {
	skipShort(t)

	cases := []struct {
		name     string
		handler  CallHandler
		wantCode int
	}{
		{
			name:     "decline",
			handler:  func(ctx context.Context, d *InboundCall) error { return ErrDecline },
			wantCode: sip.StatusGlobalDecline,
		},
		{
			name:     "temporarily unavailable",
			handler:  func(ctx context.Context, d *InboundCall) error { return ErrTemporarilyUnavailable },
			wantCode: sip.StatusTemporarilyUnavailable,
		},
		{
			name:     "busy",
			handler:  func(ctx context.Context, d *InboundCall) error { return ErrBusy },
			wantCode: sip.StatusBusyHere,
		},
		{
			name:     "not found",
			handler:  func(ctx context.Context, d *InboundCall) error { return ErrNotFound },
			wantCode: sip.StatusNotFound,
		},
		{
			name: "custom response",
			handler: func(ctx context.Context, d *InboundCall) error {
				resErr, err := NewResponseError(402, "Payment Required")
				require.NoError(t, err)
				return resErr
			},
			wantCode: 402,
		},
		{
			name: "generic error",
			handler: func(ctx context.Context, d *InboundCall) error {
				return errors.New("internal boom")
			},
			wantCode: sip.StatusInternalServerError,
		},
		{
			// A handler context error while the dialog is alive must not
			// leave the INVITE unanswered.
			name:     "handler context error",
			handler:  func(ctx context.Context, d *InboundCall) error { return context.DeadlineExceeded },
			wantCode: sip.StatusInternalServerError,
		},
		{
			// A handler that returns without answering declines the pending
			// INVITE: the call must not silently hang.
			name:     "nil return unanswered declines",
			handler:  func(ctx context.Context, d *InboundCall) error { return nil },
			wantCode: sip.StatusTemporarilyUnavailable,
		},
		{
			name:     "nil handler",
			handler:  nil, // Serve maps nil to the 503 default handler
			wantCode: sip.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			uaSrv, err := sipgo.NewUA(sipgo.WithUserAgent("server"))
			require.NoError(t, err)
			t.Cleanup(func() { uaSrv.Close() })
			_, srvPort := servingAgent(t, ctx, tc.handler,
				withUserAgent(uaSrv),
				withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
			)

			uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
			require.NoError(t, err)
			t.Cleanup(func() { uaCli.Close() })
			dgCli, err := newAgent(withUserAgent(uaCli), withTransport(Transport{
				Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
			}))
			require.NoError(t, err)

			_, err = dgCli.DialURI(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: srvPort})
			require.Error(t, err)

			res := requireDialogResponseError(t, err)
			assert.Equal(t, tc.wantCode, res.StatusCode)
		})
	}
}

func TestIntegrationHandlerContextCanceled(t *testing.T) {
	skipShort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	uaSrv, err := sipgo.NewUA(sipgo.WithUserAgent("server"))
	require.NoError(t, err)
	t.Cleanup(func() { uaSrv.Close() })

	handlerCtxErr := make(chan error, 1)
	_, srvPort := servingAgent(t, ctx, func(hctx context.Context, d *InboundCall) error {
		if err := d.Answer(ctx); err != nil {
			return err
		}
		<-hctx.Done()
		handlerCtxErr <- hctx.Err()
		return nil
	}, withUserAgent(uaSrv), withTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))

	uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
	require.NoError(t, err)
	t.Cleanup(func() { uaCli.Close() })
	dgCli, err := newAgent(withUserAgent(uaCli), withTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))
	require.NoError(t, err)

	dlg, err := dgCli.DialURI(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: srvPort})
	require.NoError(t, err)
	defer dlg.Close()

	require.NoError(t, dlg.Hangup(ctx))

	select {
	case err := <-handlerCtxErr:
		assert.ErrorIs(t, err, context.Canceled, "handler ctx must be canceled when the dialog ends")
	case <-time.After(5 * time.Second):
		t.Fatal("handler context was not canceled after the dialog ended")
	}
}

func TestIntegrationHandlerErrorAfterAnswerSendsByeReason(t *testing.T) {
	skipShort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	uaSrv, err := sipgo.NewUA(sipgo.WithUserAgent("server"))
	require.NoError(t, err)
	t.Cleanup(func() { uaSrv.Close() })

	_, srvPort := servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
		if err := d.Answer(ctx); err != nil {
			return err
		}
		// The call was answered: the error must turn into BYE + Reason header.
		return ErrBusy
	}, withUserAgent(uaSrv), withTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))

	var mu sync.Mutex
	var byeReq *sip.Request
	uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
	require.NoError(t, err)
	t.Cleanup(func() { uaCli.Close() })
	dgCli, err := newAgent(withUserAgent(uaCli),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		withServerRequestMiddleware(func(next sipgo.RequestHandler) sipgo.RequestHandler {
			return func(req *sip.Request, tx sip.ServerTransaction) {
				if req.Method == sip.BYE {
					mu.Lock()
					byeReq = req.Clone()
					mu.Unlock()
				}
				next(req, tx)
			}
		}),
	)
	require.NoError(t, err)
	require.NoError(t, dgCli.ServeBackground(ctx, nil))

	dlg, err := dgCli.DialURI(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: srvPort})
	require.NoError(t, err)
	defer dlg.Close()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := byeReq
		mu.Unlock()
		if got != nil {
			reason := got.GetHeader("Reason")
			require.NotNil(t, reason, "BYE must carry a Reason header")
			assert.Contains(t, reason.Value(), "cause=486")
			assert.Contains(t, reason.Value(), "Busy Here")
			break
		}

		select {
		case <-deadline:
			t.Fatal("BYE with Reason header was not observed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	select {
	case <-dlg.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client dialog did not end after server BYE")
	}
}

// TestIntegrationHandlerAnswerReturnKeepsCallAlive locks the v3 handler
// semantics: a handler that answers and returns must NOT end the call. The
// call stays up until the peer hangs up, and the teardown is a BYE.
func TestIntegrationHandlerAnswerReturnKeepsCallAlive(t *testing.T) {
	skipShort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uaSrv, err := sipgo.NewUA(sipgo.WithUserAgent("server"))
	require.NoError(t, err)
	t.Cleanup(func() { uaSrv.Close() })

	handlerReturned := make(chan struct{})
	var serverBye atomic.Int32
	dgSrv, err := newAgent(withUserAgent(uaSrv),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		withServerRequestMiddleware(func(next sipgo.RequestHandler) sipgo.RequestHandler {
			return func(req *sip.Request, tx sip.ServerTransaction) {
				if req.Method == sip.BYE {
					serverBye.Add(1)
				}
				next(req, tx)
			}
		}),
	)
	require.NoError(t, err)
	require.NoError(t, dgSrv.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		if err := d.Answer(ctx); err != nil {
			return err
		}
		close(handlerReturned)
		// Returning must keep the call alive.
		return nil
	}))
	srvPort := dgSrv.transports[0].BindPort

	var clientBye atomic.Int32
	uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
	require.NoError(t, err)
	t.Cleanup(func() { uaCli.Close() })
	dgCli, err := newAgent(withUserAgent(uaCli),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		withServerRequestMiddleware(func(next sipgo.RequestHandler) sipgo.RequestHandler {
			return func(req *sip.Request, tx sip.ServerTransaction) {
				if req.Method == sip.BYE {
					clientBye.Add(1)
				}
				next(req, tx)
			}
		}),
	)
	require.NoError(t, err)
	require.NoError(t, dgCli.ServeBackground(ctx, nil))

	dlg, err := dgCli.DialURI(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: srvPort})
	require.NoError(t, err)

	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}

	// The server handler returned: the call must still be alive.
	select {
	case <-dlg.Context().Done():
		t.Fatal("call ended after the handler returned; it must stay alive")
	case <-time.After(500 * time.Millisecond):
	}
	assert.EqualValues(t, 0, clientBye.Load(), "framework must not send BYE when the handler returns")
	assert.EqualValues(t, 0, serverBye.Load(), "peer has not hung up yet")

	// The owner ends the call; the client leg observes the BYE.
	require.NoError(t, dlg.Hangup(ctx))
	select {
	case <-dlg.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client dialog did not end after Hangup")
	}
	require.Eventually(t, func() bool { return serverBye.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"server must receive the client's BYE")

	// The framework closes the server dialog when it ends.
	require.Eventually(t, func() bool {
		n := 0
		dgSrv.cache.server.rangeDialogs(ctx, func(id string, d *InboundCall) bool {
			n++
			return true
		})
		return n == 0
	}, 5*time.Second, 20*time.Millisecond, "server dialog must be evicted when the call ends")
}

func TestHangupCloseClient(t *testing.T) {
	var byeCount atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if req.Method == sip.BYE {
			byeCount.Add(1)
		}
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d, err := dg.DialURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.Equal(t, 1, cacheCountClientDialogs(dg))

	require.NoError(t, d.Hangup(ctx))
	assert.EqualValues(t, 1, byeCount.Load(), "Hangup must send BYE")
	require.NoError(t, d.Close())
	assert.Equal(t, 0, cacheCountClientDialogs(dg), "Close must evict the dialog")

	// Both are idempotent.
	require.NoError(t, d.Hangup(ctx))
	require.NoError(t, d.Close())
}
