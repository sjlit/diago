// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransportValidate(t *testing.T) {
	tlsConf := &tls.Config{}
	cases := []struct {
		name      string
		transport Transport
		wantErr   string
	}{
		{name: "udp", transport: Transport{Network: "udp"}},
		{name: "udp4", transport: Transport{Network: "udp4"}},
		{name: "udp6", transport: Transport{Network: "udp6"}},
		{name: "tcp", transport: Transport{Network: "tcp"}},
		{name: "tcp4", transport: Transport{Network: "tcp4"}},
		{name: "tcp6", transport: Transport{Network: "tcp6"}},
		{name: "ws", transport: Transport{Network: "ws"}},
		{name: "ws4", transport: Transport{Network: "ws4"}},
		{name: "wss", transport: Transport{Network: "wss", TLSConf: tlsConf}},
		{name: "wss4", transport: Transport{Network: "wss4", TLSConf: tlsConf}},
		{name: "wss6", transport: Transport{Network: "wss6", TLSConf: tlsConf}},
		{name: "tls", transport: Transport{Network: "tls", TLSConf: tlsConf}},
		{name: "tls4", transport: Transport{Network: "tls4", TLSConf: tlsConf}},
		{name: "empty", transport: Transport{}, wantErr: "invalid network"},
		{name: "unknown", transport: Transport{Network: "bogus"}, wantErr: "invalid network"},
		{name: "bad suffix", transport: Transport{Network: "tcp64"}, wantErr: "invalid network"},
		{name: "sctp", transport: Transport{Network: "sctp"}, wantErr: "invalid network"},
		{name: "tls without conf", transport: Transport{Network: "tls"}, wantErr: "requires TLSConf"},
		{name: "wss without conf", transport: Transport{Network: "wss"}, wantErr: "requires TLSConf"},
		{name: "tls6 unsupported", transport: Transport{Network: "tls6", TLSConf: tlsConf}, wantErr: "not supported by the sip stack"},
		{name: "udp with tls conf", transport: Transport{Network: "udp", TLSConf: tlsConf}, wantErr: "cannot carry TLSConf"},
		{name: "tcp with tls conf", transport: Transport{Network: "tcp", TLSConf: tlsConf}, wantErr: "cannot carry TLSConf"},
		{name: "bind port range", transport: Transport{Network: "udp", BindPort: 70000}, wantErr: "invalid bind port"},
		{name: "external port range", transport: Transport{Network: "udp", ExternalPort: -1}, wantErr: "invalid external port"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.transport.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNewTransport(t *testing.T) {
	tran, err := NewTransport("udp4")
	require.NoError(t, err)
	assert.Equal(t, Transport{Network: "udp4"}, tran)

	_, err = NewTransport("bogus")
	require.Error(t, err)

	_, err = NewTransport("tls")
	require.Error(t, err, "tls requires TLSConf, so the value cannot be constructed blind")
}

func TestTLSServerNetwork(t *testing.T) {
	cases := map[string]string{
		"tls":  "tls",
		"tls4": "tcp4",
		"wss":  "wss",
		"wss4": "ws4",
		"wss6": "ws6",
		"udp":  "udp",
	}
	for network, want := range cases {
		assert.Equal(t, want, tlsListenNetwork(network), network)
	}
}

// TestServeTLS4Transport locks the serve-time translation of the tls4 network
// (tls4 -> tcp4 for sipgo) with a real TLS listener.
func TestServeTLS4Transport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := newAgent(withUserAgent(ua), withTransport(Transport{
		Network:  "tls4",
		BindHost: "127.0.0.1",
		TLSConf:  &tls.Config{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
	}))
	require.NoError(t, err)
	require.NoError(t, dg.ServeBackground(context.Background(), nil))
	t.Cleanup(func() { require.NoError(t, dg.Close(context.Background())) })

	port := dg.transports[0].BindPort
	require.NotZero(t, port, "tls4 listener must bind an ephemeral port")

	conn, err := tls.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, err, "tls4 must serve TLS over IPv4")
	conn.Close()
}

func TestNewRequiresTransport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	_, err = newAgent(withUserAgent(ua))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no transport configured")

	dg, err := newAgent(withUserAgent(ua), withDefaultTransport())
	require.NoError(t, err)
	require.Len(t, dg.transports, 1)
	assert.Equal(t, "udp", dg.transports[0].Network)
	assert.Equal(t, "127.0.0.1", dg.transports[0].BindHost)
	assert.Equal(t, 5060, dg.transports[0].BindPort)
}

