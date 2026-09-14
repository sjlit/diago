// SPDX-License-Identifier: MPL-2.0
package media

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"

	"github.com/emiago/dtls/v3"
	"github.com/sjlit/diago/testdata"
	"github.com/stretchr/testify/require"
)

func TestToLibConf_ForcesRequireAnyWhenFingerprintsPresent(t *testing.T) {
	fps := []sdpFingerprints{{alg: "SHA-256", fingerprint: "AA:BB"}}
	conf := DTLSConfig{ServerClientAuth: ServerClientAuthNoCert}
	lib := conf.ToLibConf(fps)
	require.Equal(t, dtls.RequireAnyClientCert, lib.ClientAuth)

	confEmpty := DTLSConfig{ServerClientAuth: ServerClientAuthNoCert}
	libEmpty := confEmpty.ToLibConf(nil)
	require.Equal(t, dtls.NoClientCert, libEmpty.ClientAuth)
}

func TestServerClientAuthRequireCert_MapsToRequireAny(t *testing.T) {
	require.Equal(t, int(dtls.RequireAnyClientCert), ServerClientAuthRequireCert)
}

func newDTLSTestSession(t *testing.T, secure int) *MediaSession {
	t.Helper()
	return newDTLSTestSessionOnPort(t, secure, 23100)
}

func newDTLSTestSessionOnPort(t *testing.T, secure, port int) *MediaSession {
	t.Helper()
	m := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw, CodecAudioAlaw, CodecTelephoneEvent8000},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
		Mode:      "sendrecv",
		SecureRTP: secure,
		DTLSConf:  DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
	}
	require.NoError(t, m.Init())
	t.Cleanup(func() { m.Close() })
	return m
}

func TestMediaSession_RemoteSDP_NoFingerprintRejects(t *testing.T) {
	m := newDTLSTestSession(t, 2)
	sd := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=Sip Go Media
c=IN IP4 127.0.0.1
t=0 0
m=audio 5004 UDP/TLS/RTP/SAVP 0
a=rtpmap:0 PCMU/8000
a=setup:active
a=sendrecv`
	err := m.RemoteSDP([]byte(sd))
	require.Error(t, err)
	require.Contains(t, err.Error(), "fingerprint")
}

// A DTLS session that has not completed the handshake must still arm Finalize:
// the reuse shortcut in RemoteSDP only applies to established associations,
// otherwise media would silently fall back to plain RTP.
func TestMediaSession_RemoteSDP_ArmsFinalizeWhenNotEstablished(t *testing.T) {
	m := newDTLSTestSessionOnPort(t, 2, 23118)
	require.False(t, m.dtlsEstablished)
	fp, err := dtlsSHA256Fingerprint(testdata.ServerCertificate())
	require.NoError(t, err)
	sd := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=Sip Go Media
c=IN IP4 127.0.0.1
t=0 0
m=audio 5004 UDP/TLS/RTP/SAVP 0
a=rtpmap:0 PCMU/8000
a=setup:active
a=fingerprint:SHA-256 ` + fp + `
a=sendrecv`
	require.NoError(t, m.RemoteSDP([]byte(sd)))
	require.NotNil(t, m.onFinalize, "non-established DTLS must still handshake")
	// Do not call Finalize: it would block on a handshake with no peer.
}

func TestMediaSession_RemoteSDP_ProtoMismatch(t *testing.T) {
	// Local DTLS vs remote AVP.
	m := newDTLSTestSessionOnPort(t, 2, 23110)
	sdAVP := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=Sip Go Media
c=IN IP4 127.0.0.1
t=0 0
m=audio 5004 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendrecv`
	err := m.RemoteSDP([]byte(sdAVP))
	require.Error(t, err)
	require.Contains(t, err.Error(), "proto mismatch")

	// Local plain vs remote DTLS.
	m2 := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 23112},
		Mode:   "sendrecv",
	}
	require.NoError(t, m2.Init())
	t.Cleanup(func() { m2.Close() })
	fp, err := dtlsSHA256Fingerprint(testdata.ServerCertificate())
	require.NoError(t, err)
	sdDTLS := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=Sip Go Media
c=IN IP4 127.0.0.1
t=0 0
m=audio 5004 UDP/TLS/RTP/SAVP 0
a=rtpmap:0 PCMU/8000
a=setup:active
a=fingerprint:SHA-256 ` + fp + `
a=sendrecv`
	err = m2.RemoteSDP([]byte(sdDTLS))
	require.Error(t, err)
	require.Contains(t, err.Error(), "proto mismatch")
}

// RFC 5763 §5 forbids the RFC 4145 a=connection attribute for DTLS-SRTP:
// reuse of an established association is determined by the fingerprints, not
// signalled. The SDP must not emit it, before or after the handshake.
func TestMediaSession_LocalSDP_NoConnectionAttribute(t *testing.T) {
	m := newDTLSTestSession(t, 2)
	first := string(m.LocalSDP())
	require.NotContains(t, first, "a=connection:")

	m.dtlsEstablished = true
	second := string(m.LocalSDP())
	require.NotContains(t, second, "a=connection:")
}

func TestMediaSession_OpusGating(t *testing.T) {
	if OpusSupportedForNegotiation() {
		t.Skip("opus supported build; gating disabled")
	}
	// Offer containing opus+G711 negotiates G711 only.
	m := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 23114},
		Mode:   "sendrecv",
	}
	require.NoError(t, m.Init())
	t.Cleanup(func() { m.Close() })
	sd := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=Sip Go Media
c=IN IP4 127.0.0.1
t=0 0
m=audio 5004 RTP/AVP 0 96 101
a=rtpmap:0 PCMU/8000
a=rtpmap:96 opus/48000/2
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=sendrecv`
	require.NoError(t, m.RemoteSDP([]byte(sd)))
	for _, c := range m.filterCodecs {
		require.NotEqual(t, "opus", strings.ToLower(c.Name))
	}
	require.NotContains(t, string(m.LocalSDP()), "opus/48000")

	// Opus-only offer fails fast (callers map to 488).
	m2 := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw, CodecAudioOpus},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 23116},
		Mode:   "sendrecv",
	}
	require.NoError(t, m2.Init())
	t.Cleanup(func() { m2.Close() })
	sdOpusOnly := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=Sip Go Media
c=IN IP4 127.0.0.1
t=0 0
m=audio 5004 RTP/AVP 96
a=rtpmap:96 opus/48000/2
a=sendrecv`
	err := m2.RemoteSDP([]byte(sdOpusOnly))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no supported codecs")
}
