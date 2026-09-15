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
	t.Run("Sentinels", func(t *testing.T) {
		cases := []struct {
			err  error
			code int
		}{
			{ErrDecline, sip.StatusTemporarilyUnavailable},
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

	t.Run("ResponseError", func(t *testing.T) {
		code, reason, headers := handlerErrorResponse(&ResponseError{
			Code:    402,
			Reason:  "Payment Required",
			Headers: []sip.Header{sip.NewHeader("X-Test", "1")},
		})
		assert.Equal(t, 402, code)
		assert.Equal(t, "Payment Required", reason)
		require.Len(t, headers, 1)

		// A 2xx handler error is coerced to 500 to keep the dialog consistent.
		code, _, _ = handlerErrorResponse(&ResponseError{Code: 200})
		assert.Equal(t, sip.StatusInternalServerError, code)

		// Empty reason gets a default phrase.
		code, reason, _ = handlerErrorResponse(&ResponseError{Code: 480})
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
		handler  DialogHandler
		wantCode int
	}{
		{
			name:     "Decline",
			handler:  func(ctx context.Context, d *DialogServerSession) error { return ErrDecline },
			wantCode: sip.StatusTemporarilyUnavailable,
		},
		{
			name:     "Busy",
			handler:  func(ctx context.Context, d *DialogServerSession) error { return ErrBusy },
			wantCode: sip.StatusBusyHere,
		},
		{
			name:     "NotFound",
			handler:  func(ctx context.Context, d *DialogServerSession) error { return ErrNotFound },
			wantCode: sip.StatusNotFound,
		},
		{
			name: "CustomResponse",
			handler: func(ctx context.Context, d *DialogServerSession) error {
				return &ResponseError{Code: 402, Reason: "Payment Required"}
			},
			wantCode: 402,
		},
		{
			name: "GenericError",
			handler: func(ctx context.Context, d *DialogServerSession) error {
				return errors.New("internal boom")
			},
			wantCode: sip.StatusInternalServerError,
		},
		{
			// A handler context error while the dialog is alive must not
			// leave the INVITE unanswered.
			name:     "HandlerContextError",
			handler:  func(ctx context.Context, d *DialogServerSession) error { return context.DeadlineExceeded },
			wantCode: sip.StatusInternalServerError,
		},
		{
			name:     "NilHandler",
			handler:  nil, // Serve maps nil to the 503 default handler
			wantCode: sip.StatusServiceUnavailable,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			srvPort := 15210 + i*2
			cliPort := srvPort + 1

			uaSrv, err := sipgo.NewUA(sipgo.WithUserAgent("server"))
			require.NoError(t, err)
			t.Cleanup(func() { uaSrv.Close() })
			dgSrv := NewDiago(uaSrv, WithTransport(Transport{
				Transport: "udp", BindHost: "127.0.0.1", BindPort: srvPort,
			}))
			require.NoError(t, dgSrv.ServeBackground(ctx, tc.handler))

			uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
			require.NoError(t, err)
			t.Cleanup(func() { uaCli.Close() })
			dgCli := NewDiago(uaCli, WithTransport(Transport{
				Transport: "udp", BindHost: "127.0.0.1", BindPort: cliPort,
			}))

			_, err = dgCli.Dial(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: srvPort})
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
	dgSrv := NewDiago(uaSrv, WithTransport(Transport{
		Transport: "udp", BindHost: "127.0.0.1", BindPort: 15230,
	}))
	require.NoError(t, dgSrv.ServeBackground(ctx, func(hctx context.Context, d *DialogServerSession) error {
		if err := d.Answer(ctx); err != nil {
			return err
		}
		<-hctx.Done()
		handlerCtxErr <- hctx.Err()
		return nil
	}))

	uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
	require.NoError(t, err)
	t.Cleanup(func() { uaCli.Close() })
	dgCli := NewDiago(uaCli, WithTransport(Transport{
		Transport: "udp", BindHost: "127.0.0.1", BindPort: 15231,
	}))

	dlg, err := dgCli.Dial(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: 15230})
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

	dgSrv := NewDiago(uaSrv, WithTransport(Transport{
		Transport: "udp", BindHost: "127.0.0.1", BindPort: 15240,
	}))
	require.NoError(t, dgSrv.ServeBackground(ctx, func(ctx context.Context, d *DialogServerSession) error {
		if err := d.Answer(ctx); err != nil {
			return err
		}
		// The call was answered: the error must turn into BYE + Reason header.
		return ErrBusy
	}))

	var mu sync.Mutex
	var byeReq *sip.Request
	uaCli, err := sipgo.NewUA(sipgo.WithUserAgent("client"))
	require.NoError(t, err)
	t.Cleanup(func() { uaCli.Close() })
	dgCli := NewDiago(uaCli,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 15241}),
		WithServerRequestMiddleware(func(next sipgo.RequestHandler) sipgo.RequestHandler {
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
	require.NoError(t, dgCli.ServeBackground(ctx, nil))

	dlg, err := dgCli.Dial(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: 15240})
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

func TestHangupAndCloseClient(t *testing.T) {
	var byeCount atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if req.Method == sip.BYE {
			byeCount.Add(1)
		}
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d, err := dg.Dial(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.Equal(t, 1, cacheCountClientDialogs(dg))

	require.NoError(t, d.HangupAndClose(ctx))
	assert.EqualValues(t, 1, byeCount.Load(), "HangupAndClose must send BYE")
	assert.Equal(t, 0, cacheCountClientDialogs(dg), "HangupAndClose must evict the dialog")

	// Close is idempotent.
	require.NoError(t, d.HangupAndClose(ctx))
}
