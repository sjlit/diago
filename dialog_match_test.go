// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDialogRequest(callID, fromTag, toTag string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "example.com"})
	cid := sip.CallIDHeader(callID)
	req.AppendHeader(&cid)
	fromParams := sip.NewParams()
	fromParams.Add("tag", fromTag)
	req.AppendHeader(&sip.FromHeader{
		Address: sip.Uri{User: "alice", Host: "example.com"},
		Params:  fromParams,
	})
	toParams := sip.NewParams()
	toParams.Add("tag", toTag)
	req.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{User: "bob", Host: "example.com"},
		Params:  toParams,
	})
	return req
}

func TestDiagoMatchDialog_NilAndOutOfDialog(t *testing.T) {
	ua, _ := sipgo.NewUA()
	dg, err := newAgent(withUserAgent(ua), withDefaultTransport())
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	// nil guards
	_, ok := dg.FindCall(nil)
	assert.False(t, ok, "nil request must not match")

	var nilDG *Agent
	_, ok = nilDG.FindCall(newDialogRequest("c1", "f1", "t1"))
	assert.False(t, ok, "nil engine must not match")

	// out-of-dialog: INVITE without To tag
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "example.com"})
	cid := sip.CallIDHeader("call-out")
	req.AppendHeader(&cid)
	fromP := sip.NewParams()
	fromP.Add("tag", "from1")
	req.AppendHeader(&sip.FromHeader{
		Address: sip.Uri{User: "alice", Host: "example.com"},
		Params:  fromP,
	})
	// To without tag
	req.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{User: "bob", Host: "example.com"},
		Params:  sip.NewParams(),
	})
	_, ok = dg.FindCall(req)
	assert.False(t, ok, "request without To tag is out-of-dialog")

	// unknown dialog (tags present but not in cache)
	req2 := newDialogRequest("unknown-call", "f1", "t1")
	_, ok = dg.FindCall(req2)
	assert.False(t, ok)
}

func TestDiagoMatchDialog_ServerAndClient(t *testing.T) {
	ua, _ := sipgo.NewUA()
	dg, err := newAgent(withUserAgent(ua), withDefaultTransport())
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	ctx := context.Background()

	// Seed server dialog under its UAS id
	reqSrv := newDialogRequest("call-srv", "from-srv", "to-srv")
	idSrv, err := sip.DialogIDFromRequestUAS(reqSrv)
	require.NoError(t, err)
	sd := &InboundCall{}
	require.NoError(t, dg.cache.server.store(ctx, idSrv, sd))

	// Seed client dialog under its UAC id
	reqCli := newDialogRequest("call-cli", "from-cli", "to-cli")
	idCli, err := sip.DialogIDFromRequestUAC(reqCli)
	require.NoError(t, err)
	cd := &OutboundCall{}
	require.NoError(t, dg.cache.client.store(ctx, idCli, cd))

	// UAS request must match server
	got, ok := dg.FindCall(reqSrv)
	require.True(t, ok)
	assert.Equal(t, Call(sd), got, "UAS request should resolve to server dialog")

	// UAC request (same headers but lookup via second try) must match client.
	// Build a request where UAS lookup misses but UAC hits: reuse reqCli which is not in server cache.
	got, ok = dg.FindCall(reqCli)
	require.True(t, ok)
	// reqCli's UAS id is Make(call-cli, to-cli, from-cli) which is not stored; UAC id is Make(call-cli, from-cli, to-cli) which is stored.
	assert.Equal(t, Call(cd), got, "UAC request should resolve to client dialog after server miss")
}

func TestDiagoMatchDialog_ServerPriority(t *testing.T) {
	ua, _ := sipgo.NewUA()
	dg, err := newAgent(withUserAgent(ua), withDefaultTransport())
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	ctx := context.Background()

	// Same request matches both caches: server should win (mirrors internal FindCall order).
	req := newDialogRequest("call-both", "from-both", "to-both")
	idUAS, err := sip.DialogIDFromRequestUAS(req)
	require.NoError(t, err)
	idUAC, err := sip.DialogIDFromRequestUAC(req)
	require.NoError(t, err)
	require.NotEqual(t, idUAS, idUAC, "UAS and UAC ids must differ when tags differ")

	sd := &InboundCall{}
	cd := &OutboundCall{}
	require.NoError(t, dg.cache.server.store(ctx, idUAS, sd))
	require.NoError(t, dg.cache.client.store(ctx, idUAC, cd))

	got, ok := dg.FindCall(req)
	require.True(t, ok)
	assert.Equal(t, Call(sd), got, "server must win over client for same request")
}

func TestDiagoMatchDialog_RealClientDialog(t *testing.T) {
	// End-to-end: real confirmed client dialog tracked by engine must be found via an in-dialog request.
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx := context.Background()
	d, err := dg.DialURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	// d is confirmed and stored in client cache under d.ID (DialogIDFromResponse).
	// An in-dialog request from the remote (From=remote tag, To=our tag) with same Call-ID
	// must match via UAC path. Build it from the dialog's InviteResponse.
	res := d.sess.InviteResponse
	require.NotNil(t, res, "InviteResponse must be present for confirmed dialog")
	remoteTo := res.To()
	localFrom := res.From()
	require.NotNil(t, remoteTo)
	require.NotNil(t, localFrom)
	remoteTag, ok := remoteTo.Params.Get("tag")
	require.True(t, ok, "remote To tag")
	localTag, ok := localFrom.Params.Get("tag")
	require.True(t, ok, "local From tag")
	callID := res.CallID()
	require.NotNil(t, callID)

	req := sip.NewRequest(sip.INFO, sip.Uri{User: "alice", Host: "localhost"})
	cid := sip.CallIDHeader(string(*callID))
	req.AppendHeader(&cid)
	// Remote is sender for the in-dialog request we receive
	fromParams2 := sip.NewParams()
	fromParams2.Add("tag", remoteTag)
	req.AppendHeader(&sip.FromHeader{
		Address: remoteTo.Address,
		Params:  fromParams2,
	})
	toParams2 := sip.NewParams()
	toParams2.Add("tag", localTag)
	req.AppendHeader(&sip.ToHeader{
		Address: localFrom.Address,
		Params:  toParams2,
	})

	got, ok := dg.FindCall(req)
	require.True(t, ok, "in-dialog request for confirmed client dialog must be found")
	assert.Equal(t, Call(d), got)
}
