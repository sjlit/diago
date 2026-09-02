// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjlit/diago/media"
)

func dtmfEvents(sink *capturedRTPWriter) []media.DTMFEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var evs []media.DTMFEvent
	for _, p := range sink.packets {
		if p.PayloadType != media.CodecTelephoneEvent8000.PayloadType {
			continue
		}
		var ev media.DTMFEvent
		if err := media.DTMFDecode(p.Payload, &ev); err != nil {
			continue
		}
		evs = append(evs, ev)
	}
	return evs
}

func TestSendDTMFRTPNegotiated(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	if err := m.SendDTMF(context.Background(), "19"); err != nil {
		t.Fatal(err)
	}
	evs := dtmfEvents(sink)
	// 2 digits * 7 packets
	if len(evs) != 14 {
		t.Fatalf("expected 14 dtmf packets, got %d", len(evs))
	}
	if evs[0].Event != 1 {
		t.Fatalf("first digit: %+v", evs[0])
	}
	if evs[7].Event != 9 {
		t.Fatalf("second digit: %+v", evs[7])
	}
}

func TestSendDTMFMethodAndOptions(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	err := m.SendDTMF(context.Background(), "5",
		WithDTMFMethod(DTMFMethodRTP),
		WithDTMFVolume(20),
		WithDTMFEventDuration(40*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	evs := dtmfEvents(sink)
	if len(evs) != 7 {
		t.Fatalf("got %d", len(evs))
	}
	if evs[0].Volume != 20 || evs[0].Duration != 80 {
		t.Fatalf("options not applied: %+v", evs[0])
	}
}

func TestSendDTMFAutoFallsBackToInband(t *testing.T) {
	m, sink := newFakeDialogMedia(t, []media.Codec{media.CodecAudioUlaw}) // no tel-event
	err := m.SendDTMF(context.Background(), "5", WithDTMFInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(dtmfEvents(sink)) != 0 {
		t.Fatal("must not emit PT101 when not negotiated")
	}
	if sink.count() < 4 {
		t.Fatalf("inband tone should produce audio packets, got %d", sink.count())
	}
	for _, p := range sink.packets {
		if p.PayloadType != media.CodecAudioUlaw.PayloadType {
			t.Fatalf("inband digit must ride the audio PT, got %d", p.PayloadType)
		}
	}
}

func TestSendDTMFRTPExplicitUnsupported(t *testing.T) {
	m, _ := newFakeDialogMedia(t, []media.Codec{media.CodecAudioUlaw})
	err := m.SendDTMF(context.Background(), "5", WithDTMFMethod(DTMFMethodRTP))
	if !errors.Is(err, ErrDTMFUnsupported) {
		t.Fatalf("want ErrDTMFUnsupported, got %v", err)
	}
}

func TestSendDTMFExplicitInband(t *testing.T) {
	// Inband forced even though the peer negotiated telephone-event: the
	// digit must ride the audio payload type, no PT101 events.
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	err := m.SendDTMF(context.Background(), "7",
		WithDTMFMethod(DTMFMethodInband), WithDTMFInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(dtmfEvents(sink)) != 0 {
		t.Fatal("explicit inband must not emit PT101")
	}
	if sink.count() < 2 {
		t.Fatalf("explicit inband must produce audio packets, got %d", sink.count())
	}
}

func TestSendDTMFVolumeZero(t *testing.T) {
	// Volume 0 is a legal wire value (loudest); the option must pass it
	// through instead of falling back to the default 10.
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	if err := m.SendDTMF(context.Background(), "1", WithDTMFVolume(0)); err != nil {
		t.Fatal(err)
	}
	evs := dtmfEvents(sink)
	if len(evs) == 0 {
		t.Fatal("no dtmf packets")
	}
	if evs[0].Volume != 0 {
		t.Fatalf("explicit volume 0 must reach the wire, got %d", evs[0].Volume)
	}
}

func TestSendDTMFInvalidDigit(t *testing.T) {
	m, _ := newFakeDialogMedia(t, ulawCodecs())
	if err := m.SendDTMF(context.Background(), "5x"); err == nil {
		t.Fatal("must reject non DTMF digits")
	}
}

func TestSendDTMFContextCancel(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := m.SendDTMF(ctx, "111111") // ~1 digit per 160ms: cannot finish
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ctx error, got %v", err)
	}
	if n := len(dtmfEvents(sink)); n == 0 || n >= 6*7 {
		t.Fatalf("partial send expected, got %d packets", n)
	}
}

func TestSendDTMFEmpty(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	if err := m.SendDTMF(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 0 {
		t.Fatal("empty digits must be a no-op")
	}
}

func TestSendDTMFContextCancelWhileGatePaused(t *testing.T) {
	// The RTP event write waits on a paused audio-write gate; SendDTMF's ctx
	// must cut that wait short instead of hanging until the gate releases.
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	release, err := m.PauseAudioWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := m.SendDTMF(ctx, "5"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error from paused wait, got %v", err)
	}
	if n := len(dtmfEvents(sink)); n != 0 {
		t.Fatalf("no event packet may reach the wire while paused, got %d", n)
	}
}

func TestSendDTMFConcurrentSerialized(t *testing.T) {
	// Two concurrent SendDTMF calls must not interleave digits: the dialog
	// send gate serializes whole strings, so the wire shows one contiguous
	// digit run followed by the other.
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	errCh := make(chan error, 2)
	go func() { errCh <- m.SendDTMF(context.Background(), "1", WithDTMFInterval(time.Millisecond)) }()
	go func() { errCh <- m.SendDTMF(context.Background(), "2", WithDTMFInterval(time.Millisecond)) }()
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}

	evs := dtmfEvents(sink)
	if len(evs) != 14 {
		t.Fatalf("expected 14 dtmf packets, got %d", len(evs))
	}
	// First 7 packets must be one digit, next 7 the other — no interleaving.
	first, second := evs[0].Event, evs[7].Event
	if first == second {
		t.Fatalf("expected two distinct digits, got %d twice", first)
	}
	for i, e := range evs {
		want := first
		if i >= 7 {
			want = second
		}
		if e.Event != want {
			t.Fatalf("interleaved digits at packet %d: want %d got %d", i, want, e.Event)
		}
	}
}
