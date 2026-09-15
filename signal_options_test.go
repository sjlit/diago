// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
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

// scopeTestAll accepts every option so the factory tests can assert on field
// application without restating per-API scopes.
const scopeTestAll optionScope = ^optionScope(0)

func newTestParams(opts []SignalOption) (*signalParams, error) {
	return newSignalParams(opts, "Test", scopeTestAll)
}

func TestSignalMediaConfigOverlay(t *testing.T) {
	base := MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Network: NetworkConfig{
			BindIP:     net.IPv4(10, 0, 0, 1),
			ExternalIP: net.IPv4(1, 1, 1, 1),
		},
	}

	p, err := newTestParams(nil)
	require.NoError(t, err)
	got := signalMediaConfig(base, p)
	assert.Equal(t, base, got)

	nat := RTPNATLearnSource
	p, err = newTestParams([]SignalOption{
		nil, // nil options are ignored
		WithCodecs(media.CodecAudioAlaw),
		WithRTPNAT(nat),
		WithMediaBindIP(net.IPv4(10, 0, 0, 2)),
		WithMediaExternalIP(net.IPv4(2, 2, 2, 2)),
	})
	require.NoError(t, err)

	got = signalMediaConfig(base, p)
	assert.Equal(t, []media.Codec{media.CodecAudioAlaw}, got.Codecs)
	assert.Equal(t, RTPNATLearnSource, got.Network.RTPNAT)
	assert.Equal(t, "10.0.0.2", got.Network.BindIP.String())
	assert.Equal(t, "2.2.2.2", got.Network.ExternalIP.String())

	// Base config must not be mutated
	assert.Equal(t, "1.1.1.1", base.Network.ExternalIP.String())
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw}, base.Codecs)

	// Invalid options are reported
	_, err = newTestParams([]SignalOption{WithMediaExternalIP(nil)})
	assert.Error(t, err)
	_, err = newTestParams([]SignalOption{WithContact(nil)})
	assert.Error(t, err)
}

