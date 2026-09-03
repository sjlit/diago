// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/examples"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
	"github.com/sjlit/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDiagoClient(t *testing.T, onRequest func(req *sip.Request) *sip.Response, opts ...DiagoOption) *Diago {
	// Create client transaction request
	cTxReq := &clientTxRequester{
		onRequest: onRequest,
	}

	ua, _ := sipgo.NewUA()
	client, _ := sipgo.NewClient(ua)
	client.TxRequester = cTxReq
	t.Cleanup(func() {
		ua.Close()
	})

	opts = append(opts, WithClient(client))
	return NewDiago(ua, opts...)
}

func TestMain(m *testing.M) {
	examples.SetupLogger()
	m.Run()
}

// skipShort skips tests that take multiple seconds of real (wall-clock) time,
// keeping `go test -short ./...` as a fast development loop. Run the full
// suite without -short in CI to cover them.
func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping slow test in -short mode")
	}
}

func TestDiagoRegister(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		sync.OnceFunc(func() {
			sip.NewResponseFromRequest(req, 100, "Trying", nil)
		})()

		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx := context.TODO()
	rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"}, RegisterOptions{})
	require.NoError(t, err)

	err = rtx.Register(ctx)
	require.NoError(t, err)
}

func TestDiagoRegisterAuthorization(t *testing.T) {
	var authValue atomic.Value
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if req.GetHeader("Authorization") == nil {
			res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="test", nonce="abc123", algorithm=MD5`))
			return res
		}
		authValue.Store(req.GetHeader("Authorization").Value())
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx := context.TODO()

	t.Run("SignalOptionCredentials", func(t *testing.T) {
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"}, RegisterOptions{})
		require.NoError(t, err)

		err = rtx.Register(ctx, WithAuthCredentials("aliceFromSignal", "secret"))
		require.NoError(t, err)
		assert.Contains(t, authValue.Load(), `username="aliceFromSignal"`)
	})

	t.Run("RegisterOptionsFallback", func(t *testing.T) {
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "bob", Host: "localhost"}, RegisterOptions{Username: "bobFromOpts", Password: "pw"})
		require.NoError(t, err)

		err = rtx.Register(ctx)
		require.NoError(t, err)
		assert.Contains(t, authValue.Load(), `username="bobFromOpts"`)
	})
}

// TestDiagoInviteOptionsRunOnce locks the single-execution guarantee: a user
// SignalOption func must run exactly once across Diago.Invite dialog creation
// and Invite.
func TestDiagoInviteOptionsRunOnce(t *testing.T) {
	var runs atomic.Int32
	reqCh := make(chan *sip.Request, 4)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	})

	ctx := context.TODO()
	d, err := dg.Invite(ctx, sip.Uri{User: "alice", Host: "localhost"},
		func(p *SignalParams) error {
			runs.Add(1)
			p.Msg.Headers = append(p.Msg.Headers, sip.NewHeader("X-Test-Run", "once"))
			return nil
		})
	require.NoError(t, err)
	defer d.Close()

	assert.Equal(t, int32(1), runs.Load(), "user SignalOption must execute exactly once per Diago.Invite")
	require.NoError(t, d.Hangup(d.Context()))

	req := <-reqCh
	assert.NotNil(t, req.GetHeader("X-Test-Run"), "option-applied header must be present on the INVITE")
}

// TestDiagoInviteBridgeOptionsRunOnce locks the same guarantee for InviteBridge.
func TestDiagoInviteBridgeOptionsRunOnce(t *testing.T) {
	var runs atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	})

	bridge := NewBridge()
	ctx := context.TODO()
	d, err := dg.InviteBridge(ctx, sip.Uri{User: "alice", Host: "localhost"}, &bridge,
		func(p *SignalParams) error {
			runs.Add(1)
			return nil
		})
	require.NoError(t, err)
	defer d.Close()

	assert.Equal(t, int32(1), runs.Load(), "user SignalOption must execute exactly once per Diago.InviteBridge")
	require.NoError(t, d.Hangup(d.Context()))
}

func TestDiagoInviteCallerID(t *testing.T) {

	t.Run("NoSDPInResponse", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		})

		_, err := dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"})
		if assert.Error(t, err) {
			assert.Equal(t, "no SDP in response", err.Error())
		}
	})

	reqCh := make(chan *sip.Request)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		return sip.NewResponseFromRequest(req, 500, "", nil)
	})

	t.Run("DefaultCallerID", func(t *testing.T) {
		go dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"})
		req := <-reqCh

		assert.Equal(t, dg.ua.Name(), req.From().Address.User)
		assert.Equal(t, dg.ua.Hostname(), req.From().Address.Host)
		assert.NotEmpty(t, req.From().Params.GetOr("tag", ""))
	})

}

func TestDiagoTransportConfs(t *testing.T) {
	type testCase = struct {
		tran                    Transport
		expectedContactHostPort string
		expectedMediaHost       string
	}

	doTest := func(tc testCase) {
		tran := tc.tran
		reqCh := make(chan *sip.Request)
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			reqCh <- req
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}, WithTransport(tran))

		go dg.Invite(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})

		// Now check our req passed on client
		req := <-reqCh

		// parse SDP
		sd := sdp.SessionDescription{}
		require.NoError(t, sdp.Unmarshal(req.Body(), &sd))
		connInfo, err := sd.ConnectionInformation()
		require.NoError(t, err)

		assert.Equal(t, tc.expectedContactHostPort, req.Contact().Address.HostPort())
		assert.Equal(t, tc.expectedMediaHost, connInfo.IP.String())
	}

	t.Run("ExternalHost", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:    "udp",
				BindHost:     "127.0.0.1",
				BindPort:     15060,
				ExternalHost: "1.2.3.4",
			},
			expectedContactHostPort: "1.2.3.4:15060",
			expectedMediaHost:       "1.2.3.4",
		}

		doTest(tc)
	})

	t.Run("ExternalHostFQDN", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:    "udp",
				BindHost:     "127.0.0.1",
				BindPort:     15060,
				ExternalHost: "myhost.pbx.com",
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "127.0.0.1", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})

	t.Run("ExternalHostFQDNExternalMedia", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:       "udp",
				BindHost:        "127.0.0.1",
				BindPort:        15060,
				ExternalHost:    "myhost.pbx.com",
				MediaExternalIP: net.IPv4(1, 2, 3, 4),
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "1.2.3.4", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})
}

func TestDiagoNewDialog(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	})
	ctx := context.TODO()

	t.Run("CloseNoError", func(t *testing.T) {
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		dialog.Close()
	})

	// t.Run("NoAcked", func(t *testing.T) {
	// 	dialog, err := dg.NewDialog( sip.Uri{User: "alice", Host: "localhost"}, NewDialogOpts{})
	// 	require.NoError(t, err)
	// 	defer dialog.Close()

	// 	err = dialog.Invite(ctx, InviteOptions{})
	// 	require.NoError(t, err)

	// 	dialog.Audio
	// })

	t.Run("FullDialog", func(t *testing.T) {
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		defer dialog.Close()

		err = dialog.Invite(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, dialog.ID)

		err = dialog.Ack(ctx)
		require.NoError(t, err)

		// assert.NotEmpty(t, dialog.ID)
	})

	// _, err := dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"})
	// if assert.Error(t, err) {
	// 	assert.Equal(t, "no SDP in response", err.Error())
	// }
}

func TestIntegrationDiagoTransportEmpheralPort(t *testing.T) {
	tran := Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  0,
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(tran))

	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	newTran, _ := dg.getTransport("udp")
	t.Log("port assigned", newTran.BindPort)
	assert.NotEmpty(t, newTran.BindPort)
}

func TestIntegrationDiagoCallWithCustomCodecs(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l16Codec := media.Codec{
		Name:        "L16",
		PayloadType: 98,
		SampleRate:  8000,
		SampleDur:   20 * time.Millisecond,
		NumChannels: 1,
	}

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  15066,
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
			},
		))

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 15066}, WithDialogTransport("tcp"))
	require.NoError(t, err)

	l16Audio := bytes.Repeat([]byte{0, 16, 96, 0}, l16Codec.Samples16()/4)
	reader := bytes.NewBuffer(l16Audio)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, len(l16Audio))
	r.Read(recv)
	assert.Equal(t, l16Audio, recv)
}

func TestIntegrationDiagoSRTPCall(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  15443,
					MediaSRTP: 1, // This enables SRTP
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  15441,
				MediaSRTP: 1, // USE SRTP
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))

	// err = dg.ServeBackground(ctx, func(d *DialogServerSession) {})
	// require.NoError(t, err)

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 15443}, WithDialogTransport("tcp"))
	require.NoError(t, err)

	// pb, err := d.PlaybackCreate()
	// if err != nil {
	// 	panic(err)
	// }

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}

func TestIntegrationDiagoDTLSCall(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	// media.DTLSDebug = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  16443,
					MediaSRTP: 2, // This enables SRTP DTLS
					MediaDTLSConf: media.DTLSConfig{
						Certificates:     []tls.Certificate{testdata.ServerCertificate()},
						ServerClientAuth: media.ServerClientAuthNoCert,
					},
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  16441,
				MediaSRTP: 2, // USE DTLS
				// We do not need any Certificate verification
				MediaDTLSConf: media.DTLSConfig{},
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))

	// err = dg.ServeBackground(ctx, func(d *DialogServerSession) {})
	// require.NoError(t, err)

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 16443}, WithDialogTransport("tcp"))
	require.NoError(t, err)

	// pb, err := d.PlaybackCreate()
	// if err != nil {
	// 	panic(err)
	// }

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	time.Sleep(1 * time.Second)
	t.Log("---------------------------Writing media")
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}
