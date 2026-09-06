package media

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func testDTMFLoopSequence(r *RTPDtmfReader, sequence []DTMFEvent) string {
	detected := strings.Builder{}
	for i, ev := range sequence {
		fmt.Println("Processing", ev, i, i%7 == 0)
		r.processDTMFEvent(ev, i%7 == 0)
		dtmf, set := r.ReadDTMF()
		if set {
			detected.WriteRune(dtmf)
		}
	}
	return detected.String()
}

func TestDTMFReader(t *testing.T) {
	r := RTPDtmfReader{}

	// DTMF 109
	sequence := []DTMFEvent{
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 0, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 0, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 0, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 0, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 0, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 0, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 0, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 9, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 9, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 9, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 9, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 9, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 9, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 9, EndOfEvent: true, Volume: 10, Duration: 800},
	}

	dtmf := testDTMFLoopSequence(&r, sequence)
	assert.Equal(t, "109", dtmf)
}

func TestDTMFReaderRepeated(t *testing.T) {
	r := RTPDtmfReader{}

	// DTMF 109
	sequence := []DTMFEvent{
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
	}

	dtmf := testDTMFLoopSequence(&r, sequence)
	assert.Equal(t, "111", dtmf)
}

func TestDTMFReaderLatePacket(t *testing.T) {
	r := RTPDtmfReader{}

	// DTMF 109
	sequence := []DTMFEvent{
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 160},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 320},
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 480},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800}, // End event received before
		{Event: 1, EndOfEvent: false, Volume: 10, Duration: 640},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
	}

	dtmf := testDTMFLoopSequence(&r, sequence)
	assert.Equal(t, "1", dtmf)
}

func TestDTMFReaderCases(t *testing.T) {
	t.Skip("We need to check can this be test valid")
	r := RTPDtmfReader{}

	// DTMF 109
	sequence := []DTMFEvent{
		{Event: 1, EndOfEvent: false, Volume: 0, Duration: 0},
		{Event: 1, EndOfEvent: false, Volume: 0, Duration: 0},
		{Event: 1, EndOfEvent: false, Volume: 0, Duration: 0},
		{Event: 1, EndOfEvent: false, Volume: 0, Duration: 0},
		{Event: 1, EndOfEvent: true, Volume: 0, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 0, Duration: 800},
		{Event: 1, EndOfEvent: true, Volume: 0, Duration: 800},
	}

	dtmf := testDTMFLoopSequence(&r, sequence)
	assert.Equal(t, "1", dtmf)
}

func TestDTMFReaderSinglePacketEvent(t *testing.T) {
	r := RTPDtmfReader{}

	// RFC 4733 §3.6: a short event may fit one packet carrying both marker
	// and end bit. It must surface a digit, and its retransmissions (same
	// packet, no marker) must not duplicate it.
	r.processDTMFEvent(DTMFEvent{Event: 5, EndOfEvent: true, Volume: 10, Duration: 80}, true)
	dtmf, set := r.ReadDTMF()
	assert.True(t, set)
	assert.Equal(t, '5', dtmf)

	r.processDTMFEvent(DTMFEvent{Event: 5, EndOfEvent: true, Volume: 10, Duration: 80}, false)
	_, set = r.ReadDTMF()
	assert.False(t, set, "end retransmission must not re-emit the digit")

	// A next event still works after a single-packet one
	r.processDTMFEvent(DTMFEvent{Event: 9, EndOfEvent: true, Volume: 10, Duration: 80}, true)
	dtmf, set = r.ReadDTMF()
	assert.True(t, set)
	assert.Equal(t, '9', dtmf)
}

func TestDTMFReaderIgnoresLineEvents(t *testing.T) {
	r := RTPDtmfReader{}

	// RFC 4733 event codes above 15 are line events (ex. flash), not DTMF
	// keys. Both single-packet and multi-packet shapes must be ignored.
	r.processDTMFEvent(DTMFEvent{Event: 16, EndOfEvent: true, Volume: 10, Duration: 80}, true)
	_, set := r.ReadDTMF()
	assert.False(t, set)

	r.processDTMFEvent(DTMFEvent{Event: 16, EndOfEvent: false, Volume: 10, Duration: 160}, true)
	r.processDTMFEvent(DTMFEvent{Event: 16, EndOfEvent: true, Volume: 10, Duration: 320}, false)
	_, set = r.ReadDTMF()
	assert.False(t, set)
}
