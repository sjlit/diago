// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"net"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/sjlit/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: codec matching used full struct equality including payload type,
// so a peer offering opus at PT 111 (typical browser value) or telephone-event
// at PT 100 was rejected with "no supported codecs"
func TestMediaSessionUpdateSDPDynamicPayloadTypes(t *testing.T) {
	sd := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/AVP 111 100
a=rtpmap:111 opus/48000/2
a=rtpmap:100 telephone-event/8000
a=fmtp:100 0-16
a=ptime:20
a=maxptime:20
a=sendrecv`

	m := MediaSession{
		Codecs: []Codec{
			CodecAudioAlaw, CodecAudioOpus, CodecTelephoneEvent8000,
		},
		// Do not reuse 1234/1235, they stay bound (RTP+RTCP) by earlier session tests
		Laddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1345},
		Mode:  "sendrecv",
	}
	require.NoError(t, m.Init())
	require.NoError(t, m.RemoteSDP([]byte(sd)))

	if OpusSupportedForNegotiation() {
		require.Len(t, m.filterCodecs, 2)
		assert.Equal(t, uint8(111), m.filterCodecs[0].PayloadType)
		assert.Equal(t, "opus", m.filterCodecs[0].Name)
		assert.Equal(t, uint8(100), m.filterCodecs[1].PayloadType)
		assert.Equal(t, "telephone-event", m.filterCodecs[1].Name)
	} else {
		// #39: opus filtered without with_opus_c; telephone-event remains.
		require.Len(t, m.filterCodecs, 1)
		assert.Equal(t, uint8(100), m.filterCodecs[0].PayloadType)
		assert.Equal(t, "telephone-event", m.filterCodecs[0].Name)
	}

	// DTMF must resolve to the negotiated payload type, not the PT 101 default
	assert.Equal(t, uint8(100), m.DTMFCodec().PayloadType)
	assert.Equal(t, uint32(8000), m.DTMFCodec().SampleRate)

	// Generated answer must carry rtpmap/fmtp for the negotiated payload types
	lsd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(m.LocalSDP(), &lsd))
	if OpusSupportedForNegotiation() {
		assert.Equal(t, "audio 1345 RTP/AVP 111 100", lsd.Value("m"))
		assert.Contains(t, lsd.Values("a"), "rtpmap:111 opus/48000/2")
	} else {
		assert.Equal(t, "audio 1345 RTP/AVP 100", lsd.Value("m"))
	}
	assert.Contains(t, lsd.Values("a"), "rtpmap:100 telephone-event/8000")
	assert.Contains(t, lsd.Values("a"), "fmtp:100 0-16")
}

// Regression: duplicate rtpmap lines (or a second m= section reusing the same
// payload type) made CodecsFromSDPRead write past the caller's slice sized by
// len(md.Formats) - a remote INVITE could panic the process.
func TestCodecsFromSDPReadDuplicateRtpmapNoPanic(t *testing.T) {
	formats := []string{"96", "101"}
	attrs := []string{
		"rtpmap:96 PCMU/8000",
		"rtpmap:96 H264/90000", // duplicate PT from another m= section
		"rtpmap:101 telephone-event/8000",
	}
	codecs := make([]Codec, len(formats))

	n, err := CodecsFromSDPRead(formats, attrs, codecs)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	assert.Equal(t, "PCMU", codecs[0].Name)
	assert.Equal(t, uint8(96), codecs[0].PayloadType)
	assert.Equal(t, "telephone-event", codecs[1].Name)

	// Full RemoteSDP path: a malformed offer must be rejected or negotiated,
	// never panic.
	sd := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
c=IN IP4 127.0.0.1
t=0 0
m=audio 4000 RTP/AVP 96 101
a=rtpmap:96 PCMU/8000
a=rtpmap:96 H264/90000
a=rtpmap:101 telephone-event/8000`
	m := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw},
		Mode:   sdp.ModeSendrecv,
	}
	require.NotPanics(t, func() {
		_ = m.RemoteSDP([]byte(sd))
	})
}

