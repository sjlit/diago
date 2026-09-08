// SPDX-License-Identifier: MPL-2.0
package diago

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStereoRecording_CleanedByDialogClose(t *testing.T) {
	d, _, _ := tapTestDialog()
	spool := t.TempDir()
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	_, err = d.StartStereoRecording(f, WithRecordingSpoolDir(spool))
	require.NoError(t, err)

	// Forget rec.Close: BYE path must still reclaim spool files.
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, d.Close(), "DialogMedia.Close idempotent")
}
