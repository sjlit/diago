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

// TestMediaSessionForkContract locks the documented Fork semantics
// (docs/contracts.md §3): drafts share connections, copy configuration, and
// reset everything negotiated. If Fork starts copying or dropping fields
// differently, this test and the doc must be updated together.
func TestMediaSessionForkContract(t *testing.T) {
	parent := &MediaSession{
		Codecs:     []Codec{CodecAudioUlaw, CodecAudioAlaw},
		Mode:       sdp.ModeSendrecv,
		ExternalIP: net.ParseIP("10.0.0.1"),
		SecureRTP:  2,
		SRTPAlg:    7,
		RTPNAT:     1,
		DTLSConf:   DTLSConfig{},
		sdp:        []byte("v=0"),
	}
	parent.Laddr = net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000}
	require.NoError(t, parent.Init())
	defer parent.Close()
	parent.SetRemoteAddr(&net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 5000})

	// Negotiated state on the parent that must NOT leak into the draft
	parent.mode = sdp.ModeRecvonly
	parent.filterCodecs = []Codec{CodecAudioUlaw}

	fork := parent.Fork()

	// Shared: connections
	assert.Same(t, parent.rtpConn, fork.rtpConn, "fork must share the RTP conn")
	assert.Same(t, parent.rtcpConn, fork.rtcpConn, "fork must share the RTCP conn")

	// Copied: configuration
	assert.Equal(t, parent.Laddr, fork.Laddr)
	assert.Equal(t, parent.Mode, fork.Mode)
	assert.Equal(t, parent.RTPNAT, fork.RTPNAT)
	assert.Equal(t, parent.sessionID, fork.sessionID)
	assert.Equal(t, parent.sessionVersion, fork.sessionVersion)
	assert.Equal(t, parent.SecureRTP, fork.SecureRTP)
	assert.Equal(t, parent.SRTPAlg, fork.SRTPAlg)
	assert.Equal(t, parent.DTLSConf, fork.DTLSConf)
	assert.Equal(t, parent.sdp, fork.sdp)

	// Codecs are cloned: mutating the fork must not affect the parent
	assert.Equal(t, parent.Codecs, fork.Codecs)
	fork.Codecs[0] = CodecAudioOpus
	assert.Equal(t, CodecAudioUlaw, parent.Codecs[0], "fork Codecs must be a clone")

	// ExternalIP is cloned
	assert.Equal(t, parent.ExternalIP, fork.ExternalIP)
	fork.ExternalIP = net.ParseIP("10.9.9.9")
	assert.Equal(t, "10.0.0.1", parent.ExternalIP.String(), "fork ExternalIP must be a clone")

	// Reset: negotiated state
	assert.Nil(t, fork.Raddr.IP, "fork must reset Raddr")
	assert.Zero(t, fork.Raddr.Port, "fork must reset Raddr")
	assert.Zero(t, fork.rtcpRaddr.Port, "fork must reset rtcpRaddr")
	assert.Empty(t, fork.mode, "fork must reset negotiated mode")
	assert.Empty(t, fork.filterCodecs, "fork must reset negotiated codecs")
	assert.Nil(t, fork.localCtxSRTP, "fork must reset SRTP contexts")
	assert.Nil(t, fork.remoteCtxSRTP, "fork must reset SRTP contexts")
	assert.Nil(t, fork.dtlsConn, "fork must reset DTLS conn")
	assert.Nil(t, fork.onFinalize, "fork must reset onFinalize")

	// Reset: NAT-learned addresses (documented known behavior)
	assert.Nil(t, fork.learnedRTPFrom.Load(), "fork must reset NAT-learned RTP addr")
	assert.Nil(t, fork.learnedRTCPFrom.Load(), "fork must reset NAT-learned RTCP addr")
}
