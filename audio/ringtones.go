// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package audio

import (
	"fmt"
	"sync"
	"time"

	"github.com/sjlit/diago/media"
)

var (
	ringtones sync.Map
	beeps     sync.Map
)

// BeepLoadPCM loads pregenerated beep in PCM format.
// The returned slice is the shared cache backing array: callers must treat it
// as read-only and copy before any in-place transformation.
func BeepLoadPCM(codec media.Codec) ([]byte, error) {
	uuid := fmt.Sprintf("%s-%d", codec.Name, codec.SampleRate)
	ringval, exists := beeps.Load(uuid)
	if exists {
		return ringval.([]byte), nil
	}
	pcmBytes := beepPCMGenerate(int(codec.SampleRate))
	beeps.Store(uuid, pcmBytes)
	return pcmBytes, nil
}

// beepPCMGenerate renders the short 700Hz confirmation beep (0.5s).
func beepPCMGenerate(sampleRate int) []byte {
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{700}, On: 500 * time.Millisecond, Volume: 0.2}}}
	return RenderTonePCM(tone, sampleRate)
}

// RingtoneLoadPCM loads pregenerated ringtone in PCM format.
// The returned slice is the shared cache backing array: callers must treat it
// as read-only and copy before any in-place transformation.
func RingtoneLoadPCM(codec media.Codec) ([]byte, error) {
	uuid := fmt.Sprintf("%s-%d", codec.Name, codec.SampleRate)
	ringval, exists := ringtones.Load(uuid)
	if exists {
		return ringval.([]byte), nil
	}
	pcmBytes := ringtonePCMGenerate(int(codec.SampleRate))
	ringtones.Store(uuid, pcmBytes)
	return pcmBytes, nil
}

// ringtonePCMGenerate renders the 350+440Hz ringing tone (2s).
func ringtonePCMGenerate(sampleRate int) []byte {
	tone := Tone{Segments: []ToneSegment{{Freqs: []float64{350, 440}, On: 2 * time.Second, Volume: 0.3}}}
	return RenderTonePCM(tone, sampleRate)
}
