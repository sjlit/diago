// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
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
