// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package audio

import (
	"testing"

	"github.com/sjlit/diago/media"
)

func TestRingtoneBeepPCM(t *testing.T) {
	codec := media.CodecAudioUlaw
	ring, err := RingtoneLoadPCM(codec)
	if err != nil {
		t.Fatal(err)
	}
	// 2s * 8000 * 2 bytes
	if len(ring) != 2*8000*2 {
		t.Fatalf("ringtone len=%d want %d", len(ring), 2*8000*2)
	}
	samples := pcmToSamples(ring)
	for _, f := range []float64{350, 440} {
		if toneEnergy(samples, f, 8000) < 1000 {
			t.Fatalf("ringtone missing frequency %v", f)
		}
	}

	beep, err := BeepLoadPCM(codec)
	if err != nil {
		t.Fatal(err)
	}
	// 0.5s * 8000 * 2 bytes
	if len(beep) != 8000 {
		t.Fatalf("beep len=%d want %d", len(beep), 8000)
	}
	if toneEnergy(pcmToSamples(beep), 700, 8000) < 1000 {
		t.Fatal("beep missing 700Hz")
	}

	// second call hits the cache and returns identical bytes
	ring2, err := RingtoneLoadPCM(codec)
	if err != nil {
		t.Fatal(err)
	}
	if string(ring) != string(ring2) {
		t.Fatal("cache must return identical bytes")
	}
}
