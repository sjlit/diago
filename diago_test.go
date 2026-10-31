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

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/examples"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/media/sdp"
	"github.com/sjlit/diago/v2/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDiagoClient(t *testing.T, onRequest func(req *sip.Request) *sip.Response, opts ...agentOption) *Agent {
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

	opts = append(opts, withDefaultTransport(), withClient(client), withUserAgent(ua))
	dg, err := newAgent(opts...)
	require.NoError(t, err)
	return dg
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
	rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"})
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

	t.Run("signal option credentials", func(t *testing.T) {
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)

		err = rtx.Register(ctx, WithAuthCredentials("aliceFromSignal", "secret"))
		require.NoError(t, err)
		assert.Contains(t, authValue.Load(), `username="aliceFromSignal"`)
	})

	t.Run("register transaction credentials fallback", func(t *testing.T) {
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "bob", Host: "localhost"}, WithAuthCredentials("bobFromOpts", "pw"))
		require.NoError(t, err)

		err = rtx.Register(ctx)
		require.NoError(t, err)
		assert.Contains(t, authValue.Load(), `username="bobFromOpts"`)
	})
}

// registerTestCapture records a clone of every request seen by the fake
// transaction layer; clientTxRequester invokes onRequest synchronously, so the
// slice is complete once the call under test returns.
func registerTestCapture(reqs *[]*sip.Request) func(req *sip.Request) *sip.Response {
	return func(req *sip.Request) *sip.Response {
		*reqs = append(*reqs, req.Clone())
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	}
}

func registerTestHeaderValue(t *testing.T, req *sip.Request, name string) string {
	t.Helper()
	h := req.GetHeader(name)
	require.NotNil(t, h, "header %q missing", name)
	return h.Value()
}

// TestDiagoRegisterTransactionOptions locks the transaction-level
// SignalOption contract: WithRegister* options shape the Origin template, the
// transaction-level WithRequestMutator runs on every REGISTER attempt
// (initial + qualify) before per-call options, and OnRegistered fires after a
// successful initial REGISTER.
func TestDiagoRegisterTransactionOptions(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx := context.TODO()

	t.Run("transaction options shape every attempt", func(t *testing.T) {
		var reqs []*sip.Request
		dg2 := testDiagoClient(t, registerTestCapture(&reqs))

		rtx, err := dg2.RegisterTransaction(ctx, sip.Uri{User: "carol", Host: "localhost"},
			WithRegisterExpiry(3600*time.Second),
			WithRegisterAllowHeaders("INVITE", "ACK", "BYE"),
			WithRequestMutator(func(req *sip.Request) error {
				req.AppendHeader(sip.NewHeader("X-Transaction", "mutated"))
				return nil
			}),
		)
		require.NoError(t, err)

		// The Origin template already carries the static option shape.
		assert.Equal(t, "3600", registerTestHeaderValue(t, rtx.Origin, "Expires"))
		assert.Contains(t, registerTestHeaderValue(t, rtx.Origin, "Allow"), "INVITE")

		require.NoError(t, rtx.Register(ctx))
		require.NoError(t, rtx.Qualify(ctx))

		require.Len(t, reqs, 2)
		for i, req := range reqs {
			assert.Equal(t, "3600", registerTestHeaderValue(t, req, "Expires"), "attempt %d", i)
			assert.Equal(t, "mutated", registerTestHeaderValue(t, req, "X-Transaction"), "attempt %d", i)
		}
	})

	t.Run("per call mutator runs on top of transaction", func(t *testing.T) {
		var reqs []*sip.Request
		dg2 := testDiagoClient(t, registerTestCapture(&reqs))

		rtx, err := dg2.RegisterTransaction(ctx, sip.Uri{User: "erin", Host: "localhost"},
			WithRequestMutator(func(req *sip.Request) error {
				req.AppendHeader(sip.NewHeader("X-Transaction", "mutated"))
				return nil
			}),
		)
		require.NoError(t, err)

		require.NoError(t, rtx.Register(ctx, WithRequestMutator(func(req *sip.Request) error {
			// Last-chance hook: the transaction mutation is already present.
			assert.Equal(t, "mutated", registerTestHeaderValue(t, req, "X-Transaction"))
			req.AppendHeader(sip.NewHeader("X-PerCall", "last"))
			return nil
		})))

		require.Len(t, reqs, 1)
		assert.Equal(t, "mutated", registerTestHeaderValue(t, reqs[0], "X-Transaction"))
		assert.Equal(t, "last", registerTestHeaderValue(t, reqs[0], "X-PerCall"))
	})

	t.Run("contact override", func(t *testing.T) {
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "heidi", Host: "localhost"},
			WithContact(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "heidi", Host: "example.com", Port: 5060}}))
		require.NoError(t, err)
		cont := rtx.Origin.Contact()
		require.NotNil(t, cont)
		assert.Equal(t, "example.com", cont.Address.Host)
	})

	t.Run("proxy host destination", func(t *testing.T) {
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "grace", Host: "localhost"},
			WithRegisterProxyHost("127.0.0.1:9999"))
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:9999", rtx.Origin.Destination())
	})

	t.Run("on registered fires once", func(t *testing.T) {
		var registered atomic.Int32
		rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "frank", Host: "localhost"},
			WithOnRegistered(func() { registered.Add(1) }))
		require.NoError(t, err)

		require.NoError(t, rtx.Register(ctx))
		assert.Equal(t, 1, int(registered.Load()))
	})
}

