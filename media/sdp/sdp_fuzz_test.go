// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package sdp

import (
	"strings"
	"testing"
)

// FuzzUnmarshal checks that the parser and the section-aware connection
// lookup survive arbitrary input: they may error, but must not panic or hang.
// The seed corpus doubles as a regression suite for the multi m= / media-level
// c= handling in ConnectionInformationFor.
func FuzzUnmarshal(f *testing.F) {
	seeds := []string{
		// Minimal valid
		"v=0\r\no=- 1 1 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\n",
		// Multi m= with media-level connection lines
		"v=0\r\no=- 1 1 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
			"m=video 5006 RTP/AVP 96\r\nc=IN IP4 192.168.100.21\r\n" +
			"m=audio 5004 RTP/AVP 0\r\nc=IN IP4 192.168.100.22\r\n",
		// No session-level connection
		"v=0\r\no=- 1 1 IN IP4 10.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\nc=IN IP4 192.168.100.22\r\n",
		// Media c= without a matching m= index
		"v=0\r\no=- 1 1 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\nm=video 5006 RTP/AVP 96\r\n",
		// Truncated and malformed lines
		"v=0\r\no=-\r\nm=audio\r\nc=IN",
		"c=IN IP4 1.2.3.4",
		"m=audio 5004",
		strings.Repeat("m=audio 5004 RTP/AVP 0\r\n", 20),
		strings.Repeat("c=IN IP4 10.0.0.1\r\n", 20),
		"\x00\xff\xfe garbage \r\n lines",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		sd := SessionDescription{}
		if err := Unmarshal(data, &sd); err != nil {
			return
		}
		// Parsed successfully: the lookups must hold up on whatever was
		// assembled (including the internal c_sections tracking).
		_, _ = sd.MediaDescription("audio")
		_, _ = sd.MediaDescription(string([]byte{0x00}))
		_, _ = sd.ConnectionInformationFor("audio")
		_, _ = sd.ConnectionInformation()
	})
}
