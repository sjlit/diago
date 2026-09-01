// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package audio

import (
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"
)

func pcmToSamples(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// Goertzel magnitude of freq in samples.
func toneEnergy(samples []int16, freq float64, sampleRate int) float64 {
	w := 2 * math.Pi * freq / float64(sampleRate)
	coeff := 2 * math.Cos(w)
	s1, s2 := 0.0, 0.0
	for _, s := range samples {
		s0 := float64(s) + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	return math.Sqrt(math.Abs(s1*s1 + s2*s2 - coeff*s1*s2))
}

func TestToneReaderSingleTone(t *testing.T) {
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{440}, On: 100 * time.Millisecond}}}
	buf, err := io.ReadAll(NewToneReader(tone, 8000, 1))
	if err != nil {
		t.Fatal(err)
	}
	// 0.1s * 8000 samples * 2 bytes
	if len(buf) != 1600 {
		t.Fatalf("expected 1600 bytes, got %d", len(buf))
	}
	samples := pcmToSamples(buf)
	e440 := toneEnergy(samples, 440, 8000)
	e300 := toneEnergy(samples, 300, 8000)
	if e440 < 100*e300 {
		t.Fatalf("440Hz must dominate: e440=%f e300=%f", e440, e300)
	}
	// Fades: edges must be far quieter than a mid-tone window (no click).
	// Single-sample comparison of the interior is unsafe (sine zero
	// crossings), so compare narrow windows against mid RMS.
	mid := pcmRMS(samples, 300, 500)
	if samples[0] != 0 {
		t.Fatalf("fade-in must start at zero, got %d", samples[0])
	}
	if r := pcmRMS(samples[:10], 0, 10); r > 0.2*mid {
		t.Fatalf("fade-in missing: edge rms=%f mid=%f", r, mid)
	}
	if r := pcmRMS(samples[len(samples)-10:], 0, 10); r > 0.25*mid {
		t.Fatalf("fade-out missing: edge rms=%f mid=%f", r, mid)
	}
}

func TestToneReaderCadenceSilence(t *testing.T) {
	// 450 Hz 1s on, 1s off
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{450}, On: time.Second, Off: time.Second}}}
	buf, err := io.ReadAll(NewToneReader(tone, 8000, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 2*8000*2 {
		t.Fatalf("expected 2s of audio, got %d bytes", len(buf))
	}
	samples := pcmToSamples(buf)
	off := samples[8000:]
	for _, s := range off {
		if s != 0 {
			t.Fatal("off period must be silence")
		}
	}
	if toneEnergy(samples[:8000], 450, 8000) < 1000 {
		t.Fatal("on period must contain 450Hz")
	}
}

func TestToneReaderStereo(t *testing.T) {
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{700}, On: 20 * time.Millisecond}}}
	buf, err := io.ReadAll(NewToneReader(tone, 8000, 2))
	if err != nil {
		t.Fatal(err)
	}
	// mono 8k 20ms = 160 samples; stereo doubles frames
	if len(buf) != 160*2*2 {
		t.Fatalf("expected %d bytes, got %d", 160*2*2, len(buf))
	}
	samples := pcmToSamples(buf)
	for i := 0; i < len(samples); i += 2 {
		if samples[i] != samples[i+1] {
			t.Fatalf("channel %d != %d", samples[i], samples[i+1])
		}
	}
}

func TestToneReaderLoop(t *testing.T) {
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{440}, On: 50 * time.Millisecond}}}
	r := NewToneReader(tone, 8000, 1)
	r.Loop()
	buf := make([]byte, 8000*2*2) // 2 periods = 200ms
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("looped reader must fill the buffer, got %d", n)
	}
	a := pcmToSamples(buf[:8000*2])
	b := pcmToSamples(buf[8000*2:])
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("period not repeated at %d", i)
		}
	}
}

func TestToneReaderEmptySegmentsEOF(t *testing.T) {
	r := NewToneReader(Tone{}, 8000, 1)
	if _, err := r.Read(make([]byte, 320)); err != io.EOF {
		t.Fatalf("empty tone must EOF, got %v", err)
	}
}

func pcmRMS(s []int16, from, to int) float64 {
	var acc float64
	for _, v := range s[from:to] {
		acc += float64(v) * float64(v)
	}
	return math.Sqrt(acc / float64(to-from))
}

func TestWithVolume(t *testing.T) {
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{440}, On: 100 * time.Millisecond}}}
	full := pcmToSamples(mustRender(t, tone, 1.0))
	half := pcmToSamples(mustRender(t, tone, 0.5))
	// compare mid-tone windows (800 samples total, skip the 5ms fades = 40 samples each side)
	rf, rh := pcmRMS(full, 400, 500), pcmRMS(half, 400, 500)
	if math.Abs(rf/2-rh)/rf > 0.05 {
		t.Fatalf("WithVolume(0.5) should halve RMS: full=%f half=%f", rf, rh)
	}
}

func mustRender(t *testing.T, tone Tone, scale float64) []byte {
	t.Helper()
	buf, err := io.ReadAll(NewToneReader(tone.WithVolume(scale), 8000, 1))
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

func TestToneDTMFDigit(t *testing.T) {
	// Each digit must map to its correct row/column dual tone.
	// Frequency pairs match dtmfToneFreqs exactly (order irrelevant).
	cases := map[rune][2]float64{
		'1': {697, 1209}, '5': {770, 1336}, '0': {941, 1336},
		'*': {941, 1209}, '#': {941, 1477},
		'A': {697, 1633}, 'B': {770, 1633}, 'C': {852, 1633}, 'D': {941, 1633},
	}
	for digit, freqs := range cases {
		tone, err := ToneDTMFDigit(digit)
		if err != nil {
			t.Fatalf("digit %c: %v", digit, err)
		}
		if len(tone.Segments) != 1 || tone.Segments[0].On <= 0 {
			t.Fatalf("digit %c: bad segment", digit)
		}
		buf, err := io.ReadAll(NewToneReader(tone, 8000, 1))
		if err != nil {
			t.Fatal(err)
		}
		samples := pcmToSamples(buf)
		for _, f := range freqs {
			if toneEnergy(samples, f, 8000) < 1000 {
				t.Fatalf("digit %c: missing frequency %v", digit, f)
			}
		}
	}
	// lowercase a-d accepted, invalid chars rejected
	if _, err := ToneDTMFDigit('a'); err != nil {
		t.Fatalf("lowercase a must be accepted: %v", err)
	}
	if _, err := ToneDTMFDigit('x'); err == nil {
		t.Fatal("'x' must be rejected")
	}
}

func TestToneDTMFDigitSampleRates(t *testing.T) {
	// The 80ms default hold must scale with the clock: 8k -> 640 samples,
	// 16k -> 1280, 48k -> 3840.
	tone, err := ToneDTMFDigit('7')
	if err != nil {
		t.Fatal(err)
	}
	for _, rate := range []int{8000, 16000, 48000} {
		buf, err := io.ReadAll(NewToneReader(tone, rate, 1))
		if err != nil {
			t.Fatal(err)
		}
		want := int(0.08*float64(rate)) * 2
		if len(buf) != want {
			t.Fatalf("%d Hz: expected %d bytes, got %d", rate, want, len(buf))
		}
	}
}

func TestPresetTonesFinite(t *testing.T) {
	for name, tone := range map[string]Tone{"dial": ToneDial, "busy": ToneBusy, "ringback": ToneRingback} {
		buf, err := io.ReadAll(NewToneReader(tone, 8000, 1))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(buf) == 0 {
			t.Fatalf("%s: empty", name)
		}
	}
}
