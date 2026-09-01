// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
	"unicode"
)

const (
	// toneDefaultVolume is used when ToneSegment.Volume is zero.
	toneDefaultVolume = 0.25
	// toneFadeDur is the linear fade applied at each On block edge to
	// avoid clicks at tone boundaries.
	toneFadeDur = 5 * time.Millisecond
)

// ToneSegment is a block of tone: a sum of sine waves (telephony signals are
// single or dual tone), followed by a silence tail that forms the cadence.
type ToneSegment struct {
	// Freqs are the tone frequencies in Hz. Empty means pure silence.
	Freqs []float64
	// On is the audible duration. Durations shorter than twice the 5ms edge
	// fade overlap both fades and are smoothly attenuated instead of playing
	// at full level.
	On time.Duration
	// Off is the silence duration after the tone.
	Off time.Duration
	// Volume is 0..1 of full scale. Zero means toneDefaultVolume.
	Volume float64
}

// Tone is a finite sequence of tone segments.
type Tone struct {
	Segments []ToneSegment
}

// WithVolume returns a copy of the tone with every segment volume scaled by
// scale (clamped to (0, 1] to keep headroom). Segments relying on the default
// volume are scaled from toneDefaultVolume.
func (t Tone) WithVolume(scale float64) Tone {
	if scale <= 0 {
		return t
	}
	if scale > 1 {
		scale = 1
	}
	out := Tone{Segments: make([]ToneSegment, len(t.Segments))}
	for i, s := range t.Segments {
		base := s.Volume
		if base == 0 {
			base = toneDefaultVolume
		}
		s.Volume = base * scale
		out.Segments[i] = s
	}
	return out
}

// ToneReader synthesizes a tone as PCM16 little endian interleaved samples.
// The output feeds AudioPlayback ("audio/pcm") or audio.PCMEncoderWriter.
type ToneReader struct {
	tone        Tone
	sampleRate  int
	numChannels int
	loop        bool

	segIdx int
	segPos int // samples consumed within the current segment
}

// NewToneReader creates a reader. Zero/negative sampleRate falls back to 8000,
// numChannels to 1.
func NewToneReader(tone Tone, sampleRate, numChannels int) *ToneReader {
	if sampleRate <= 0 {
		sampleRate = 8000
	}
	if numChannels <= 0 {
		numChannels = 1
	}
	return &ToneReader{tone: tone, sampleRate: sampleRate, numChannels: numChannels}
}

// Loop makes the reader repeat its segment sequence forever; Read then never
// returns io.EOF. Stop by discarding the reader (the consumer controls pacing).
func (r *ToneReader) Loop() { r.loop = true }

// Read fills p with PCM16 LE interleaved samples. When the (non looping)
// sequence is exhausted it returns io.EOF, or the bytes read so far if a
// partial frame was produced. p must be large enough for one frame.
func (r *ToneReader) Read(p []byte) (int, error) {
	frame := 2 * r.numChannels
	if len(p) < frame {
		return 0, io.ErrShortBuffer
	}
	written := 0
	for written+frame <= len(p) {
		sample, ok := r.nextSample()
		if !ok {
			if written == 0 {
				return 0, io.EOF
			}
			break
		}
		for ch := 0; ch < r.numChannels; ch++ {
			binary.LittleEndian.PutUint16(p[written:written+2], uint16(sample))
			written += 2
		}
	}
	return written, nil
}

// nextSample returns the next mono sample value, advancing the position.
// ok=false when the (non looping) sequence is exhausted.
func (r *ToneReader) nextSample() (int16, bool) {
	for {
		if r.segIdx >= len(r.tone.Segments) {
			if !r.loop {
				return 0, false
			}
			r.segIdx = 0
			r.segPos = 0
		}
		seg := r.tone.Segments[r.segIdx]
		onN := int(float64(r.sampleRate) * seg.On.Seconds())
		offN := int(float64(r.sampleRate) * seg.Off.Seconds())
		if r.segPos >= onN+offN {
			r.segIdx++
			r.segPos = 0
			continue
		}
		pos := r.segPos
		r.segPos++
		if pos >= onN || len(seg.Freqs) == 0 {
			return 0, true // silence part of the segment
		}

		vol := seg.Volume
		if vol == 0 {
			vol = toneDefaultVolume
		}
		var v float64
		for _, f := range seg.Freqs {
			v += math.Sin(2 * math.Pi * f * float64(pos) / float64(r.sampleRate))
		}
		v /= float64(len(seg.Freqs))

		fade := int(float64(r.sampleRate) * toneFadeDur.Seconds())
		if fade > 0 {
			if pos < fade {
				v *= float64(pos) / float64(fade)
			}
			if remain := onN - pos; remain < fade {
				v *= float64(remain) / float64(fade)
			}
		}
		return int16(v * vol * math.MaxInt16), true
	}
}

// RenderTonePCM renders a tone to a PCM16 little endian mono byte slice.
func RenderTonePCM(tone Tone, sampleRate int) []byte {
	buf := &bytes.Buffer{}
	io.Copy(buf, NewToneReader(tone, sampleRate, 1))
	return buf.Bytes()
}

// Common telephony tone presets. Cadences are regional (CN/ETSI/US differ);
// these are reasonable defaults — build a custom Tone for country specific
// plans. Loop presets that model an ongoing signal (ringback, dial) on the
// consumer side.
var (
	// ToneDial is the continuous dial tone (350+440 Hz).
	ToneDial = Tone{Segments: []ToneSegment{{Freqs: []float64{350, 440}, On: 2 * time.Second}}}
	// ToneBusy is the US busy tone (480+620 Hz, 2s on / 2s off).
	ToneBusy = Tone{Segments: []ToneSegment{{Freqs: []float64{480, 620}, On: 2 * time.Second, Off: 2 * time.Second}}}
	// ToneRingback is the CN ringback tone (450 Hz, 1s on / 4s off).
	ToneRingback = Tone{Segments: []ToneSegment{{Freqs: []float64{450}, On: 1 * time.Second, Off: 4 * time.Second}}}
)

// dtmfToneFreqs maps digits to their dual tone [row, col] frequencies
// (ITU-T Q.23 / RFC 4733 tone plan).
var dtmfToneFreqs = map[rune][2]float64{
	'1': {697, 1209}, '2': {697, 1336}, '3': {697, 1477}, 'A': {697, 1633},
	'4': {770, 1209}, '5': {770, 1336}, '6': {770, 1477}, 'B': {770, 1633},
	'7': {852, 1209}, '8': {852, 1336}, '9': {852, 1477}, 'C': {852, 1633},
	'*': {941, 1209}, '0': {941, 1336}, '#': {941, 1477}, 'D': {941, 1633},
}

// ToneDTMFDigit returns the inband dual tone for a DTMF digit
// ('0'-'9', 'A'-'D' case-insensitive, '*', '#'). The 80ms hold matches the
// RFC 4733 event duration default so both DTMF delivery methods use one
// consistent digit length.
func ToneDTMFDigit(digit rune) (Tone, error) {
	f, ok := dtmfToneFreqs[digit]
	if !ok {
		f, ok = dtmfToneFreqs[unicode.ToUpper(digit)]
	}
	if !ok {
		return Tone{}, fmt.Errorf("tone: %q is not a DTMF digit", digit)
	}
	return Tone{Segments: []ToneSegment{{Freqs: f[:], On: 80 * time.Millisecond}}}, nil
}
