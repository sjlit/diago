// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

type dtmfRTPSink struct {
	mu       sync.Mutex
	headers  []rtp.Header
	payloads [][]byte
}

func (s *dtmfRTPSink) WriteRTP(p *rtp.Packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	cp.Payload = append([]byte(nil), p.Payload...)
	s.headers = append(s.headers, p.Header)
	s.payloads = append(s.payloads, cp.Payload)
	return nil
}

func (s *dtmfRTPSink) snapshot() ([]rtp.Header, [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs := append([]rtp.Header(nil), s.headers...)
	ps := append([][]byte(nil), s.payloads...)
	return hs, ps
}

func TestRTPDtmfWriterDefaults8k(t *testing.T) {
	sink := &dtmfRTPSink{}
	pw := NewRTPPacketWriter(sink, CodecAudioUlaw)
	w := NewRTPDTMFWriter(CodecTelephoneEvent8000, pw, nil)

	if err := w.WriteDTMF('7'); err != nil {
		t.Fatal(err)
	}
	headers, payloads := sink.snapshot()
	if len(payloads) != 7 {
		t.Fatalf("expected 7 packets, got %d", len(payloads))
	}
	var ev DTMFEvent
	DTMFDecode(payloads[0], &ev)
	if ev.Event != 7 || ev.EndOfEvent || ev.Volume != 10 || ev.Duration != 160 {
		t.Fatalf("first packet: %+v", ev)
	}
	DTMFDecode(payloads[6], &ev)
	if !ev.EndOfEvent || ev.Duration != 640 {
		t.Fatalf("last packet: %+v", ev)
	}
	for i, h := range headers {
		if h.PayloadType != 101 {
			t.Fatalf("packet %d PT=%d want 101", i, h.PayloadType)
		}
		if (h.Marker == true) != (i == 0) {
			t.Fatalf("packet %d marker=%v", i, h.Marker)
		}
		if h.Timestamp != headers[0].Timestamp {
			t.Fatalf("DTMF packets must share timestamp (no audio clock advance), packet %d", i)
		}
	}
}

func TestRTPDtmfWriter48kWithOptions(t *testing.T) {
	sink := &dtmfRTPSink{}
	pw := NewRTPPacketWriter(sink, CodecAudioUlaw)
	tel48 := Codec{PayloadType: 110, SampleRate: 48000, SampleDur: 20 * time.Millisecond, NumChannels: 1, Name: "telephone-event"}
	w := NewRTPDTMFWriter(tel48, pw, nil)

	err := w.WriteDTMFWithOptions('B', DTMFEncodeOptions{Volume: 15, VolumeSet: true, EventDuration: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	headers, payloads := sink.snapshot()
	if len(payloads) != 7 {
		t.Fatalf("expected 7 packets, got %d", len(payloads))
	}
	var ev DTMFEvent
	DTMFDecode(payloads[0], &ev)
	// 60ms @48k = 2880 ticks final; step 720
	if ev.Event != 13 || ev.Volume != 15 || ev.Duration != 720 {
		t.Fatalf("first packet: %+v", ev)
	}
	if h := headers[0]; h.PayloadType != 110 {
		t.Fatalf("PT=%d want negotiated 110", h.PayloadType)
	}
}

func TestRTPDtmfWriterZeroSampleDur(t *testing.T) {
	// A hand-built codec without SampleDur must not panic the pacing ticker;
	// it falls back to the conventional 20ms packet interval.
	sink := &dtmfRTPSink{}
	pw := NewRTPPacketWriter(sink, CodecAudioUlaw)
	bare := Codec{PayloadType: 101, SampleRate: 8000, NumChannels: 1, Name: "telephone-event"}
	w := NewRTPDTMFWriter(bare, pw, nil)

	if err := w.WriteDTMF('5'); err != nil {
		t.Fatal(err)
	}
	if _, payloads := sink.snapshot(); len(payloads) != 7 {
		t.Fatalf("expected 7 packets with fallback pacing, got %d", len(payloads))
	}
}