func TestNewRejectsInvalidTransport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	_, err = newAgent(withUserAgent(ua), withTransport(Transport{Network: "bogus"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid network")

	_, err = newAgent(withUserAgent(ua), withTransport(Transport{Network: "tls", BindPort: 0}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires TLSConf")

	// withTransport trims the suffix from Transport, so New must validate
	// the raw network it kept.
	_, err = newAgent(withUserAgent(ua), withTransport(Transport{Network: "tls6", TLSConf: &tls.Config{}}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported by the sip stack")

	dg, err := newAgent(withUserAgent(ua), withTransport(Transport{Network: "tls4", BindHost: "127.0.0.1", TLSConf: &tls.Config{}}))
	require.NoError(t, err)
	require.Len(t, dg.transports, 1)
	assert.Equal(t, "tls4", dg.transports[0].network)
	assert.Equal(t, "tls", dg.transports[0].Network)
}

// TestAgentTransportsResolved locks the Transports accessor: the ephemeral port
// is reported once the listener is ready, the IP-family suffix is trimmed and
// the returned slice is a copy. Polling Transports during startup keeps the
// race detector on the listener ready callback.
func TestAgentTransportsResolved(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := newAgent(withUserAgent(ua), withTransport(Transport{Network: "udp4", BindHost: "127.0.0.1"}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = dg.Transports()
			}
		}
	}()
	err = dg.ServeBackground(ctx, nil)
	close(stop)
	wg.Wait()
	require.NoError(t, err)
	t.Cleanup(func() { dg.Close(context.Background()) })

	got := dg.Transports()
	require.Len(t, got, 1)
	assert.Equal(t, "udp", got[0].Network, "the IP-family suffix is not part of the resolved network")
	assert.NotZero(t, got[0].BindPort, "ephemeral port must be reported once listening")
	assert.Equal(t, got[0].BindPort, got[0].ExternalPort)

	got[0].BindPort = 1
	assert.NotEqual(t, 1, dg.Transports()[0].BindPort, "Transports must return a copy")
}

// TestReaderWriterWithProps locks the props accessors: the props come back
// from the call itself instead of through a pointer option.
func TestReaderWriterWithProps(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	}, withMediaConfig(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
	}))

	d, err := dg.DialURI(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	defer d.Close()

	r, rprops, err := d.ReaderWithProps()
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, media.CodecAudioAlaw, rprops.Codec, "props must carry the negotiated codec")
	assert.NotEmpty(t, rprops.Laddr)
	assert.NotEmpty(t, rprops.Raddr)

	w, wprops, err := d.WriterWithProps()
	require.NoError(t, err)
	require.NotNil(t, w)
	assert.Equal(t, media.CodecAudioAlaw, wprops.Codec)
	assert.Equal(t, rprops.Laddr, wprops.Laddr)
	assert.Equal(t, rprops.Raddr, wprops.Raddr)

	// Same options as AudioReader/AudioWriter can be passed along.
	dr, _, err := d.ReaderWithProps(WithReaderJitterBuffer(media.RTPJitterBufferOptions{}))
	require.NoError(t, err)
	require.NotNil(t, dr)
}

func TestReaderWriterWithPropsUnanswered(t *testing.T) {
	dm := &Media{}
	_, _, err := dm.ReaderWithProps()
	require.ErrorIs(t, err, ErrDialogNotAnswered)

	_, _, err = dm.WriterWithProps()
	require.ErrorIs(t, err, ErrDialogNotAnswered)
}
