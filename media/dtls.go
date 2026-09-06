// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"hash"
	"net"
	"strings"
	"sync/atomic"

	"github.com/emiago/dtls/v3"
	"github.com/emiago/dtls/v3/pkg/crypto/elliptic"
	"github.com/pion/logging"
)

var (
	// DTLSDebug enables DTLS handshake logging. Atomic so it can be flipped
	// while handshakes run
	DTLSDebug atomic.Bool
)

const (
	ServerClientAuthNoCert      = int(dtls.NoClientCert)
	ServerClientAuthRequireCert = int(dtls.RequestClientCert)

	EllipticCurveP256   uint16 = uint16(elliptic.P256)
	EllipticCurveP384   uint16 = uint16(elliptic.P384)
	EllipticCurveX25519 uint16 = uint16(elliptic.X25519)
)

type DTLSConfig struct {
	Certificates []tls.Certificate
	// If used as client this would verify server certificate
	ServerName string

	// ServerClientAuth determines the server's policy for
	// TLS Client Authentication. The default is ServerClientAuthNoCert.
	// Check ServerClientAuth
	ServerClientAuth int

	// SRTPProfiles to use in exchange. Check constant vars with media.SRTPProfile...
	SRTPProfiles []uint16

	// SDP Setup Role force value.
	// Values: active,passive,actpass
	// Default: offer->active answer->passive
	SDPSetupRole func(offer bool) string `json:"-"`

	// List of Elliptic Curves to use
	//
	// If an ECC ciphersuite is configured and EllipticCurves is empty
	// it will default to X25519, P-256, P-384 in this specific order.
	// Check values with media.Eliptic<name>
	EllipticCurves []uint16
}

func (conf *DTLSConfig) ToLibConf(fingerprints []sdpFingerprints) *dtls.Config {

	config := &dtls.Config{
		// Use appropriate certificate or generate self-signed
		Certificates:     conf.Certificates,
		SignatureSchemes: []tls.SignatureScheme{},

		// CipherSuites: []dtls.CipherSuiteID{
		// 	dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		// },
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AEAD_AES_128_GCM,
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
			// dtls.SRTP_AES128_CM_HMAC_SHA1_32,
		},

		// SignatureSchemes: []tls.SignatureScheme{
		// 	tls.ECDSAWithP256AndSHA256
		// },

		// If you're acting as the server
		// We are verifying Connection fingerprints so we require client cert
		// use dtls.NoClientCert without verfication
		ClientAuth:           dtls.ClientAuthType(conf.ServerClientAuth),
		ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,

		InsecureSkipVerify: conf.ServerName == "", // Accept self-signed certs (for dev)
		ServerName:         conf.ServerName,       // If insecure is false

		// IT IS STILL UNCLEAR WHY WE CAN NOT READ CERTIFICATE HERE
		VerifyConnection: func(state *dtls.State) error {
			if len(fingerprints) == 0 {
				return nil
			}
			return dtlsVerifyConnection(state, fingerprints)
		},
		StopReaderAfterHandshake: true,
	}

	if conf.SRTPProfiles != nil {
		srtpProfs := make([]dtls.SRTPProtectionProfile, len(conf.SRTPProfiles))
		for i, p := range conf.SRTPProfiles {
			srtpProfs[i] = dtls.SRTPProtectionProfile(p)
		}
		config.SRTPProtectionProfiles = srtpProfs
	}

	if conf.EllipticCurves != nil {
		config.EllipticCurves = make([]elliptic.Curve, len(conf.EllipticCurves))
		for i, c := range conf.EllipticCurves {
			config.EllipticCurves[i] = elliptic.Curve(c)
		}
	}

	if DTLSDebug.Load() {
		loggerFactory := logging.NewDefaultLoggerFactory()
		loggerFactory.DefaultLogLevel = logging.LogLevelTrace
		config.LoggerFactory = loggerFactory
	}
	return config
}

func dtlsServer(conn net.PacketConn, raddr net.Addr, certificates []tls.Certificate) (*dtls.Conn, error) {
	conf := DTLSConfig{
		Certificates: certificates,
	}
	return dtls.Server(conn, raddr, conf.ToLibConf([]sdpFingerprints{}))
}

func dtlsClient(conn net.PacketConn, raddr net.Addr, certificates []tls.Certificate, serverName string) (*dtls.Conn, error) {
	// Client DTLS config
	conf := DTLSConfig{
		Certificates: certificates,
		ServerName:   serverName,
	}
	return dtls.Client(conn, raddr, conf.ToLibConf([]sdpFingerprints{}))
}

// dtlsVerifyConnection checks the DTLS peer certificate against the
// fingerprints offered in the remote SDP. RFC 5763 section 5.10 makes this
// verification mandatory - without it a MITM who can tamper with the SDP can
// present its own certificate. It fails closed: when none of the offered
// fingerprints matches, the handshake is aborted.
func dtlsVerifyConnection(state *dtls.State, fingerprints []sdpFingerprints) error {
	if len(fingerprints) == 0 {
		return nil
	}
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("no certificate found in dtls")
	}

	remoteCert := state.PeerCertificates[0]
	for _, fp := range fingerprints {
		remoteFP, err := certificateFingerprint(remoteCert, fp.alg)
		if err != nil {
			DefaultLogger().Debug("Skipping fingerprint with unsupported hash algorithm", "alg", fp.alg)
			continue
		}

		DefaultLogger().Debug("Comparing fingerprint", "alg", fp.alg, "fp", fp.fingerprint, "rfp", remoteFP)
		if fingerprintEqual(fp.fingerprint, remoteFP) {
			return nil
		}
	}

	return fmt.Errorf("dtls peer certificate does not match any of the %d offered SDP fingerprint(s)", len(fingerprints))
}

// certificateFingerprint hashes raw certificate bytes with the RFC 8122 hash
// function named by alg (case-insensitive) and formats the digest as
// upper-case colon separated hex pairs.
func certificateFingerprint(certDER []byte, alg string) (string, error) {
	var h hash.Hash
	switch strings.ToLower(alg) {
	case "sha-1":
		h = sha1.New()
	case "sha-224":
		h = sha256.New224()
	case "sha-256":
		h = sha256.New()
	case "sha-384":
		h = sha512.New384()
	case "sha-512":
		h = sha512.New()
	default:
		return "", fmt.Errorf("unsupported fingerprint hash algorithm %q", alg)
	}
	h.Write(certDER)
	return formatFingerprint(h.Sum(nil)), nil
}

// formatFingerprint renders a digest as upper-case colon separated hex pairs.
func formatFingerprint(sum []byte) string {
	hexStr := strings.ToUpper(hex.EncodeToString(sum))
	var fingerprint strings.Builder
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			fingerprint.WriteString(":")
		}
		fingerprint.WriteString(hexStr[i : i+2])
	}
	return fingerprint.String()
}

// fingerprintEqual compares two colon separated fingerprints ignoring case
// and spacing so "ab:cd" matches "ABCD".
func fingerprintEqual(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, ":", ""), strings.ReplaceAll(b, ":", ""))
}

func dtlsSHA256Fingerprint(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", fmt.Errorf("no certificate data found")
	}
	return certificateFingerprint(cert.Certificate[0], "SHA-256")
}
