// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"encoding/binary"
	"fmt"
	"time"
)

// DTMF event mapping (RFC 4733)
var dtmfEventMapping = map[rune]byte{
	'0': 0,
	'1': 1,
	'2': 2,
	'3': 3,
	'4': 4,
	'5': 5,
	'6': 6,
	'7': 7,
	'8': 8,
	'9': 9,
	'*': 10,
	'#': 11,
	'A': 12,
	'B': 13,
	'C': 14,
	'D': 15,
}

var dtmfEventMappingRev = map[byte]rune{
	0:  '0',
	1:  '1',
	2:  '2',
	3:  '3',
	4:  '4',
	5:  '5',
	6:  '6',
	7:  '7',
	8:  '8',
	9:  '9',
	10: '*',
	11: '#',
	12: 'A',
	13: 'B',
	14: 'C',
	15: 'D',
}

func DTMFToRune(dtmf uint8) rune {
	return dtmfEventMappingRev[dtmf]
}

// IsDTMFEvent reports whether char has an RFC 4733 event code
// ('0'-'9', 'A'-'D', '*', '#').
func IsDTMFEvent(char rune) bool {
	_, ok := dtmfEventMapping[char]
	return ok
}

// DefaultDTMFVolume is the signal volume applied when DTMFEncodeOptions
// marks no explicit choice.
const DefaultDTMFVolume uint8 = 10

// DTMFEncodeOptions tunes the event packet series generated for one digit.
type DTMFEncodeOptions struct {
	// Volume is the RFC 4733 signal volume (0-63 relative dBov, 0 being the
	// loudest). It is only honored when VolumeSet is true — the zero value of
	// the struct must keep the legacy default (DefaultDTMFVolume), and volume
	// 0 is itself a legal wire value, so it cannot double as "unset".
	Volume uint8
	// VolumeSet marks Volume as explicitly provided.
	VolumeSet bool
	// EventDuration is the event hold duration reported in the Duration
	// fields. Zero means the default, 80ms.
	EventDuration time.Duration
}

// RTPDTMFEncode creates the series of redundant event packets for one digit,
// scaled to the codec clock rate (160 ticks @8k, 960 @48k per 20ms).
// A zero/negative codec clock falls back to the canonical 8 kHz
// telephone-event rate (mirroring the pacing guard in WriteDTMFWithOptions).
// Layout: 4 active packets with growing Duration, then 3 EndOfEvent packets
// repeating the final duration.
func RTPDTMFEncode(codec Codec, char rune, opts DTMFEncodeOptions) ([]DTMFEvent, error) {
	event, ok := dtmfEventMapping[char]
	if !ok {
		return nil, fmt.Errorf("rtp dtmf: invalid event %q", char)
	}
	vol := DefaultDTMFVolume
	if opts.VolumeSet {
		vol = opts.Volume
	}
	if vol > 63 {
		vol = 63
	}
	clock := codec.SampleRate
	if clock <= 0 {
		clock = CodecTelephoneEvent8000.SampleRate
	}
	stepTicks := uint16(clock / 50) // 20ms of clock ticks
	if opts.EventDuration > 0 {
		ticks := uint32(opts.EventDuration.Seconds() * float64(clock) / 4)
		if ticks == 0 || ticks > 65535/4 {
			return nil, fmt.Errorf("rtp dtmf: event duration %v out of range for %d Hz clock", opts.EventDuration, clock)
		}
		stepTicks = uint16(ticks)
	}

	events := make([]DTMFEvent, 7)
	for i := 0; i < 4; i++ {
		events[i] = DTMFEvent{
			Event:      event,
			EndOfEvent: false,
			Volume:     vol,
			Duration:   stepTicks * uint16(i+1),
		}
	}
	// End events with redundancy: duration must not grow past the event hold
	for i := 4; i < 7; i++ {
		events[i] = DTMFEvent{
			Event:      event,
			EndOfEvent: true,
			Volume:     vol,
			Duration:   stepTicks * 4,
		}
	}
	return events, nil
}

// RTPDTMFEncode8000 creates series of DTMF redudant events which should be encoded as payload
// It is currently only 8000 sample rate considered for telophone event
//
// Deprecated: Use RTPDTMFEncode with the negotiated codec.
func RTPDTMFEncode8000(char rune) []DTMFEvent {
	evs, err := RTPDTMFEncode(CodecTelephoneEvent8000, char, DTMFEncodeOptions{})
	if err != nil {
		// legacy behavior: unknown characters encoded as event 0
		evs, _ = RTPDTMFEncode(CodecTelephoneEvent8000, '0', DTMFEncodeOptions{})
	}
	return evs
}

// DTMFEvent represents a DTMF event
type DTMFEvent struct {
	Event      uint8
	EndOfEvent bool
	Volume     uint8
	Duration   uint16
}

func (ev *DTMFEvent) String() string {
	out := "RTP DTMF Event:\n"
	out += fmt.Sprintf("\tEvent: %d\n", ev.Event)
	out += fmt.Sprintf("\tEndOfEvent: %v\n", ev.EndOfEvent)
	out += fmt.Sprintf("\tVolume: %d\n", ev.Volume)
	out += fmt.Sprintf("\tDuration: %d\n", ev.Duration)
	return out
}

// DecodeRTPPayload decodes an RTP payload into a DTMF event
func DTMFDecode(payload []byte, d *DTMFEvent) error {
	if len(payload) < 4 {
		return fmt.Errorf("payload too short")
	}

	d.Event = payload[0]
	d.EndOfEvent = payload[1]&0x80 != 0
	d.Volume = payload[1] & 0x7F
	d.Duration = binary.BigEndian.Uint16(payload[2:4])
	// d.Duration = uint16(payload[2])<<8 | uint16(payload[3])
	return nil
}

func DTMFEncode(d DTMFEvent) []byte {
	header := make([]byte, 4)
	header[0] = d.Event

	if d.EndOfEvent {
		header[1] = 0x80
	}
	header[1] |= d.Volume & 0x3F
	binary.BigEndian.PutUint16(header[2:4], d.Duration)
	return header
}
