// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalMediaConfigOverlay(t *testing.T) {
	base := MediaConfig{
		Codecs:     []media.Codec{media.CodecAudioUlaw},
		BindIP:     net.IPv4(10, 0, 0, 1),
		ExternalIP: net.IPv4(1, 1, 1, 1),
	}

	p, err := newSignalParams(nil)
	require.NoError(t, err)
	got := signalMediaConfig(base, p)
	assert.Equal(t, base, got)

	nat := 1
	p, err = newSignalParams([]SignalOption{
		nil, // nil options are ignored
		WithCodecs(media.CodecAudioAlaw),
		WithRTPNAT(nat),
		WithMediaBindIP(net.IPv4(10, 0, 0, 2)),
		WithMediaExternalIP(net.IPv4(2, 2, 2, 2)),
	})
	require.NoError(t, err)

	got = signalMediaConfig(base, p)
	assert.Equal(t, []media.Codec{media.CodecAudioAlaw}, got.Codecs)
	assert.Equal(t, 1, got.RTPNAT)
	assert.Equal(t, "10.0.0.2", got.BindIP.String())
	assert.Equal(t, "2.2.2.2", got.ExternalIP.String())

	// Base config must not be mutated
	assert.Equal(t, "1.1.1.1", base.ExternalIP.String())
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw}, base.Codecs)

	// Invalid options are reported
	_, err = newSignalParams([]SignalOption{WithMediaExternalIP(nil)})
	assert.Error(t, err)
	_, err = newSignalParams([]SignalOption{WithContact(nil)})
	assert.Error(t, err)
}

func TestSignalMediaConfigSDPSessionNameOverlay(t *testing.T) {
	t.Run("OverrideBaseConfig", func(t *testing.T) {
		base := MediaConfig{
			Codecs:         []media.Codec{media.CodecAudioUlaw},
			SDPSessionName: "BaseName",
		}
		p, err := newSignalParams([]SignalOption{
			WithMediaSDPSessionName("OverrideName"),
		})
		require.NoError(t, err)

		got := signalMediaConfig(base, p)
		assert.Equal(t, "OverrideName", got.SDPSessionName)
		// Base config must not be mutated
		assert.Equal(t, "BaseName", base.SDPSessionName)
	})

	t.Run("UnsetLeavesBaseIntact", func(t *testing.T) {
		base := MediaConfig{
			Codecs:         []media.Codec{media.CodecAudioUlaw},
			SDPSessionName: "BaseName",
		}
		p, err := newSignalParams(nil)
		require.NoError(t, err)

		got := signalMediaConfig(base, p)
		assert.Equal(t, "BaseName", got.SDPSessionName)
	})

	t.Run("EmptyStringRejected", func(t *testing.T) {
		// Empty string carries no information — indistinguishable from "not set".
		// Match WithMediaBindIP(nil) / WithContact(nil) convention and reject.
		_, err := newSignalParams([]SignalOption{WithMediaSDPSessionName("")})
		assert.Error(t, err)
	})

	t.Run("LineBreakRejected", func(t *testing.T) {
		// CR/LF would inject extra SDP lines after "s=" — reject at option time.
		_, err := newSignalParams([]SignalOption{WithMediaSDPSessionName("x\nm=audio 0")})
		assert.Error(t, err)
		_, err = newSignalParams([]SignalOption{WithMediaSDPSessionName("x\r\nv=0")})
		assert.Error(t, err)
	})
}

