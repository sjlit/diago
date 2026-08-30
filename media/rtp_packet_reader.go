// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// var rtpBufPool = &sync.Pool{
// 	New: func() any { return make([]byte, RTPBufSize) },
// }

type RTPReader interface {
	ReadRTP(buf []byte, p *rtp.Packet) (int, error)
}

// RTPReaderRaw is a raw byte-level RTP reader abstraction.
//
// Deprecated: No implementation and no use; use RTPPacketReader instead. It will be removed.
type RTPReaderRaw interface {
	ReadRTPRaw(buf []byte) (int, error)
}

// RTCPReader is an RTCP reader abstraction.
//
// Deprecated: No implementation and no use; use RTPPacketReader instead. It will be removed.
type RTCPReader interface {
	ReadRTCP(buf []byte, pkts []rtcp.Packet) (n int, err error)
}

// RTPCReaderRaw is a raw byte-level RTCP reader abstraction.
//
// Deprecated: No implementation and no use; use RTPPacketReader instead. It will be removed.
type RTPCReaderRaw interface {
	ReadRTCPRaw(buf []byte) (int, error)
}

// RTPPacketReader reads RTP packet and extracts payload and header
type RTPPacketReader struct {
	mu  sync.RWMutex
	log *slog.Logger

	reader RTPReader

	// pokeTarget is the conn owner used to interrupt in-flight reads (deadline
	// poke). Nil for wrapper readers, which are interrupted through the
	// readPauser signal instead. Guarded by mu.
	pokeTarget readInterrupter
	// pauseCount is the refcount of active PauseRead holders. Guarded by mu
	// for updates, loaded atomically on the read hot path.
	pauseCount atomic.Int32
	// pauseSignal is non-nil while paused; closed channel. Guarded by mu.
	pauseSignal chan struct{}

	// PacketHeader is stored after calling Read
	// Safe to read only in same goroutine as Read
	PacketHeader rtp.Header
	// packet is temporarly packet holder for header and data
	packet rtp.Packet

	// payloadType uint8
	seqReader RTPExtendedSequenceNumber

	unreadPayload []byte
	unread        int
	// We want to track our last SSRC.
	lastSSRC uint32
}

// NewRTPPacketReaderSession just helper constructor
func NewRTPPacketReaderSession(sess *RTPSession) *RTPPacketReader {
	r := newRTPPacketReaderMedia(sess.Sess)
	r.reader = sess
	return r
}

// used for tests only
func newRTPPacketReaderMedia(sess *MediaSession) *RTPPacketReader {
	codec := CodecAudioFromSession(sess)
	w := NewRTPPacketReader(sess, codec)
	return w
}

func NewRTPPacketReader(reader RTPReader, codec Codec) *RTPPacketReader {
	w := RTPPacketReader{
		reader: reader,
		// payloadType:   codec.PayloadType,
		seqReader: RTPExtendedSequenceNumber{},
		// unreadPayload: make([]byte, RTPBufSize),
		// rtpBuffer:     make([]byte, RTPBufSize),
		log: DefaultLogger().With("caller", "media"),
	}
	// Track the conn owner for read interrupts; wrapper readers resolve it
	// via UpdateReader or interrupt through the readPauser signal.
	switch s := reader.(type) {
	case *RTPSession:
		w.pokeTarget = s.Sess
	case *MediaSession:
		w.pokeTarget = s
	}

	return &w
}

