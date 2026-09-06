// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/tls"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/emiago/dtls/v3"
	"github.com/sjlit/diago/testdata"
	"github.com/stretchr/testify/require"
)

func TestDTLSSetup(t *testing.T) {
	clientAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15333}
	serverAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15444}
	slog.SetLogLoggerLevel(slog.LevelDebug)

	listener, err := net.ListenUDP("udp", serverAddr)
	require.NoError(t, err)
	defer listener.Close()

	serverConn, err := dtlsServer(listener, clientAddr, []tls.Certificate{testdata.ServerCertificate()})
	require.NoError(t, err)
	defer serverConn.Close()

	listenerClient, err := net.ListenUDP("udp", clientAddr)
	if err != nil {
		panic(err)
	}
	defer listenerClient.Close()

	clientConn, err := dtlsClient(listenerClient, serverAddr, []tls.Certificate{testdata.ClientCertificate()}, "")
	require.NoError(t, err)
	defer clientConn.Close()

	serverErr := make(chan error)
	go func() {
		serverErr <- serverConn.Handshake()
	}()
	err = clientConn.Handshake()
	require.NoError(t, err)
	require.NoError(t, <-serverErr)
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

	t.Run("matchingSHA256FingerprintPasses", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: fpSHA256}})
		require.NoError(t, err)
	})

	// Regression: a peer certificate that matches none of the offered SDP
	// fingerprints used to be accepted (fail-open), defeating RFC 5763
	// section 5.10 MITM protection.
	t.Run("mismatchedFingerprintFailsClosed", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: mismatch}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not match")
	})

	t.Run("otherHashAlgorithmsSupported", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "sha-1", fingerprint: fpSHA1}})
		require.NoError(t, err)
	})

	t.Run("fingerprintFormattingIsIgnored", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		normalized := strings.ToLower(strings.ReplaceAll(fpSHA256, ":", ""))
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: normalized}})
		require.NoError(t, err)
	})

	t.Run("onlyUnsupportedAlgorithmsFailClosed", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "MD5", fingerprint: "00:11"}})
		require.Error(t, err)
	})

	t.Run("missingPeerCertificateFails", func(t *testing.T) {
		state := &dtls.State{}
		err := dtlsVerifyConnection(state, []sdpFingerprints{{alg: "SHA-256", fingerprint: fpSHA256}})
		require.Error(t, err)
	})

	t.Run("noFingerprintsSkipsValidation", func(t *testing.T) {
		state := &dtls.State{PeerCertificates: [][]byte{der}}
		err := dtlsVerifyConnection(state, nil)
		require.NoError(t, err)
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
