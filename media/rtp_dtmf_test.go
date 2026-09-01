// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"testing"
	"time"
)

func TestIsDTMFEvent(t *testing.T) {
	for _, c := range "0123456789*#ABCD" {
		if !IsDTMFEvent(c) {
			t.Fatalf("%c must be a dtmf event", c)
		}
	}
	if IsDTMFEvent('x') || IsDTMFEvent('a') {
		t.Fatal("non events rejected")
	}
}

func TestRTPDTMFEncodeDefaults8k(t *testing.T) {
	evs, err := RTPDTMFEncode(CodecTelephoneEvent8000, '5', DTMFEncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// legacy layout preserved: 4 active (160..640) + 3 EoE
	if len(evs) != 7 {
		t.Fatalf("expected 7 events, got %d", len(evs))
	}
	wantDur := []uint16{160, 320, 480, 640, 640, 640, 640}
	for i, e := range evs {
		if e.Event != 5 || e.Volume != 10 {
			t.Fatalf("event %d: %+v", i, e)
		}
		if e.Duration != wantDur[i] {
			t.Fatalf("event %d duration %d want %d", i, e.Duration, wantDur[i])
		}
		if wantEoE := i >= 4; e.EndOfEvent != wantEoE {
			t.Fatalf("event %d EndOfEvent=%v", i, e.EndOfEvent)
		}
	}
}

func TestRTPDTMFEncodeClockRates(t *testing.T) {
	cod48 := Codec{PayloadType: 110, SampleRate: 48000, SampleDur: 20 * time.Millisecond, NumChannels: 1, Name: "telephone-event"}
	evs, err := RTPDTMFEncode(cod48, 'A', DTMFEncodeOptions{Volume: 20, VolumeSet: true, EventDuration: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// 100ms @48k = 4800 ticks final; 4 steps of 1200
	if evs[0].Duration != 1200 || evs[3].Duration != 4800 || evs[4].Duration != 4800 {
		t.Fatalf("48k durations wrong: %d %d %d", evs[0].Duration, evs[3].Duration, evs[4].Duration)
	}
	if evs[0].Volume != 20 || evs[0].Event != 12 {
		t.Fatalf("volume/event not honored: %+v", evs[0])
	}
}

func TestRTPDTMFEncodeInvalid(t *testing.T) {
	if _, err := RTPDTMFEncode(CodecTelephoneEvent8000, 'x', DTMFEncodeOptions{}); err == nil {
		t.Fatal("invalid char must error")
	}
}

func TestRTPDTMFEncodeVolumeSentinel(t *testing.T) {
	// Zero value of the struct keeps the legacy default (10)
	evs, err := RTPDTMFEncode(CodecTelephoneEvent8000, '1', DTMFEncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Volume != 10 {
		t.Fatalf("unset volume must default to 10, got %d", evs[0].Volume)
	}
	// Volume 0 is a legal wire value (loudest): needs VolumeSet to express
	evs, err = RTPDTMFEncode(CodecTelephoneEvent8000, '1', DTMFEncodeOptions{Volume: 0, VolumeSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Volume != 0 {
		t.Fatalf("explicit volume 0 must reach the wire, got %d", evs[0].Volume)
	}
	// Out of range clamps to the 6-bit maximum
	evs, err = RTPDTMFEncode(CodecTelephoneEvent8000, '1', DTMFEncodeOptions{Volume: 200, VolumeSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Volume != 63 {
		t.Fatalf("volume 200 must clamp to 63, got %d", evs[0].Volume)
	}
}

func TestRTPDTMFEncode8000Legacy(t *testing.T) {
	// legacy wrapper keeps working, unknown char maps to '0' as before
	evs := RTPDTMFEncode8000('x')
	if evs[0].Event != 0 {
		t.Fatal("legacy unknown char must encode 0")
	}
	if len(evs) != 7 || evs[0].Duration != 160 {
		t.Fatalf("legacy layout changed: %+v", evs)
	}
}

func TestRTPDTMFEncodeZeroRateFallback(t *testing.T) {
	// A hand-built codec without a sample rate must fall back to the
	// canonical 8 kHz telephone-event clock, not put zero durations on the
	// wire. Mirrors the pacing guard in WriteDTMFWithOptions for SampleDur.
	cod := Codec{PayloadType: 101, SampleDur: 20 * time.Millisecond, NumChannels: 1, Name: "telephone-event"}
	evs, err := RTPDTMFEncode(cod, '5', DTMFEncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantDur := []uint16{160, 320, 480, 640, 640, 640, 640}
	for i, e := range evs {
		if e.Duration != wantDur[i] {
			t.Fatalf("event %d duration %d want %d (8k fallback)", i, e.Duration, wantDur[i])
		}
	}

	// The fallback clock also scales explicit event durations.
	evs, err = RTPDTMFEncode(cod, '5', DTMFEncodeOptions{EventDuration: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Duration != 200 || evs[3].Duration != 800 || evs[4].Duration != 800 {
		t.Fatalf("100ms @8k fallback wrong: %d %d %d", evs[0].Duration, evs[3].Duration, evs[4].Duration)
	}
}
