// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/siptest"
	"github.com/icholy/digest"
)

func newAuthInvite(t *testing.T) *sip.Request {
	t.Helper()
	recipient := sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: 5060, User: "test"}
	req := sip.NewRequest(sip.INVITE, recipient)
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	cli, err := sipgo.NewClient(ua, sipgo.WithClientAddr("127.0.0.1:11111"))
	if err != nil {
		t.Fatal(err)
	}
	req.AppendHeader(sip.NewHeader("Contact", "<sip:127.0.0.1:11111>"))
	if err := sipgo.ClientRequestBuild(cli, req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestDigestAuthChallengeAndAuthorize(t *testing.T) {
	srv := NewDigestServer()
	defer srv.Close()

	req := newAuthInvite(t)
	auth := DigestAuth{Username: "alice", Password: "wonderland"}

	// First request without credentials must be challenged
	res, err := srv.AuthorizeRequest(req, auth)
	if err != nil {
		t.Fatalf("challenge returned error: %v", err)
	}
	if res.StatusCode != sip.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
	wwwAuth := res.GetHeader("WWW-Authenticate")
	if wwwAuth == nil {
		t.Fatal("401 response has no WWW-Authenticate header")
	}
	chal, err := digest.ParseChallenge(wwwAuth.Value())
	if err != nil {
		t.Fatalf("failed to parse challenge: %v", err)
	}
	if chal.Realm != "sipgo" {
		t.Fatalf("expected default realm sipgo, got %q", chal.Realm)
	}
	if chal.Nonce == "" {
		t.Fatal("challenge has empty nonce")
	}

	// Retry with correct credentials
	cred, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      req.Recipient.Addr(),
		Username: auth.Username,
		Password: auth.Password,
	})
	if err != nil {
		t.Fatal(err)
	}
	req.RemoveHeader("Authorization")
	req.AppendHeader(sip.NewHeader("Authorization", cred.String()))

	res, err = srv.AuthorizeRequest(req, auth)
	if err != nil {
		t.Fatalf("authorize failed: %v", err)
	}
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	// Nonce is single-use: replay must be re-challenged
	res, err = srv.AuthorizeRequest(req, auth)
	if err == nil {
		t.Fatal("replayed nonce must return error")
	}
	if res.StatusCode != sip.StatusUnauthorized {
		t.Fatalf("expected 401 on replay, got %d", res.StatusCode)
	}
	if res.GetHeader("WWW-Authenticate") == nil {
		t.Fatal("replay 401 must carry a fresh WWW-Authenticate challenge")
	}
}

func TestDigestAuthBadCredentials(t *testing.T) {
	srv := NewDigestServer()
	defer srv.Close()

	req := newAuthInvite(t)
	auth := DigestAuth{Username: "alice", Password: "wonderland"}

	res, _ := srv.AuthorizeRequest(req, auth)
	chal, err := digest.ParseChallenge(res.GetHeader("WWW-Authenticate").Value())
	if err != nil {
		t.Fatal(err)
	}

	cred, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      req.Recipient.Addr(),
		Username: auth.Username,
		Password: "wrong-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	req.AppendHeader(sip.NewHeader("Authorization", cred.String()))

	res, err = srv.AuthorizeRequest(req, auth)
	if err == nil {
		t.Fatal("expected error for bad credentials")
	}
	if res.StatusCode != sip.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
}

func TestDigestAuthExpiredNonceReChallenges(t *testing.T) {
	srv := NewDigestServer()
	defer srv.Close()

	req := newAuthInvite(t)
	auth := DigestAuth{Username: "alice", Password: "wonderland", Expire: 10 * time.Millisecond}

	res, _ := srv.AuthorizeRequest(req, auth)
	chal, err := digest.ParseChallenge(res.GetHeader("WWW-Authenticate").Value())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the nonce expire

	cred, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      req.Recipient.Addr(),
		Username: auth.Username,
		Password: auth.Password,
	})
	if err != nil {
		t.Fatal(err)
	}
	req.AppendHeader(sip.NewHeader("Authorization", cred.String()))

	res, err = srv.AuthorizeRequest(req, auth)
	if err == nil {
		t.Fatal("expired nonce must return error")
	}
	if res.StatusCode != sip.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
	wwwAuth := res.GetHeader("WWW-Authenticate")
	if wwwAuth == nil {
		t.Fatal("expired nonce 401 must carry a fresh WWW-Authenticate challenge")
	}
	newChal, err := digest.ParseChallenge(wwwAuth.Value())
	if err != nil {
		t.Fatal(err)
	}
	if newChal.Nonce == chal.Nonce {
		t.Fatal("expected a fresh nonce in re-challenge")
	}

	// Client can complete the handshake with the fresh nonce
	cred, err = digest.Digest(newChal, digest.Options{
		Method:   req.Method.String(),
		URI:      req.Recipient.Addr(),
		Username: auth.Username,
		Password: auth.Password,
	})
	if err != nil {
		t.Fatal(err)
	}
	req.RemoveHeader("Authorization")
	req.AppendHeader(sip.NewHeader("Authorization", cred.String()))
	if res, err = srv.AuthorizeRequest(req, auth); err != nil || res.StatusCode != sip.StatusOK {
		t.Fatalf("expected 200 after re-challenge, got %d err=%v", res.StatusCode, err)
	}
}

func TestDigestAuthorizeDialogSends401(t *testing.T) {
	inviteReq := newAuthInvite(t)
	dialogUA := sipgo.DialogUA{
		Client:     &sipgo.Client{},
		ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "tester", Host: "127.0.0.1", Port: 5060}},
	}
	tx := siptest.NewServerTxRecorder(inviteReq)
	sess, err := dialogUA.ReadInvite(inviteReq, tx)
	if err != nil {
		t.Fatal(err)
	}
	d := &DialogServerSession{DialogServerSession: sess}

	srv := NewDigestServer()
	defer srv.Close()

	// 401 completes the INVITE transaction only after ACK/TimerH, and
	// WriteResponse blocks until then - run it in the background
	authDone := make(chan error, 1)
	go func() { authDone <- d.Authorize(srv, DigestAuth{Username: "alice", Password: "wonderland"}) }()

	res := tx.Result()
	deadline := time.After(5 * time.Second)
	for len(res) == 0 {
		select {
		case <-deadline:
			t.Fatal("no 401 written to transaction")
		case <-time.After(10 * time.Millisecond):
			res = tx.Result()
		}
	}
	defer tx.Terminate()
	if res[0].StatusCode != sip.StatusUnauthorized {
		t.Fatalf("expected 401 written to transaction, got %+v", res)
	}
	if h := res[0].GetHeader("WWW-Authenticate"); h == nil || !strings.Contains(h.Value(), "nonce=") {
		t.Fatalf("expected WWW-Authenticate with nonce, got %v", h)
	}
	// Terminate so the blocked WriteResponse (waiting on ACK/TimerH) returns
	tx.Terminate()
	if err := <-authDone; err == nil {
		t.Fatal("first Authorize must fail with 401 challenge")
	}
}