// TestRegisterOptionValidation locks the nil/empty validation of the options
// introduced with the SignalOption unification.
func TestRegisterOptionValidation(t *testing.T) {
	p := &signalParams{}
	assert.Error(t, WithOnReferNotify(nil)(p))
	assert.Error(t, WithOnRegistered(nil)(p))
	assert.Error(t, WithRegisterProxyHost("")(p))

	// Happy paths apply without error.
	assert.NoError(t, WithRegisterExpiry(0)(p))
	assert.NoError(t, WithRegisterProxyHost("127.0.0.1:5060")(p))
}

// runOnceOption is a test-only option counting how many times the library
// applies it. It implements the dial option interfaces (unexported markers are
// implementable only inside the package).
type runOnceOption struct{ runs *atomic.Int32 }

func (o runOnceOption) applySignal(p *signalParams) error {
	o.runs.Add(1)
	p.Msg.Headers = append(p.Msg.Headers, sip.NewHeader("X-Test-Run", "once"))
	return nil
}
func (runOnceOption) isDialOption() {}

// TestDiagoInviteOptionsRunOnce locks the single-execution guarantee: an
// option must be applied exactly once across dialog creation and Invite.
func TestDiagoInviteOptionsRunOnce(t *testing.T) {
	var runs atomic.Int32
	reqCh := make(chan *sip.Request, 4)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx := context.TODO()
	d, err := dg.DialURI(ctx, sip.Uri{User: "alice", Host: "localhost"}, runOnceOption{runs: &runs})
	require.NoError(t, err)
	defer d.Close()

	assert.Equal(t, int32(1), runs.Load(), "user option must execute exactly once per Agent.Dial")
	require.NoError(t, d.Hangup(d.Context()))

	req := <-reqCh
	assert.NotNil(t, req.GetHeader("X-Test-Run"), "option-applied header must be present on the INVITE")
}

// TestDiagoDialBridgeOptionsRunOnce locks the same guarantee for DialBridge.
func TestDiagoDialBridgeOptionsRunOnce(t *testing.T) {
	var runs atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	bridge := NewBridge()
	ctx := context.TODO()
	d, err := dg.DialBridgeURI(ctx, sip.Uri{User: "alice", Host: "localhost"}, bridge, runOnceOption{runs: &runs})
	require.NoError(t, err)
	defer d.Close()

	assert.Equal(t, int32(1), runs.Load(), "user option must execute exactly once per Agent.DialBridge")
	require.NoError(t, d.Hangup(d.Context()))
}

