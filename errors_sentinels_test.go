// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorsIsMediaSessionMissing(t *testing.T) {
	dm := &Media{}

	err := dm.checkEarlyMedia([]byte("v=0"))
	assert.ErrorIs(t, err, ErrMediaSessionMissing)

	err = dm.sdpReInviteUnsafe([]byte("v=0"))
	assert.ErrorIs(t, err, ErrMediaSessionMissing)
}

type stubReInviteTransport struct {
	res *sip.Response
}

func (s *stubReInviteTransport) Do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return s.res, nil
}

func (s *stubReInviteTransport) RemoteContact() *sip.ContactHeader {
	return nil
}

func (s *stubReInviteTransport) writeReInviteACK(ctx context.Context, target sip.Uri) error {
	return nil
}

func TestReinviteNoContactError(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "alice", Host: "localhost"})
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	// A 2xx without Contact is malformed; the dialog has no remote target
	// either, so the exchange must fail with the sentinel.
	assert.Nil(t, res.Contact())

	tr := &stubReInviteTransport{res: res}
	_, err := reInviteExchange(context.Background(), tr, req)
	assert.ErrorIs(t, err, ErrRemoteContactMissing)
}

func TestConfigSentinels(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	_, err = newAgent(withUserAgent(ua))
	assert.ErrorIs(t, err, ErrNoTransport)

	_, err = newAgent(withUserAgent(nil), withDefaultTransport())
	assert.ErrorIs(t, err, ErrInvalidOption)

	_, err = newAgent(withUserAgent(ua), withDefaultTransport(), withMediaConfig(MediaConfig{}))
	assert.ErrorIs(t, err, ErrInvalidOption)
}

func TestAgentSentinels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	_, err := dg.Dial(ctx, "sip:bob@localhost;transport=sctp")
	assert.ErrorIs(t, err, ErrTransportNotFound)

	_, err = dg.Register(ctx, "sip:alice@localhost;transport=sctp")
	assert.ErrorIs(t, err, ErrTransportNotFound)

	_, err = dg.Dial(ctx, "sip:bob@localhost", WithHeaders(nil))
	assert.ErrorIs(t, err, ErrInvalidOption)

	_, err = dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"}, WithRegistrationDetached())
	assert.ErrorIs(t, err, ErrInvalidOption)

	_, err = dg.DialBridge(ctx, "sip:bob@localhost", nil)
	assert.ErrorIs(t, err, ErrBridgeNil)

	var nilBridge *Bridge
	_, err = dg.DialBridge(ctx, "sip:bob@localhost", nilBridge)
	assert.ErrorIs(t, err, ErrBridgeNil)

	err = dg.Handle(sip.INVITE, func(req *Request, tx ServerTransaction) error { return nil })
	assert.ErrorIs(t, err, ErrManagedMethod)
}

func TestServeSentinels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := newAgent(withUserAgent(ua), withTransport(Transport{Network: "udp", BindHost: "127.0.0.1"}))
	require.NoError(t, err)
	require.NoError(t, dg.ServeBackground(ctx, nil))

	err = dg.serve(ctx, nil, func() {})
	assert.ErrorIs(t, err, ErrAgentServing)

	err = dg.Handle(sip.SUBSCRIBE, func(req *Request, tx ServerTransaction) error { return nil })
	assert.ErrorIs(t, err, ErrHandleAfterServe)

	require.NoError(t, dg.Close(ctx))

	err = dg.Serve(ctx, nil)
	assert.ErrorIs(t, err, ErrAgentClosed)

	err = dg.Handle(sip.SUBSCRIBE, func(req *Request, tx ServerTransaction) error { return nil })
	assert.ErrorIs(t, err, ErrAgentClosed)
}
