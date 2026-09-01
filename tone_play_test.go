// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

type capturedRTPWriter struct {
	mu      sync.Mutex
	packets []*rtp.Packet
}

func (c *capturedRTPWriter) WriteRTP(p *rtp.Packet) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *p
	cp.Payload = append([]byte(nil), p.Payload...)
	c.packets = append(c.packets, &cp)
	return nil
}

func (c *capturedRTPWriter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.packets)
}

// newFakeDialogMedia builds a DialogMedia with an in-memory write path.
func newFakeDialogMedia(t *testing.T, codecs []media.Codec) (*DialogMedia, *capturedRTPWriter) {
	t.Helper()
	sink := &capturedRTPWriter{}
	sess := &media.MediaSession{Codecs: codecs}
	pw := media.NewRTPPacketWriter(sink, media.CodecAudioUlaw)
	m := &DialogMedia{}
	m.initMediaSessionUnsafe(sess, nil, pw)
	return m, sink
}

func ulawCodecs() []media.Codec {
	return []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000}
}

func TestPlayToneFinite(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())

	tone := audio.Tone{Segments: []audio.ToneSegment{{Freqs: []float64{450}, On: 100 * time.Millisecond}}}
	if err := m.PlayTone(context.Background(), tone); err != nil {
		t.Fatal(err)
	}
	// 100ms of 20ms ulaw packets
	if n := sink.count(); n < 4 || n > 6 {
		t.Fatalf("expected ~5 ulaw packets, got %d", n)
	}
	for _, p := range sink.packets {
		if p.PayloadType != media.CodecAudioUlaw.PayloadType {
			t.Fatalf("audio must ride PT 0, got %d", p.PayloadType)
		}
	}
}

func TestPlayToneLoopUntilCancel(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.PlayTone(ctx, audio.ToneDial, WithToneLoop())
	}()

	time.Sleep(120 * time.Millisecond)
	cancel()
	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("looped tone must return ctx error on cancel, got %v", err)
	}
	if sink.count() < 4 {
		t.Fatalf("tone should have been streaming before cancel, got %d packets", sink.count())
	}
}

func TestPlayToneWaitsWritePause(t *testing.T) {
	// A concurrent PauseAudioWrite (refcounted gate owned by another
	// component) must not kill the tone: PlayTone waits for the release and
	// replays the gated frame, losing no samples.
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	release, err := m.PauseAudioWrite()
	if err != nil {
		t.Fatal(err)
	}

	tone := audio.Tone{Segments: []audio.ToneSegment{{Freqs: []float64{450}, On: 40 * time.Millisecond}}}
	done := make(chan error, 1)
	go func() { done <- m.PlayTone(context.Background(), tone) }()

	// Hold the gate past the tone's natural end: it must neither complete
	// nor drop a single packet into the sink while paused.
	time.Sleep(80 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("tone must wait the paused gate, finished early with %v", err)
	default:
	}
	if n := sink.count(); n != 0 {
		t.Fatalf("paused gate must block tone frames, got %d", n)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("tone must resume after gate release, got %v", err)
	}
	if sink.count() < 2 {
		t.Fatalf("expected resumed frames, got %d", sink.count())
	}
}

func TestPlayToneClosedDialog(t *testing.T) {
	m, _ := newFakeDialogMedia(t, ulawCodecs())
	m.closed = true
	err := m.PlayTone(context.Background(), audio.ToneDial)
	if !errors.Is(err, ErrDialogClosed) {
		t.Fatalf("want ErrDialogClosed, got %v", err)
	}
}
