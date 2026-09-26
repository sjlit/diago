package media

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression: Read divides the RTP timestamp delta by SampleTimestamp(); a
// degenerate codec with SampleDur set but SampleRate zero passed Init and
// panicked with an integer division by zero on the second packet.
func TestNewRTPRealTimeReaderRejectsDegenerateCodec(t *testing.T) {
	_, err := NewRTPRealTimeReader(strings.NewReader(""), nil, Codec{
		Name:      "bad",
		SampleDur: 20 * time.Millisecond,
	})
	require.Error(t, err)

	_, err = NewRTPRealTimeReader(strings.NewReader(""), nil, Codec{})
	require.Error(t, err)

	_, err = NewRTPRealTimeReader(strings.NewReader(""), nil, CodecAudioUlaw)
	require.NoError(t, err)
}
