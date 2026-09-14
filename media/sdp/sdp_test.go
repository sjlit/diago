// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package sdp

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSDP(t *testing.T) {
	// t.Skip("TODO: fix SDP unmarshal")
	body := `v=0
o=- 3905350750 3905350750 IN IP4 192.168.100.11
s=pjmedia
b=AS:84
t=0 0
a=X-nat:0
m=audio 57797 RTP/AVP 96 97 98 99 3 0 8 9 120 121 122
c=IN IP4 192.168.100.11
b=TIAS:64000
a=sendrecv
a=rtpmap:96 speex/16000
a=rtpmap:97 speex/8000
a=rtpmap:98 speex/32000
a=rtpmap:99 iLBC/8000
a=fmtp:99 mode=30
a=rtpmap:120 telephone-event/16000
a=fmtp:120 0-16
a=rtpmap:121 telephone-event/8000
a=fmtp:121 0-16
a=rtpmap:122 telephone-event/32000
a=fmtp:122 0-16
a=ssrc:1204560450 cname:4585300731f880ff
a=rtcp:57798 IN IP4 192.168.100.11
a=rtcp-mux
`

	// Fails due to b=TIAS:64000
	sd := SessionDescription{}
	err := Unmarshal([]byte(body), &sd)
	require.NoError(t, err)

	require.Equal(t, sd.Value("m"), "audio 57797 RTP/AVP 96 97 98 99 3 0 8 9 120 121 122")

	md, err := sd.MediaDescription("audio")
	require.NoError(t, err)
	require.Equal(t, 57797, md.Port)
	require.Equal(t, "RTP/AVP", md.Proto)
	require.Equal(t, []string{"96", "97", "98", "99", "3", "0", "8", "9", "120", "121", "122"}, md.Formats)

	ci, err := sd.ConnectionInformation()
	require.NoError(t, err)
	require.Equal(t, "IN", ci.NetworkType)
	require.Equal(t, "IP4", ci.AddressType)
	require.Equal(t, net.ParseIP("192.168.100.11").String(), ci.IP.String())

}

// A video line must not break audio selection, and the audio section must use
// its own media-level c= (video listed first).
func TestMediaDescriptionMultiMediaLine(t *testing.T) {
	body := `v=0
o=- 3905350750 3905350750 IN IP4 192.168.100.11
s=test
t=0 0
m=video 40000 RTP/AVP 96
c=IN IP4 192.168.100.20
a=rtpmap:96 H264/90000
m=audio 57797 RTP/AVP 0 8 101
c=IN IP4 192.168.100.21
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=sendrecv
`
	sd := SessionDescription{}
	require.NoError(t, Unmarshal([]byte(body), &sd))

	md, err := sd.MediaDescription("audio")
	require.NoError(t, err)
	require.Equal(t, 57797, md.Port)

	ci, err := sd.ConnectionInformationFor("audio")
	require.NoError(t, err)
	require.Equal(t, net.ParseIP("192.168.100.21").String(), ci.IP.String())
}

// An SDP without session-level c= carries the connection per media section.
func TestConnectionInformationMediaLevelOnly(t *testing.T) {
	body := `v=0
o=- 3905350750 3905350750 IN IP4 192.168.100.11
s=test
t=0 0
m=audio 57797 RTP/AVP 0 8
c=IN IP4 192.168.100.11
a=rtpmap:0 PCMU/8000
a=sendrecv
`
	sd := SessionDescription{}
	require.NoError(t, Unmarshal([]byte(body), &sd))

	ci, err := sd.ConnectionInformationFor("audio")
	require.NoError(t, err)
	require.Equal(t, net.ParseIP("192.168.100.11").String(), ci.IP.String())
}

// A video-only media c= must not shadow the session-level c= for audio.
func TestConnectionInformationSessionFallback(t *testing.T) {
	body := `v=0
o=- 3905350750 3905350750 IN IP4 192.168.100.11
s=test
t=0 0
c=IN IP4 192.168.100.11
m=audio 57797 RTP/AVP 0 8
a=rtpmap:0 PCMU/8000
a=sendrecv
m=video 40000 RTP/AVP 96
c=IN IP4 192.168.100.20
a=rtpmap:96 H264/90000
`
	sd := SessionDescription{}
	require.NoError(t, Unmarshal([]byte(body), &sd))

	ci, err := sd.ConnectionInformationFor("audio")
	require.NoError(t, err)
	require.Equal(t, net.ParseIP("192.168.100.11").String(), ci.IP.String())
}

// Regression: MediaDirection scanned every a= line, so a later video section
// could override the audio direction (and vice versa).
func TestMediaDirectionForMultiMedia(t *testing.T) {
	body := `v=0
o=- 3905350750 3905350750 IN IP4 192.168.100.11
s=test
t=0 0
c=IN IP4 192.168.100.11
m=audio 57797 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendrecv
m=video 40000 RTP/AVP 96
a=rtpmap:96 H264/90000
a=recvonly
`
	sd := SessionDescription{}
	require.NoError(t, Unmarshal([]byte(body), &sd))
	require.Equal(t, ModeSendrecv, sd.MediaDirectionFor("audio"))
	require.Equal(t, ModeRecvonly, sd.MediaDirectionFor("video"))
	// The legacy last-wins scan still sees the video attribute: this is the
	// behavior MediaDirectionFor exists to avoid.
	require.Equal(t, ModeRecvonly, sd.MediaDirection())

	// A session-level direction is overridden by the media section.
	body2 := `v=0
o=- 1 1 IN IP4 192.168.100.11
s=test
t=0 0
c=IN IP4 192.168.100.11
a=sendonly
m=audio 57797 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendrecv
`
	sd2 := SessionDescription{}
	require.NoError(t, Unmarshal([]byte(body2), &sd2))
	require.Equal(t, ModeSendrecv, sd2.MediaDirectionFor("audio"))

	// No direction attributes at all defaults to sendrecv.
	body3 := `v=0
o=- 1 1 IN IP4 192.168.100.11
s=test
t=0 0
c=IN IP4 192.168.100.11
m=audio 57797 RTP/AVP 0
a=rtpmap:0 PCMU/8000
`
	sd3 := SessionDescription{}
	require.NoError(t, Unmarshal([]byte(body3), &sd3))
	require.Equal(t, ModeSendrecv, sd3.MediaDirectionFor("audio"))
}

// Regression: the parser accepted arbitrary key lengths, so a line like
// "c_sections=7" forged the internal section-tracking key and broke the
// media-level c= mapping. SDP types are single characters (RFC 4566).
func TestUnmarshalRejectsMultiCharacterType(t *testing.T) {
	body := `v=0
o=- 1 1 IN IP4 10.0.0.1
s=-
c=IN IP4 10.0.0.1
c_sections=7
t=0 0
m=audio 5004 RTP/AVP 0
`
	sd := SessionDescription{}
	err := Unmarshal([]byte(body), &sd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "single character")
	// Only the legitimate session-level c= is tracked; the forged line was
	// rejected before it could append to the internal key.
	require.Equal(t, []string{"0"}, sd.Values("c_sections"))
}
