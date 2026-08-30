// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: a blank line (single LF) used to index below zero in nextLine and
// panic the parser on remote supplied SDP
func TestUnmarshalBlankLine(t *testing.T) {
	sd := SessionDescription{}
	require.NotPanics(t, func() {
		err := Unmarshal([]byte("v=0\n\nm=audio 1234 RTP/AVP 0\n\n"), &sd)
		require.NoError(t, err)
	})
	assert.Equal(t, []string{"0"}, sd["v"])
	assert.Equal(t, []string{"audio 1234 RTP/AVP 0"}, sd["m"])
}

// Regression: last line without newline terminator used to be silently dropped
func TestUnmarshalLastLineWithoutCRLF(t *testing.T) {
	sd := SessionDescription{}
	err := Unmarshal([]byte("v=0\r\nm=audio 1234 RTP/AVP 0\r\na=sendrecv"), &sd)
	require.NoError(t, err)
	assert.Equal(t, []string{"sendrecv"}, sd["a"])

	sd = SessionDescription{}
	err = Unmarshal([]byte("v=0\nm=audio 1234 RTP/AVP 0\na=sendrecv"), &sd)
	require.NoError(t, err)
	assert.Equal(t, []string{"sendrecv"}, sd["a"])
}