// Read Implements io.Reader and extracts Payload from RTP packet
// has no input queue or sorting control of packets
// Buffer is used for reading headers and after parsing Headers are stored in PacketHeader
//
// NOTE: Consider that if you are passsing smaller buffer than RTP header+payload, io.ErrShortBuffer is returned
func (r *RTPPacketReader) Read(b []byte) (int, error) {
	if r.paused() {
		return 0, ErrReadPaused
	}
	if r.unread > 0 {
		n := r.readPayload(b, r.unreadPayload[:r.unread])
		return n, nil
	}

	var n int

	// For io.ReadAll buffer size is constantly changing and starts small. Normally user should set buf > rtp Packet size
	// Use unread buffer and still avoid alloc
	buf := b
	unreadPayload := b
	if len(b) < RTPBufSize {
		if r.unreadPayload == nil {
			r.log.Debug("Read RTP buf is < RTPBufSize!!! Creating larger buffer!!!")
			r.unreadPayload = make([]byte, RTPBufSize)
		}

		buf = r.unreadPayload
		unreadPayload = r.unreadPayload
	}

	pkt := &r.packet

	r.mu.RLock()
	reader := r.reader
	r.mu.RUnlock()

	// NOTE: Packet Payload can or will reference this buffer as payload. To return only payload copy is required
	// DO NOT EXPOSE Payload from this point
	rtpN, err := reader.ReadRTP(buf, pkt)
	if err != nil {
		// A pause interrupts the in-flight read; surface it before any retry.
		if r.paused() {
			return 0, ErrReadPaused
		}
		// In case we error while new reader update happen, then retry again
		// This can be deadline, timeout, or connection closed
		r.mu.RLock()
		newReader := r.reader
		r.mu.RUnlock()
		if newReader != reader {
			// Make sure read is enabled if this is rtp connection
			// Reason is we SetDeadline on Update but media session may not change connection
			// TODO we may need to expose this
			if ms, ok := newReader.(*MediaSession); ok {
				ms.rtpConn.SetReadDeadline(time.Time{})
			}
			rtpN, err = newReader.ReadRTP(buf, pkt)
		}
	}
	if err != nil {
		// For now underhood IO should only have net closed
		// Here we are returning EOF to be io package compatilble
		// like with func io.ReadAll
		if errors.Is(err, net.ErrClosed) {
			return 0, io.EOF
		}
		return 0, err
	}
	if rtpN == 0 {
		// Call on hold or keep alive
		r.log.Debug("ZERO Payload on RTP", "header", pkt.Header)
		return 0, nil
	}

	// payloadSize := rtpN - pkt.Header.MarshalSize() - int(pkt.PaddingSize)
	payloadSize := len(pkt.Payload)

	// Keeping this for double check
	if calcPayloadSize := rtpN - pkt.Header.MarshalSize() - int(pkt.PaddingSize); calcPayloadSize != payloadSize {
		return 0, fmt.Errorf("payload size calc does not match. calc=%d payload=%d header=%+v", calcPayloadSize, payloadSize, pkt.Header)
	}
	// payloadSize := len(pkt.Payload)
	// In case of DTMF we can receive different payload types
	// if pt != pkt.PayloadType {
	// 	return 0, fmt.Errorf("payload type does not match. expected=%d, actual=%d", pt, pkt.PayloadType)
	// }
	if payloadSize <= 0 {
		r.log.Debug("Invalid payload size", "rtpN", rtpN, "headerSize", pkt.Header.MarshalSize(), "padding", pkt.PaddingSize)
		return 0, nil
	}

	// If we are tracking this source, do check are we keep getting pkts in sequence
	if r.lastSSRC == pkt.SSRC {
		prevSeq := r.seqReader.ReadExtendedSeq()
		if err := r.seqReader.UpdateSeq(pkt.SequenceNumber); err != nil {
			r.log.Warn(err.Error())
		}

		newSeq := r.seqReader.ReadExtendedSeq()
		if prevSeq+1 != newSeq {
			r.log.Debug("Out of order pkt received", "expected", prevSeq+1, "actual", newSeq, "real", pkt.SequenceNumber)
		}
	} else {
		r.seqReader.InitSeq(pkt.SequenceNumber)
	}

	r.lastSSRC = pkt.SSRC
	r.PacketHeader = pkt.Header

	// Do we use larger buffer, then copy and save unread
	if len(b) < len(unreadPayload) {
		n = r.readPayload(b, pkt.Payload)
		return n, nil
	}

	n = copy(b, pkt.Payload)
	if n < payloadSize {
		return n, io.ErrShortBuffer
	}
	return n, nil
}

func (r *RTPPacketReader) readPayload(b []byte, payload []byte) int {
	n := copy(b, payload)
	if n < len(payload) {
		written := copy(r.unreadPayload, payload[n:])
		if written < len(payload[n:]) {
			r.log.Error("Payload is huge, it will be unread")
		}
		r.unread = written
	} else {
		r.unread = 0
	}
	return n
}

func (r *RTPPacketReader) Reader() RTPReader {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reader
}

func (r *RTPPacketReader) UpdateRTPSession(rtpSess *RTPSession) {
	r.UpdateReader(rtpSess)

	// codec := CodecFromSession(rtpSess.Sess)
	// r.mu.Lock()
	// r.RTPSession = rtpSess
	// // r.payloadType = codec.PayloadType
	// r.reader = rtpSess
	// r.mu.Unlock()
}

func (r *RTPPacketReader) UpdateReader(reader RTPReader) {
	// codec := CodecFromSession(rtpSess.Sess)
	r.mu.Lock()
	// Make sure that current reading is stopped
	if m, ok := r.reader.(*MediaSession); ok {
		m.rtpConn.SetReadDeadline(time.Now())
	}
	r.reader = reader
	// Maintain the interrupt poke target: session-owned readers are
	// interrupted via deadline poke, wrapper readers via the readPauser
	// signal delivered in applyPause/clearPause.
	switch s := reader.(type) {
	case *RTPSession:
		r.pokeTarget = s.Sess
	case *MediaSession:
		r.pokeTarget = s
	default:
		r.pokeTarget = nil
	}
	// Deliver an active pause to a newly installed pause-aware reader.
	if r.pauseSignal != nil {
		if p, ok := reader.(readPauser); ok {
			p.setReadPause(r.pauseSignal)
		}
	}
	// TODO we need to make sure that current Audio Reading is really stopped before updating
	r.mu.Unlock()
}
