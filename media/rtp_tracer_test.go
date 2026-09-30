// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type rtpTraceEvent struct {
	laddr  string
	raddr  string
	packet *rtp.Packet
}

type testRTPTracer struct {
	reads  []rtpTraceEvent
	writes []rtpTraceEvent
}

func (t *testRTPTracer) RTPTraceRead(laddr string, raddr string, packet *rtp.Packet) {
	t.reads = append(t.reads, rtpTraceEvent{laddr: laddr, raddr: raddr, packet: packet.Clone()})
}

func (t *testRTPTracer) RTPTraceWrite(laddr string, raddr string, packet *rtp.Packet) {
	t.writes = append(t.writes, rtpTraceEvent{laddr: laddr, raddr: raddr, packet: packet.Clone()})
}

// TestRTPDebugTracer locks the per-session tracing contract: tracing is
// enabled with MediaSession.RTPDebug and delivered to MediaSession.Tracer
// without touching process-wide state.
func TestRTPDebugTracer(t *testing.T) {
	tracer := &testRTPTracer{}

	sess := fakeMediaSessionWriter(0, 1234, nil)
	// The writer is the same session under test: enable tracing on it.
	sess.RTPDebug = false
	sess.Tracer = tracer

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    8,
			SequenceNumber: 42,
			Timestamp:      160,
			SSRC:           1234,
		},
		Payload: []byte("payload"),
	}

	require.NoError(t, sess.WriteRTP(pkt))
	require.Empty(t, tracer.writes, "tracing must be off by default")

	sess.RTPDebug = true
	require.NoError(t, sess.WriteRTP(pkt))
	require.Len(t, tracer.writes, 1)
	require.Equal(t, sess.Laddr.String(), tracer.writes[0].laddr)
	require.Equal(t, sess.Raddr.String(), tracer.writes[0].raddr)
	require.Equal(t, pkt, tracer.writes[0].packet)

	data, err := pkt.Marshal()
	require.NoError(t, err)
	reader := &MediaSession{
		Laddr:    sess.Laddr,
		Raddr:    sess.Raddr,
		RTPDebug: true,
		Tracer:   tracer,
		rtpConn: &fakes.UDPConn{
			RAddr:  sess.Raddr,
			Reader: bytes.NewReader(data),
		},
	}
	readPkt := &rtp.Packet{}
	_, err = reader.ReadRTP(make([]byte, RTPBufSize), readPkt)
	require.NoError(t, err)
	require.Len(t, tracer.reads, 1)
	require.Equal(t, reader.Laddr.String(), tracer.reads[0].laddr)
	require.Equal(t, reader.Raddr.String(), tracer.reads[0].raddr)
	require.Equal(t, pkt.SequenceNumber, tracer.reads[0].packet.SequenceNumber)
	require.Equal(t, pkt.Timestamp, tracer.reads[0].packet.Timestamp)
	require.Equal(t, pkt.PayloadType, tracer.reads[0].packet.PayloadType)
	require.Equal(t, pkt.SSRC, tracer.reads[0].packet.SSRC)
	require.Equal(t, pkt.Payload, tracer.reads[0].packet.Payload)

	// Without a tracer the packets go to the session logger.
	var logs bytes.Buffer
	reader.Tracer = nil
	reader.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logRTPWrite(reader, pkt)
	require.Contains(t, logs.String(), "RTP write")
}

func TestRTPDebugTracerSkipsFailedWrite(t *testing.T) {
	tracer := &testRTPTracer{}

	sess := &MediaSession{
		Laddr:    net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Raddr:    net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		RTPDebug: true,
		Tracer:   tracer,
		rtpConn: &fakes.UDPConn{
			Writers: map[string]io.Writer{},
		},
	}

	err := sess.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2}})
	require.Error(t, err)
	require.Empty(t, tracer.writes)
}
