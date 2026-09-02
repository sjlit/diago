// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

type RTPDtmfWriter struct {
	codec        Codec
	writer       io.Writer
	packetWriter *RTPPacketWriter

	mu sync.Mutex
}

// RTP DTMF writer is midleware for passing RTP DTMF event.
// If it is chained it uses to block writer while writing DTFM events
func NewRTPDTMFWriter(codec Codec, rtpPacketizer *RTPPacketWriter, writer io.Writer) *RTPDtmfWriter {
	return &RTPDtmfWriter{
		codec:        codec,
		packetWriter: rtpPacketizer,
		writer:       writer,
	}
}

// Write is RTP io.Writer which adds more sync mechanism
func (w *RTPDtmfWriter) Write(b []byte) (int, error) {
	// If locked it means writer is currently writing DTMF over same stream
	w.mu.Lock()
	defer w.mu.Unlock()
	// Write whatever is intended
	n, err := w.writer.Write(b)
	if err != nil {
		return n, err
	}

	return n, nil
}

// WriteDTMF encodes and sends one RFC 4733 event with defaults
// (volume 10, 80ms hold). It blocks for the whole event (~7 * codec.SampleDur).
// A concurrent PauseWrite gate is waited out; use WriteDTMFContext when that
// wait must be cancellable.
func (w *RTPDtmfWriter) WriteDTMF(dtmf rune) error {
	return w.WriteDTMFContext(context.Background(), dtmf)
}

// WriteDTMFContext is WriteDTMF with a context: ctx never starts the event
// once canceled and it interrupts a wait spent behind the PauseWrite gate.
// An event whose first packet was accepted completes regardless.
func (w *RTPDtmfWriter) WriteDTMFContext(ctx context.Context, dtmf rune) error {
	return w.WriteDTMFWithOptionsContext(ctx, dtmf, DTMFEncodeOptions{})
}

// WriteDTMFWithOptions sends one event with tuned encoding (volume, event hold).
// The event packets ride on the packet writer's SSRC with their own timestamp
// (no audio clock advance), paced one codec.SampleDur per packet. It works at
// any telephone-event clock rate (8k/16k/32k/48k per RFC 4733).
func (w *RTPDtmfWriter) WriteDTMFWithOptions(dtmf rune, opts DTMFEncodeOptions) error {
	return w.WriteDTMFWithOptionsContext(context.Background(), dtmf, opts)
}

// WriteDTMFWithOptionsContext is WriteDTMFWithOptions with a context. A
// canceled context never starts the event, and it cuts short a wait on a
// paused write gate (returning ctx.Err()); once the first packet is accepted
// the event completes, so cancellation latency is one event with the gate
// open and immediate while it stays closed.
func (w *RTPDtmfWriter) WriteDTMFWithOptionsContext(ctx context.Context, dtmf rune, opts DTMFEncodeOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	evs, err := RTPDTMFEncode(w.codec, dtmf, opts)
	if err != nil {
		return err
	}

	pacing := w.codec.SampleDur
	if pacing <= 0 {
		// A hand-built codec may lack SampleDur; RFC 4733 events are
		// conventionally sent one per audio packet interval (20ms).
		pacing = 20 * time.Millisecond
	}
	ticker := time.NewTicker(pacing)
	defer ticker.Stop()
	for i, e := range evs {
		data := DTMFEncode(e)
		marker := i == 0

		// https://datatracker.ietf.org/doc/html/rfc2833#section-3.6
		// 		An audio source SHOULD start transmitting event packets as soon as it
		//    recognizes an event and every 50 ms thereafter or the packet interval
		//    for the audio codec used for this session

		<-ticker.C
		// We are simulating RTP clock rate
		// timestamp should not be increased for dtmf
		if err := w.writeDTMFSample(ctx, data, marker); err != nil {
			return err
		}
	}
	return nil
}

// writeDTMFSample writes one event packet, waiting out a concurrent
// PauseWrite gate instead of failing mid-event: a transient pause (recording
// tap, hold) must not cut the digit short, and the packet is retried
// unchanged so the event stream stays intact. The wait is interruptible:
// ctx (carried down from WriteDTMFWithOptionsContext) bounds it when the
// gate would otherwise never release.
func (w *RTPDtmfWriter) writeDTMFSample(ctx context.Context, data []byte, marker bool) error {
	for {
		_, err := w.packetWriter.WriteSamples(data, 0, marker, w.codec.PayloadType)
		if !errors.Is(err, ErrWritePaused) {
			return err
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
