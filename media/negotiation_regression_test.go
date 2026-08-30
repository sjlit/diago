// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"net"
	"testing"

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

	require.Len(t, m.filterCodecs, 2)
	assert.Equal(t, uint8(111), m.filterCodecs[0].PayloadType)
	assert.Equal(t, "opus", m.filterCodecs[0].Name)
	assert.Equal(t, uint8(100), m.filterCodecs[1].PayloadType)
	assert.Equal(t, "telephone-event", m.filterCodecs[1].Name)

	// DTMF must resolve to the negotiated payload type, not the PT 101 default
	assert.Equal(t, uint8(100), m.DTMFCodec().PayloadType)
	assert.Equal(t, uint32(8000), m.DTMFCodec().SampleRate)

	// Generated answer must carry rtpmap/fmtp for the negotiated payload types
	lsd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(m.LocalSDP(), &lsd))
	assert.Equal(t, "audio 1345 RTP/AVP 111 100", lsd.Value("m"))
	assert.Contains(t, lsd.Values("a"), "rtpmap:111 opus/48000/2")
	assert.Contains(t, lsd.Values("a"), "rtpmap:100 telephone-event/8000")
	assert.Contains(t, lsd.Values("a"), "fmtp:100 0-16")
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