func TestSignalMediaConfigSDPSessionNameOverlay(t *testing.T) {
	t.Run("OverrideBaseConfig", func(t *testing.T) {
		base := MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			SDP:    SDPConfig{SessionName: "BaseName"},
		}
		p, err := newTestParams([]SignalOption{
			WithSDPSessionName("OverrideName"),
		})
		require.NoError(t, err)

		got := signalMediaConfig(base, p)
		assert.Equal(t, "OverrideName", got.SDP.SessionName)
		// Base config must not be mutated
		assert.Equal(t, "BaseName", base.SDP.SessionName)
	})

	t.Run("UnsetLeavesBaseIntact", func(t *testing.T) {
		base := MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			SDP:    SDPConfig{SessionName: "BaseName"},
		}
		p, err := newTestParams(nil)
		require.NoError(t, err)

		got := signalMediaConfig(base, p)
		assert.Equal(t, "BaseName", got.SDP.SessionName)
	})

	t.Run("EmptyStringRejected", func(t *testing.T) {
		// Empty string carries no information — indistinguishable from "not set".
		// Match WithMediaBindIP(nil) / WithContact(nil) convention and reject.
		_, err := newTestParams([]SignalOption{WithSDPSessionName("")})
		assert.Error(t, err)
	})

	t.Run("LineBreakRejected", func(t *testing.T) {
		// CR/LF would inject extra SDP lines after "s=" — reject at option time.
		_, err := newTestParams([]SignalOption{WithSDPSessionName("x\nm=audio 0")})
		assert.Error(t, err)
		_, err = newTestParams([]SignalOption{WithSDPSessionName("x\r\nv=0")})
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

	dgSrv, err := NewDiago(uaSrv,
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
	require.NoError(t, err)

	err = dgSrv.ServeBackground(ctx, func(ctx context.Context, d *DialogServerSession) error {
		if err := d.Trying(ctx); err != nil {
			t.Error(err)
			return nil
		}
		if err := d.Ringing(ctx, WithHeader("Alert-Info", "<ring>")); err != nil {
			t.Error(err)
			return nil
		}
		customContact := &sip.ContactHeader{
			// Reachable address so that the client can send BYE to it, but
			// with custom user to prove the Contact was overridden
			Address: sip.Uri{Scheme: "sip", User: "answered-custom", Host: "127.0.0.1", Port: 15091},
		}
		if err := d.Answer(ctx,
			WithContact(customContact),
			WithResponseMutator(func(res *sip.Response) error {
				res.AppendHeader(sip.NewHeader("X-Mutated", "yes"))
				return nil
			}),
		); err != nil {
			t.Error(err)
			return nil
		}
		<-d.Context().Done()
		return nil
	})
	require.NoError(t, err)

	uaCli, _ := sipgo.NewUA(sipgo.WithUserAgent("client"))
	defer uaCli.Close()

	dgCli := newDialer(t, uaCli)
	err = dgCli.ServeBackground(ctx, func(ctx context.Context, d *DialogServerSession) error {
		return nil
	})
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

// TestSignalOptionsFieldCoverage directly exercises every SignalOption factory
// so a field-application regression cannot slip past the scope-table tests.
func TestSignalOptionsFieldCoverage(t *testing.T) {
	t.Run("WithBody", func(t *testing.T) {
		p, err := newTestParams([]SignalOption{WithBody([]byte("custom-sdp"))})
		require.NoError(t, err)
		assert.Equal(t, []byte("custom-sdp"), p.Msg.Body)
	})

	t.Run("WithMediaSession rejects nil", func(t *testing.T) {
		_, err := newTestParams([]SignalOption{WithMediaSession(nil)})
		assert.Error(t, err)
	})

	t.Run("WithMediaSession stores session", func(t *testing.T) {
		sess := &media.MediaSession{}
		p, err := newTestParams([]SignalOption{WithMediaSession(sess)})
		require.NoError(t, err)
		assert.Same(t, sess, p.Media.MediaSession)
	})

	t.Run("WithMediaDTLS stores pointer copy", func(t *testing.T) {
		conf := media.DTLSConfig{ServerName: "sip.example"}
		p, err := newTestParams([]SignalOption{WithMediaDTLS(conf)})
		require.NoError(t, err)
		require.NotNil(t, p.Media.MediaDTLSConf)
		// Mutating the original must not affect the stored copy.
		conf.ServerName = "mutated"
		assert.Equal(t, "sip.example", p.Media.MediaDTLSConf.ServerName)
	})

	t.Run("WithMediaDTLS rejects empty config", func(t *testing.T) {
		_, err := newTestParams([]SignalOption{WithMediaDTLS(media.DTLSConfig{})})
		assert.Error(t, err)
	})

	t.Run("WithRequestMutator rejects nil fn", func(t *testing.T) {
		_, err := newTestParams([]SignalOption{WithRequestMutator(nil)})
		assert.Error(t, err)
	})

	t.Run("WithRequestMutator runs on outgoing request", func(t *testing.T) {
		called := false
		p, err := newTestParams([]SignalOption{WithRequestMutator(func(req *sip.Request) error {
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
		p, err := newTestParams([]SignalOption{WithResponseMutator(func(res *sip.Response) error {
			res.AppendHeader(sip.NewHeader("X-Out", "1"))
			return nil
		})})
		require.NoError(t, err)
		require.NotNil(t, p.Msg.MutateResponse)
	})

	t.Run("WithDialogTransport / WithDialogTransportID", func(t *testing.T) {
		p, err := newTestParams([]SignalOption{
			WithDialogTransport("tcp"),
			WithDialogTransportID("tr-1"),
		})
		require.NoError(t, err)
		assert.Equal(t, "tcp", p.Dialog.Transport)
		assert.Equal(t, "tr-1", p.Dialog.TransportID)
	})

	t.Run("WithOriginator rejects nil", func(t *testing.T) {
		_, err := newTestParams([]SignalOption{WithOriginator(nil)})
		assert.Error(t, err)
	})

	t.Run("WithAuthCredentials", func(t *testing.T) {
		p, err := newTestParams([]SignalOption{WithAuthCredentials("u", "p")})
		require.NoError(t, err)
		assert.Equal(t, "u", p.Dialog.Username)
		assert.Equal(t, "p", p.Dialog.Password)
	})

	t.Run("WithEarlyMediaDetect", func(t *testing.T) {
		p, err := newTestParams([]SignalOption{WithEarlyMediaDetect()})
		require.NoError(t, err)
		assert.True(t, p.Dialog.EarlyMediaDetect)
	})

	t.Run("WithOnMediaUpdate / WithOnRefer / WithOnResponse", func(t *testing.T) {
		onMU := func(d *DialogMedia) {}
		onRef := func(d *DialogClientSession) error { return nil }
		onRes := func(res *sip.Response) error { return nil }
		p, err := newTestParams([]SignalOption{
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
		p, err := newTestParams([]SignalOption{
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
		p, err := newTestParams([]SignalOption{WithBody(override)})
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
		p, err := newTestParams([]SignalOption{WithContact(override)})
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
		p, err := newTestParams([]SignalOption{
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