// Regression: a typed-nil *Bridge/*BridgeMix stored in the Bridger interface
// must be rejected instead of dereferencing the nil receiver.
func TestDiagoDialBridgeTypedNilBridge(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	var b *Bridge
	_, err := dg.DialBridgeURI(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, b)
	require.ErrorContains(t, err, "bridge is nil")

	var bm *BridgeMix
	_, err = dg.DialBridgeURI(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, bm)
	require.ErrorContains(t, err, "bridge is nil")
}

func TestDiagoInviteCallerID(t *testing.T) {

	t.Run("no sdp in response", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		})

		_, err := dg.DialURI(context.Background(), sip.Uri{User: "alice", Host: "localhost"})
		if assert.Error(t, err) {
			assert.ErrorIs(t, err, ErrSDPMissing)
			assert.Equal(t, "no sdp in response: sdp missing", err.Error())
		}
	})

	reqCh := make(chan *sip.Request)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		return sip.NewResponseFromRequest(req, 500, "", nil)
	})

	t.Run("default caller id", func(t *testing.T) {
		go dg.DialURI(context.Background(), sip.Uri{User: "alice", Host: "localhost"})
		req := <-reqCh

		assert.Equal(t, dg.ua.Name(), req.From().Address.User)
		assert.Equal(t, dg.ua.Hostname(), req.From().Address.Host)
		assert.NotEmpty(t, req.From().Params.GetOr("tag", ""))
	})

}

func TestDiagoTransportConfs(t *testing.T) {
	type testCase = struct {
		tran                    Transport
		profile                 *MediaConfig
		expectedContactHostPort string
		expectedMediaHost       string
	}

	doTest := func(tc testCase) {
		tran := tc.tran
		opts := []agentOption{}
		if tc.profile != nil {
			tran.Profile = "test"
			opts = append(opts, withMediaProfile("test", *tc.profile))
		}
		opts = append(opts, withTransport(tran))
		reqCh := make(chan *sip.Request)
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			reqCh <- req
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}, opts...)

		go dg.DialURI(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})

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

	t.Run("external host", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Network:      "udp",
				BindHost:     "127.0.0.1",
				BindPort:     15060,
				ExternalHost: "1.2.3.4",
			},
			expectedContactHostPort: "1.2.3.4:15060",
			expectedMediaHost:       "1.2.3.4",
		}

		doTest(tc)
	})

	t.Run("external host fqdn", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Network:      "udp",
				BindHost:     "127.0.0.1",
				BindPort:     15060,
				ExternalHost: "myhost.pbx.com",
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "127.0.0.1", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})

	t.Run("external host fqdn external media", func(t *testing.T) {
		profile := MediaConfig{
			Codecs:  []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
			Network: NetworkConfig{ExternalIP: net.IPv4(1, 2, 3, 4)},
		}
		tc := testCase{
			tran: Transport{
				Network:      "udp",
				BindHost:     "127.0.0.1",
				BindPort:     15060,
				ExternalHost: "myhost.pbx.com",
			},
			profile:                 &profile,
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "1.2.3.4", // Hosts are not resolved so it uses the profile
		}

		doTest(tc)
	})
}