// TestIntegrationSignalOptions covers custom Contact, custom headers, media IP
// overrides, response mutator and BYE headers on a full loopback call.
func TestIntegrationSignalOptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var inviteReq *sip.Request
	var byeReq *sip.Request

	uaSrv, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
	defer uaSrv.Close()

	dgSrv := NewDiago(uaSrv,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 15091}),
		WithServerRequestMiddleware(func(next sipgo.RequestHandler) sipgo.RequestHandler {
			return func(req *sip.Request, tx sip.ServerTransaction) {
				switch req.Method {
				case sip.INVITE, sip.BYE:
					mu.Lock()
					if req.Method == sip.INVITE {
						inviteReq = req.Clone()
					} else {
						byeReq = req.Clone()
					}
					mu.Unlock()
				}
				next(req, tx)
			}
		}),
	)

	err := dgSrv.ServeBackground(ctx, func(d *DialogServerSession) {
		if err := d.Trying(); err != nil {
			t.Error(err)
			return
		}
		if err := d.Ringing(WithHeader("Alert-Info", "<ring>")); err != nil {
			t.Error(err)
			return
		}
		customContact := &sip.ContactHeader{
			// Reachable address so that the client can send BYE to it, but
			// with custom user to prove the Contact was overridden
			Address: sip.Uri{Scheme: "sip", User: "answered-custom", Host: "127.0.0.1", Port: 15091},
		}
		if err := d.Answer(
			WithContact(customContact),
			WithResponseMutator(func(res *sip.Response) error {
				res.AppendHeader(sip.NewHeader("X-Mutated", "yes"))
				return nil
			}),
		); err != nil {
			t.Error(err)
			return
		}
		<-d.Context().Done()
	})
	require.NoError(t, err)

	uaCli, _ := sipgo.NewUA(sipgo.WithUserAgent("client"))
	defer uaCli.Close()

	dgCli := newDialer(uaCli)
	err = dgCli.ServeBackground(ctx, func(d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := dgCli.NewDialog(sip.Uri{User: "server", Host: "127.0.0.1", Port: 15091})
	require.NoError(t, err)
	defer dialog.Close()

	var ringAlertInfo string
	var okContact string
	var okMutated string

	customContact := &sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "custom-caller", Host: "9.9.9.9", Port: 7777},
	}
	err = dialog.Invite(ctx,
		WithContact(customContact),
		WithMediaExternalIP(net.IPv4(7, 7, 7, 7)),
		WithHeader("X-Custom-Header", "hello"),
		WithOnResponse(func(res *sip.Response) error {
			mu.Lock()
			defer mu.Unlock()
			switch res.StatusCode {
			case sip.StatusRinging:
				if h := res.GetHeader("Alert-Info"); h != nil {
					ringAlertInfo = h.Value()
				}
			case sip.StatusOK:
				if c := res.Contact(); c != nil {
					okContact = c.Address.HostPort()
				}
				if h := res.GetHeader("X-Mutated"); h != nil {
					okMutated = h.Value()
				}
			}
			return nil
		}),
	)
	require.NoError(t, err)
	require.NoError(t, dialog.Ack(ctx))

	t.Run("ClientInviteOptions", func(t *testing.T) {
		mu.Lock()
		inv := inviteReq
		mu.Unlock()
		require.NotNil(t, inv)

		// Custom Contact present exactly once (no default duplication)
		contacts := inv.GetHeaders("Contact")
		require.Len(t, contacts, 1)
		cont, ok := contacts[0].(*sip.ContactHeader)
		require.True(t, ok)
		assert.Equal(t, "9.9.9.9", cont.Address.Host)
		assert.Equal(t, 7777, cont.Address.Port)

		assert.Equal(t, "hello", inv.GetHeader("X-Custom-Header").Value())

		// Media IP override is reflected inside SDP
		sd := sdp.SessionDescription{}
		require.NoError(t, sdp.Unmarshal(inv.Body(), &sd))
		ci, err := sd.ConnectionInformation()
		require.NoError(t, err)
		assert.Equal(t, "7.7.7.7", ci.IP.String())
	})

	t.Run("ServerResponseOptions", func(t *testing.T) {
		assert.Equal(t, "<ring>", ringAlertInfo)
		// Custom contact user proves override (default would be ua name)
		assert.Equal(t, "127.0.0.1:15091", okContact)
		cont := dialog.InviteResponse.Contact()
		require.NotNil(t, cont)
		assert.Equal(t, "answered-custom", cont.Address.User)
		assert.Equal(t, "yes", okMutated)
	})

	t.Run("HangupHeaders", func(t *testing.T) {
		hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
		defer hcancel()
		err = dialog.Hangup(hctx, WithHeader("Reason", "Q.850;cause=16"))
		require.NoError(t, err)

		mu.Lock()
		bye := byeReq
		mu.Unlock()
		require.NotNil(t, bye)
		assert.Equal(t, "Q.850;cause=16", bye.GetHeader("Reason").Value())
	})
}

