// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"context"
	"io"
	"log/slog"
)

type RTPDtmfReader struct {
	codec        Codec // Depends on media session. Defaults to 101 per current mapping
	reader       io.Reader
	packetReader *RTPPacketReader

	lastTimestamp uint32

	lastEv  DTMFEvent
	dtmf    rune
	dtmfEnd bool
}

// RTP DTMF writer is midleware for reading DTMF events
// It reads from io Reader and checks packet Reader
func NewRTPDTMFReader(codec Codec, packetReader *RTPPacketReader, reader io.Reader) *RTPDtmfReader {
	return &RTPDtmfReader{
		codec:        codec,
		packetReader: packetReader,
		reader:       reader,
		// dmtfs:        make([]rune, 0, 5), // have some
	}
}

// Write is RTP io.Writer which adds more sync mechanism
func (w *RTPDtmfReader) Read(b []byte) (int, error) {
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
	w.processDTMFEvent(ev, hdr.Marker || w.lastTimestamp != hdr.Timestamp)
	w.lastTimestamp = hdr.Timestamp
	return n, nil
}

func (w *RTPDtmfReader) processDTMFEvent(ev DTMFEvent, mbit bool) {
	if DefaultLogger().Handler().Enabled(context.Background(), slog.LevelDebug) {
		// Expensive call on logger
		DefaultLogger().Debug("Processing DTMF event", "ev", ev, "mbit", mbit)
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
func (w *RTPDtmfReader) completeEvent(ev DTMFEvent) {
	if ev.Event > 15 {
		return
	}
	w.dtmf = DTMFToRune(ev.Event)
	w.dtmfEnd = true
	w.lastEv = DTMFEvent{}
}

func (w *RTPDtmfReader) ReadDTMF() (rune, bool) {
	defer func() { w.dtmfEnd = false }()
	return w.dtmf, w.dtmfEnd
	// dtmf, ok := <-w.dtmfCh
	// return DTMFToRune(dtmf), ok
}
