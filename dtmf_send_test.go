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
