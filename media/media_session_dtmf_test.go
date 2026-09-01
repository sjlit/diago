// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"testing"
	"time"
)

func TestDTMFCodecNegotiated(t *testing.T) {
	// offered list contains telephone-event -> found with its PT
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw, CodecTelephoneEvent8000},
	}
	cod, ok := sess.DTMFCodecNegotiated()
	if !ok || cod.PayloadType != 101 {
		t.Fatalf("expected tel-event PT101, got %+v ok=%v", cod, ok)
	}

	// no telephone-event offered -> false (unlike DTMFCodec's silent fallback)
	sess2 := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
	if _, ok := sess2.DTMFCodecNegotiated(); ok {
		t.Fatal("must report no dtmf codec")
	}
	if got := sess2.DTMFCodec(); got != CodecTelephoneEvent8000 {
		t.Fatal("legacy DTMFCodec fallback must not change")
	}

	// negotiated list (filterCodecs) takes precedence over the offered one —
	// the remote used a different payload type. filterCodecs is populated by
	// RemoteSDP in production; set it directly here to keep the test
	// independent of SDP negotiation internals.
	sess3 := &MediaSession{Codecs: []Codec{CodecAudioUlaw, CodecTelephoneEvent8000}}
	sess3.filterCodecs = []Codec{
		CodecAudioUlaw,
		{PayloadType: 110, SampleRate: 8000, SampleDur: 20 * time.Millisecond, NumChannels: 1, Name: "telephone-event"},
	}
	cod3, ok := sess3.DTMFCodecNegotiated()
	if !ok {
		t.Fatal("negotiated PT110 telephone-event must be found")
	}
	if cod3.PayloadType != 110 {
		t.Fatalf("expected negotiated PT 110, got %d", cod3.PayloadType)
	}
}