func TestDiagoNewDialog(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})
	ctx := context.TODO()

	t.Run("close no error", func(t *testing.T) {
		dialog, err := dg.NewCall(sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		dialog.Close()
	})

	t.Run("full dialog", func(t *testing.T) {
		dialog, err := dg.NewCall(sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		defer dialog.Close()

		err = dialog.Invite(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, dialog.ID())

		err = dialog.Ack(ctx)
		require.NoError(t, err)
	})
}

func TestIntegrationDiagoTransportEmpheralPort(t *testing.T) {
	tran := Transport{
		Network:  "udp",
		BindHost: "127.0.0.1",
		BindPort: 0,
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg, err := newAgent(withUserAgent(ua), withTransport(tran))
	require.NoError(t, err)

	err = dg.ServeBackground(context.TODO(), func(ctx context.Context, d *InboundCall) error {
		return nil
	})
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

	var srvPort int
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		_, srvPort = servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
			d.Trying(ctx)
			if err := d.Answer(ctx); err != nil {
				return err
			}

			err := d.EchoContext(ctx)
			slog.Info("Echo finished with", "error", err)

			return nil
		}, withUserAgent(ua),
			withTransport(
				Transport{
					ID:       "tcp",
					Network:  "tcp",
					BindHost: "127.0.0.1",
					BindPort: 0,
				},
			),
			withMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
				},
			))
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg, err := newAgent(withUserAgent(ua),
		withTransport(
			Transport{
				ID:       "tcp",
				Network:  "tcp",
				BindHost: "127.0.0.1",
			},
		),
		withMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
			},
		))
	require.NoError(t, err)

	d, err := dg.DialURI(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: srvPort}, WithDialogTransport("tcp"))
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
	var srvPort int
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		_, srvPort = servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
			d.Trying(ctx)
			if err := d.Answer(ctx); err != nil {
				return err
			}

			err := d.EchoContext(ctx)
			slog.Info("Echo finished with", "error", err)

			return nil
		}, withUserAgent(ua),
			withMediaProfile("secure", MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				SRTP:   SRTPConfig{Mode: SecureRTPSDES}, // This enables SRTP
			}),
			withTransport(
				Transport{
					ID:       "tcp",
					Network:  "tcp",
					BindHost: "127.0.0.1",
					BindPort: 0,
					Profile:  "secure",
				},
			))
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg, err := newAgent(withUserAgent(ua),
		withMediaProfile("secure", MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			SRTP:   SRTPConfig{Mode: SecureRTPSDES}, // USE SRTP
		}),
		withTransport(
			Transport{
				ID:       "tcp",
				Network:  "tcp",
				BindHost: "127.0.0.1",
				BindPort: 0,
				Profile:  "secure",
			},
		))
	require.NoError(t, err)

	d, err := dg.DialURI(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: srvPort}, WithDialogTransport("tcp"))
	require.NoError(t, err)

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
	// Enable with MediaConfig{SRTP: SRTPConfig{DTLS: media.DTLSConfig{Debug: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var srvPort int
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		_, srvPort = servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
			d.Trying(ctx)
			if err := d.Answer(ctx); err != nil {
				return err
			}

			err := d.EchoContext(ctx)
			slog.Info("Echo finished with", "error", err)

			return nil
		}, withUserAgent(ua),
			withMediaProfile("secure", MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				SRTP: SRTPConfig{ // This enables SRTP DTLS
					Mode: SecureRTPDTLS,
					DTLS: media.DTLSConfig{
						Certificates:     []tls.Certificate{testdata.ServerCertificate()},
						ServerClientAuth: media.ServerClientAuthNoCert,
					},
				},
			}),
			withTransport(
				Transport{
					ID:       "tcp",
					Network:  "tcp",
					BindHost: "127.0.0.1",
					BindPort: 0,
					Profile:  "secure",
				},
			))
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg, err := newAgent(withUserAgent(ua),
		withMediaProfile("secure", MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			SRTP: SRTPConfig{ // USE DTLS
				Mode: SecureRTPDTLS,
				// DTLS-SRTP requires both sides to present certificates (RFC 5763 §5).
				DTLS: media.DTLSConfig{
					Certificates: []tls.Certificate{testdata.ClientCertificate()},
				},
			},
		}),
		withTransport(
			Transport{
				ID:       "tcp",
				Network:  "tcp",
				BindHost: "127.0.0.1",
				BindPort: 0,
				Profile:  "secure",
			},
		))
	require.NoError(t, err)

	d, err := dg.DialURI(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: srvPort}, WithDialogTransport("tcp"))
	require.NoError(t, err)

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	t.Log("---------------------------Writing media")
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}