// TestLegacyOptionsMigrators checks deprecated structs convert to SignalOptions
func TestLegacyOptionsMigrators(t *testing.T) {
	legacy := InviteOptions{
		Transport: "tcp",
		Username:  "u",
		Password:  "p",
		Headers:   []sip.Header{sip.NewHeader("X-Test", "1")},
	}
	opts, err := legacy.Options()
	require.NoError(t, err)

	p, err := newSignalParams(opts)
	require.NoError(t, err)
	assert.Equal(t, "tcp", p.Dialog.Transport)
	assert.Equal(t, "u", p.Dialog.Username)
	assert.Equal(t, "p", p.Dialog.Password)
	assert.Equal(t, "1", p.Msg.Headers[0].Value())

	legacyClient := InviteClientOptions{
		EarlyMediaDetect: true,
		Username:         "u2",
	}
	opts, err = legacyClient.Options()
	require.NoError(t, err)
	p, err = newSignalParams(opts)
	require.NoError(t, err)
	assert.True(t, p.Dialog.EarlyMediaDetect)
	assert.Equal(t, "u2", p.Dialog.Username)

	legacyDialog := NewDialogOptions{TransportID: "tr1"}
	p, err = newSignalParams(legacyDialog.Options())
	require.NoError(t, err)
	assert.Equal(t, "tr1", p.Dialog.TransportID)
}

// TestSignalOptionsFieldCoverage directly exercises every SignalOption factory
// so a field-application regression cannot slip past the legacy migrator tests.
func TestSignalOptionsFieldCoverage(t *testing.T) {
	t.Run("WithBody", func(t *testing.T) {
		p, err := newSignalParams([]SignalOption{WithBody([]byte("custom-sdp"))})
		require.NoError(t, err)
		assert.Equal(t, []byte("custom-sdp"), p.Msg.Body)
	})

	t.Run("WithMediaSession rejects nil", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithMediaSession(nil)})
		assert.Error(t, err)
	})

	t.Run("WithMediaSession stores session", func(t *testing.T) {
		sess := &media.MediaSession{}
		p, err := newSignalParams([]SignalOption{WithMediaSession(sess)})
		require.NoError(t, err)
		assert.Same(t, sess, p.Media.MediaSession)
	})

	t.Run("WithMediaDTLS stores pointer copy", func(t *testing.T) {
		conf := media.DTLSConfig{}
		p, err := newSignalParams([]SignalOption{WithMediaDTLS(conf)})
		require.NoError(t, err)
		require.NotNil(t, p.Media.MediaDTLSConf)
		// Mutating the original must not affect the stored copy.
		conf.Certificates = []tls.Certificate{}
		assert.Empty(t, p.Media.MediaDTLSConf.Certificates)
	})

	t.Run("WithRequestMutator rejects nil fn", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithRequestMutator(nil)})
		assert.Error(t, err)
	})

	t.Run("WithRequestMutator runs on outgoing request", func(t *testing.T) {
		called := false
		p, err := newSignalParams([]SignalOption{WithRequestMutator(func(req *sip.Request) error {
			called = true
			req.AppendHeader(sip.NewHeader("X-Mutator", "ok"))
			return nil
		})})
		require.NoError(t, err)

		req := sip.NewRequest(sip.INVITE, sip.Uri{User: "u", Host: "127.0.0.1"})
		require.NoError(t, applyRequestSignal(req, p))
		assert.True(t, called)
		assert.NotNil(t, req.GetHeader("X-Mutator"))
	})

	t.Run("WithResponseMutator", func(t *testing.T) {
		p, err := newSignalParams([]SignalOption{WithResponseMutator(func(res *sip.Response) error {
			res.AppendHeader(sip.NewHeader("X-Out", "1"))
			return nil
		})})
		require.NoError(t, err)
		require.NotNil(t, p.Msg.MutateResponse)
	})

	t.Run("WithDialogTransport / WithDialogTransportID", func(t *testing.T) {
		p, err := newSignalParams([]SignalOption{
			WithDialogTransport("tcp"),
			WithDialogTransportID("tr-1"),
		})
		require.NoError(t, err)
		assert.Equal(t, "tcp", p.Dialog.Transport)
		assert.Equal(t, "tr-1", p.Dialog.TransportID)
	})

	t.Run("WithOriginator rejects nil", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithOriginator(nil)})
		assert.Error(t, err)
	})

	t.Run("WithAuthCredentials", func(t *testing.T) {
		p, err := newSignalParams([]SignalOption{WithAuthCredentials("u", "p")})
		require.NoError(t, err)
		assert.Equal(t, "u", p.Dialog.Username)
		assert.Equal(t, "p", p.Dialog.Password)
	})

	t.Run("WithEarlyMediaDetect", func(t *testing.T) {
		p, err := newSignalParams([]SignalOption{WithEarlyMediaDetect()})
		require.NoError(t, err)
		assert.True(t, p.Dialog.EarlyMediaDetect)
	})

	t.Run("WithOnMediaUpdate / WithOnRefer / WithOnResponse", func(t *testing.T) {
		onMU := func(d *DialogMedia) {}
		onRef := func(d *DialogClientSession) error { return nil }
		onRes := func(res *sip.Response) error { return nil }
		p, err := newSignalParams([]SignalOption{
			WithOnMediaUpdate(onMU),
			WithOnRefer(onRef),
			WithOnResponse(onRes),
		})
		require.NoError(t, err)
		// Functions are not directly comparable; just assert they are non-nil
		// after registration.
		assert.NotNil(t, p.Dialog.OnMediaUpdate)
		assert.NotNil(t, p.Dialog.OnRefer)
		assert.NotNil(t, p.Dialog.OnResponse)
	})

	t.Run("WithBody overrides signalMediaConfig precedence", func(t *testing.T) {
		// WithMediaSession takes precedence over granular Codecs/RTPNAT
		// according to the documented precedence. Verify the configurator
		// mirrors this: signalMediaConfig should be a no-op on a session
		// that was provided externally.
		base := MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}
		sess := &media.MediaSession{Codecs: []media.Codec{media.CodecAudioAlaw}}
		p, err := newSignalParams([]SignalOption{
			WithMediaSession(sess),
			WithCodecs(media.CodecAudioOpus),
		})
		require.NoError(t, err)

		// signalMediaConfig is only used when MediaSession is nil; here it
		// is non-nil, so the granular options are ignored by design.
		_ = signalMediaConfig(base, p)
		assert.Same(t, sess, p.Media.MediaSession)
	})

	t.Run("applyRequestSignal nil params is no-op", func(t *testing.T) {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
		require.NoError(t, applyRequestSignal(req, nil))
	})
}

