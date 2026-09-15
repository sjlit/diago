// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"context"
	"io"
	"log/slog"
)

type RTPDTMFReader struct {
	codec        Codec // Depends on media session. Defaults to 101 per current mapping
	reader       io.Reader
	packetReader *RTPPacketReader

	lastTimestamp uint32
	// completedTimestamp is the RTP timestamp of the last completed event and
	// hasCompleted reports whether one was seen. RFC 4733 allows the end
	// packet of an event to be retransmitted; a single-packet event (M=1,E=1)
	// retransmission carries the same timestamp and M bit, so without this it
	// would be reported as a second key press.
	completedTimestamp uint32
	hasCompleted       bool

	lastEv    DTMFEvent
	dtmf      rune
	isDTMFEnd bool
}

// logger returns the reader's logger, inherited from the packet reader handle
// it decodes events from.
func (r *RTPDTMFReader) logger() *slog.Logger {
	if r.packetReader != nil && r.packetReader.log != nil {
		return r.packetReader.log
	}
	return DefaultLogger()
}

// RTP DTMF reader is middleware for reading DTMF events.
// It reads from io Reader and checks packet Reader
func NewRTPDTMFReader(codec Codec, packetReader *RTPPacketReader, reader io.Reader) *RTPDTMFReader {
	return &RTPDTMFReader{
		codec:        codec,
		packetReader: packetReader,
		reader:       reader,
		// dmtfs:        make([]rune, 0, 5), // have some
	}
}

// Write is RTP io.Writer which adds more sync mechanism
func (w *RTPDTMFReader) Read(b []byte) (int, error) {
	n, err := w.reader.Read(b)
	if err != nil {
		// Signal our reader that no more dtmfs will be read
		// close(w.dtmfCh)
		return n, err
	}

	// Check is this DTMF
	hdr := w.packetReader.PacketHeader
	if hdr.PayloadType != w.codec.PayloadType {
		return n, nil
	}

	// Now decode DTMF
	ev := DTMFEvent{}
	if err := DTMFDecode(b, &ev); err != nil {
		slog.Error("Failed to decode DTMF event", "error", err)
	}

	// A retransmitted end packet keeps the event timestamp. Suppress it so the
	// same digit is not surfaced twice (single-packet events carry M=1 too, so
	// the mbit check below cannot distinguish the retransmission).
	if ev.EndOfEvent && w.hasCompleted && w.completedTimestamp == hdr.Timestamp {
		w.lastTimestamp = hdr.Timestamp
		return n, nil
	}

	w.processDTMFEvent(ev, hdr.Marker || w.lastTimestamp != hdr.Timestamp)
	if ev.EndOfEvent {
		w.completedTimestamp = hdr.Timestamp
		w.hasCompleted = true
	}
	w.lastTimestamp = hdr.Timestamp
	return n, nil
}

func (w *RTPDTMFReader) processDTMFEvent(ev DTMFEvent, mbit bool) {
	if w.logger().Handler().Enabled(context.Background(), slog.LevelDebug) {
		// Expensive call on logger
		w.logger().Debug("Processing DTMF event", "ev", ev, "mbit", mbit)
	}
	if ev.EndOfEvent {
		if mbit {
			// RFC 4733 §3.6: a short event may fit a single packet carrying
			// both the marker and end bit. There is no preceding start
			// packet, so lastEv is empty here - complete it directly.
			w.completeEvent(ev)
			return
		}
		if w.lastEv.Duration == 0 {
			// Ignore this packet if we had no lastEv set
			// This can be also due to EndEvent retransmission
			return
		}
		// Does this match to our last ev
		// Consider Event can be 0, that is why we check is also lastEv.Duration set
		if w.lastEv.Event != ev.Event {
			return
		}

		w.completeEvent(ev)
		return
	}

	// if ev.Duration == 0 {
	// 	// Should be ignored
	// 	return
	// }

	// Keep playing until new marker is set
	if !mbit {
		return
	}

	// New packet
	w.lastEv = ev
}

// completeEvent surfaces the digit and clears the tracked start event.
// Event codes above 15 are RFC 4733 line events (ex. flash), not DTMF keys.
func (w *RTPDTMFReader) completeEvent(ev DTMFEvent) {
	if ev.Event > 15 {
		return
	}
	w.dtmf = DTMFToRune(ev.Event)
	w.isDTMFEnd = true
	w.lastEv = DTMFEvent{}
}

func (w *RTPDTMFReader) ReadDTMF() (rune, bool) {
	defer func() { w.isDTMFEnd = false }()
	return w.dtmf, w.isDTMFEnd
	// dtmf, ok := <-w.dtmfCh
	// return DTMFToRune(dtmf), ok
}
