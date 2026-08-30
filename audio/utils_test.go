// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package audio

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: odd-length input (short or partial RTP payload) used to panic on
// the last 2-byte sample. Mixing runs on media goroutines where a panic takes
// down the whole process
func TestPCMMixOddLength(t *testing.T) {
	mixed := make([]byte, 8)
	read := []byte{1, 0, 2, 0, 5} // 5 bytes, odd
	dst := make([]byte, 8)

	var n int
	require.NotPanics(t, func() {
		n = PCMMix(dst, mixed, read)
	})
	assert.Equal(t, 4, n) // Only whole 16-bit samples processed

	require.NotPanics(t, func() {
		PCMUnmix(dst, mixed, read)
	})
}

func TestPCMMixRoundTrip(t *testing.T) {
	frame := []byte{10, 0, 20, 0, 30, 0}
	zero := make([]byte, 6)
	mixed := make([]byte, 6)

	n := PCMMix(mixed, zero, frame)
	require.Equal(t, 6, n)
	assert.Equal(t, frame, mixed)

	unmixed := make([]byte, 6)
	PCMUnmix(unmixed, mixed, frame)
	assert.Equal(t, zero, unmixed)
}
