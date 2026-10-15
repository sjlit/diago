// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/tls"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/dtls/v3"
	"github.com/sjlit/diago/v2/testdata"
	"github.com/stretchr/testify/require"
)

func TestDTLSSetup(t *testing.T) {
	// Ephemeral binds: the DTLS conns need the peer address up front, so bind
	// both listeners first and read their ports back.
	serverListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer serverListener.Close()
	serverAddr := serverListener.LocalAddr().(*net.UDPAddr)

	clientListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer clientListener.Close()
	clientAddr := clientListener.LocalAddr().(*net.UDPAddr)

	slog.SetLogLoggerLevel(slog.LevelDebug)

	serverConn, err := dtlsServer(serverListener, clientAddr, []tls.Certificate{testdata.ServerCertificate()})
	require.NoError(t, err)
	defer serverConn.Close()

	clientConn, err := dtlsClient(clientListener, serverAddr, []tls.Certificate{testdata.ClientCertificate()}, "")
	require.NoError(t, err)
	defer clientConn.Close()

	serverErr := make(chan error)
	go func() {
		serverErr <- serverConn.Handshake()
	}()
	err = clientConn.Handshake()
	require.NoError(t, err)
	require.NoError(t, recvWithin(t, serverErr, 5*time.Second))
}

func TestDTLSFingerprint(t *testing.T) {
	fingerprint, err := dtlsSHA256Fingerprint(testdata.ClientCertificate())
	require.NoError(t, err)
	t.Log(fingerprint)

	fingerprint, err = dtlsSHA256Fingerprint(testdata.ServerCertificate())
	require.NoError(t, err)
	t.Log(fingerprint)
}

func TestDTLSVerifyConnection(t *testing.T) {
	cert := testdata.ClientCertificate()
	der := cert.Certificate[0]

	fpSHA256, err := certificateFingerprint(der, "SHA-256")
	require.NoError(t, err)
	fpSHA1, err := certificateFingerprint(der, "sha-1")
	require.NoError(t, err)

	// Corrupt the first hex pair while keeping the formatting valid
	mismatch := "FF" + fpSHA256[2:]

	t.Run("matching sha256 fingerprint passes", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: fpSHA256}}, nil)
		require.NoError(t, err)
	})

	// Regression: a peer certificate that matches none of the offered SDP
	// fingerprints used to be accepted (fail-open), defeating RFC 5763
	// section 5.10 MITM protection.
	t.Run("mismatched fingerprint fails closed", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: mismatch}}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not match")
	})

	t.Run("other hash algorithms supported", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "sha-1", fingerprint: fpSHA1}}, nil)
		require.NoError(t, err)
	})

	t.Run("fingerprint formatting is ignored", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		normalized := strings.ToLower(strings.ReplaceAll(fpSHA256, ":", ""))
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: normalized}}, nil)
		require.NoError(t, err)
	})

	t.Run("only unsupported algorithms fail closed", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "MD5", fingerprint: "00:11"}}, nil)
		require.Error(t, err)
	})

	t.Run("missing peer certificate fails", func(t *testing.T) {
		state := &dtls.State{}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: fpSHA256}}, nil)
		require.Error(t, err)
	})

	t.Run("no fingerprints fails closed", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, nil, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no sdp fingerprints")
	})
}

func TestCertificateFingerprint(t *testing.T) {
	cert := testdata.ClientCertificate()
	der := cert.Certificate[0]

	// RFC 8122 fingerprints are computed over the raw DER bytes
	fp, err := certificateFingerprint(der, "SHA-256")
	require.NoError(t, err)
	require.Equal(t, 32, len(strings.Split(fp, ":")), "sha-256 fingerprint has 32 hex pairs, got %q", fp)

	fpSHA1, err := certificateFingerprint(der, "sha-1")
	require.NoError(t, err)
	require.NotEqual(t, fp, fpSHA1)
	require.Equal(t, 20, len(strings.Split(fpSHA1, ":")), "sha-1 fingerprint has 20 hex pairs, got %q", fpSHA1)
}