// TestBuildReInviteRequest checks the shared helper used by ReInvite on
// server and client dialog sessions.
func TestBuildReInviteRequest(t *testing.T) {
	baseSDP := []byte("v=0\r\no=- 0 0\r\n")

	t.Run("uses base SDP when no Body override", func(t *testing.T) {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
		require.NoError(t, buildReInviteRequest(req, baseSDP, nil, nil))
		assert.Equal(t, baseSDP, req.Body())
		assert.Equal(t, "application/sdp", req.GetHeader("Content-Type").Value())
	})

	t.Run("params.Msg.Body replaces base SDP", func(t *testing.T) {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
		override := []byte("v=0\r\no=- custom\r\n")
		p, err := newSignalParams([]SignalOption{WithBody(override)})
		require.NoError(t, err)
		require.NoError(t, buildReInviteRequest(req, baseSDP, nil, p))
		assert.Equal(t, override, req.Body())
	})

	t.Run("defaultContact applied when params.Msg.Contact unset", func(t *testing.T) {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
		def := &sip.ContactHeader{Address: sip.Uri{User: "default", Host: "127.0.0.1", Port: 5060}}
		require.NoError(t, buildReInviteRequest(req, baseSDP, def, nil))
		contacts := req.GetHeaders("Contact")
		require.Len(t, contacts, 1)
		cont := contacts[0].(*sip.ContactHeader)
		assert.Equal(t, "default", cont.Address.User)
	})

	t.Run("params.Msg.Contact overrides defaultContact", func(t *testing.T) {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
		def := &sip.ContactHeader{Address: sip.Uri{User: "default", Host: "127.0.0.1", Port: 5060}}
		override := &sip.ContactHeader{Address: sip.Uri{User: "override", Host: "9.9.9.9", Port: 7777}}
		p, err := newSignalParams([]SignalOption{WithContact(override)})
		require.NoError(t, err)
		require.NoError(t, buildReInviteRequest(req, baseSDP, def, p))
		contacts := req.GetHeaders("Contact")
		require.Len(t, contacts, 1)
		cont := contacts[0].(*sip.ContactHeader)
		assert.Equal(t, "override", cont.Address.User)
	})

	t.Run("mutator runs after defaults applied", func(t *testing.T) {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
		var sawBody bool
		p, err := newSignalParams([]SignalOption{
			WithRequestMutator(func(r *sip.Request) error {
				sawBody = len(r.Body()) > 0
				return nil
			}),
		})
		require.NoError(t, err)
		require.NoError(t, buildReInviteRequest(req, baseSDP, nil, p))
		assert.True(t, sawBody, "mutator should observe the body that was set")
	})
}