// Regression: after a re-INVITE negotiation adopts a remote-assigned dynamic
// payload type, the media path must resolve it too - ReadRTP/WriteRTP used the
// locally configured Codecs and dropped all inbound packets / errored on write.
func TestMediaSessionDynamicPayloadTypeMediaFlow(t *testing.T) {
	m, err := NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	sd := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
c=IN IP4 127.0.0.1
t=0 0
m=audio 4000 RTP/AVP 96
a=rtpmap:96 PCMU/8000`
	require.NoError(t, m.RemoteSDP([]byte(sd)))

	rtpSess := NewRTPSession(m)

	// Inbound: PT 96 is the negotiated PCMU.
	sender, err := net.DialUDP("udp", nil, &m.Laddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sender.Close() })

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SSRC: 0x1234,
			SequenceNumber: 1, Timestamp: 160,
		},
		Payload: make([]byte, 160),
	}
	data, err := pkt.Marshal()
	require.NoError(t, err)
	_, err = sender.Write(data)
	require.NoError(t, err)

	readPkt := &rtp.Packet{}
	n, err := rtpSess.ReadRTP(make([]byte, RTPBufSize), readPkt)
	require.NoError(t, err)
	require.NotZero(t, n, "negotiated dynamic payload type must not be dropped")
	assert.Equal(t, uint32(0x1234), rtpSess.ReadStats().SSRC)

	// Outbound: same negotiated PT, writer must not error after sending.
	writePkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SSRC: 0x5678,
			SequenceNumber: 2, Timestamp: 320,
		},
		Payload: make([]byte, 160),
	}
	require.NoError(t, rtpSess.WriteRTP(writePkt))
	assert.Equal(t, uint32(0x5678), rtpSess.WriteStats().SSRC)
}

// Regression: Fork used to drop negotiated security state, so re-INVITE (hold/unhold)
// generated plain RTP/AVP offers without a=crypto, downgrading secured calls
func TestMediaSessionForkPreservesNegotiatedState(t *testing.T) {
	m := &MediaSession{
		Codecs:        []Codec{CodecAudioUlaw},
		filterCodecs:  []Codec{CodecAudioUlaw},
		Mode:          sdp.ModeSendrecv,
		SecureRTP:     1,
		SRTPAlg:       7,
		remoteProto:   "RTP/SAVP",
		srtpRemoteTag: 5,
	}

	fork := m.Fork()
	assert.Equal(t, m.SecureRTP, fork.SecureRTP)
	assert.Equal(t, m.SRTPAlg, fork.SRTPAlg)
	assert.Equal(t, m.remoteProto, fork.remoteProto)
	assert.Equal(t, m.srtpRemoteTag, fork.srtpRemoteTag)
}

// Regression: Fork reset the DTLS SRTP contexts and no install path called
// Finalize, so a re-INVITE/Hold on a secured call sent plain RTP while the SDP
// still advertised UDP/TLS/RTP/SAVP. The established association and its SRTP
// contexts must be carried over and reused without a new handshake.
func TestMediaSessionForkReusesEstablishedDTLSSRTP(t *testing.T) {
	ctx, err := srtp.CreateContext(
		make([]byte, 16),
		make([]byte, 14),
		srtp.ProtectionProfileAes128CmHmacSha1_80,
	)
	require.NoError(t, err)

	m := &MediaSession{
		Codecs:          []Codec{CodecAudioUlaw},
		Mode:            sdp.ModeSendrecv,
		SecureRTP:       2,
		SRTPAlg:         SRTPProfileAes128CmHmacSha1_80,
		dtlsEstablished: true,
		localCtxSRTP:    ctx,
		remoteCtxSRTP:   ctx,
		Laddr:           net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
	}

	fork := m.Fork()
	require.True(t, fork.dtlsEstablished)
	require.Same(t, ctx, fork.localCtxSRTP)
	require.Same(t, ctx, fork.remoteCtxSRTP)

	// Re-INVITE offer/answer: the association is reused, not renegotiated.
	sd := `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
c=IN IP4 127.0.0.1
t=0 0
m=audio 4000 UDP/TLS/RTP/SAVP 0
a=rtpmap:0 PCMU/8000
a=setup:active
a=fingerprint:SHA-256 AA:BB:CC:DD`
	require.NoError(t, fork.RemoteSDP([]byte(sd)))
	require.Nil(t, fork.onFinalize, "reused DTLS association must not re-arm the handshake")
	require.NoError(t, fork.Finalize())
	require.Same(t, ctx, fork.localCtxSRTP)
	require.Same(t, ctx, fork.remoteCtxSRTP)

	// RFC 5763 §5 forbids the RFC 4145 connection attribute.
	assert.NotContains(t, string(fork.LocalSDP()), "a=connection:")
}
