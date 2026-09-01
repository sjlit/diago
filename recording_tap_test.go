// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Agonovic

package diago

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tapTestDialog returns a DialogMedia that looks like a just-answered dialog
// with fake IO in place of live RTP, so the tap can be exercised without a
// network. probeR/probeW are the pre-existing pipeline heads; identity of
// these after Close tells us whether the tap uninstalled itself.
func tapTestDialog() (*DialogMedia, *bytes.Buffer, *bytes.Buffer) {
	fakePCM := bytes.Repeat([]byte("0123456789"), 32)
	alawFrame := make([]byte, 160)
	_, _ = audio.EncodeAlawTo(alawFrame, fakePCM)
	encoded := bytes.Repeat(alawFrame, 4)

	probeR := bytes.NewBuffer(encoded)
	probeW := &bytes.Buffer{}
	d := &DialogMedia{
		mediaSession: &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
		audioReader:  probeR,
		audioWriter:  probeW,
	}
	return d, probeR, probeW
}

func chainReader(d *DialogMedia) io.Reader {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.audioReader
}

func chainWriter(d *DialogMedia) io.Writer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.audioWriter
}

func TestStartStereoRecordingNotAnswered(t *testing.T) {
	d := &DialogMedia{}
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	_, err = d.StartStereoRecording(f)
	assert.ErrorIs(t, err, ErrDialogNotAnswered)
}

// TestStartStereoRecordingInstallsAndUninstalls pins the core contract: the
// tap is atomic-installed into both chains at Start (no deprecated setter, no
// half-wired window) and removed at Close when nothing wrapped on top.
func TestStartStereoRecordingInstallsAndUninstalls(t *testing.T) {
	d, probeR, probeW := tapTestDialog()
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	rec, err := d.StartStereoRecording(f)
	require.NoError(t, err)
	require.NotNil(t, rec)

	// Mid-call: heads are the tap, not the original probe buffers.
	assert.NotEqual(t, io.Reader(probeR), chainReader(d), "reader chain must route through the tap")
	assert.NotEqual(t, io.Writer(probeW), chainWriter(d), "writer chain must route through the tap")

	require.NoError(t, rec.Close())

	// After Close the tap is gone and the original heads are restored.
	assert.Equal(t, io.Reader(probeR), chainReader(d), "Close must uninstall the reader tap")
	assert.Equal(t, io.Writer(probeW), chainWriter(d), "Close must uninstall the writer tap")
}

// TestStartStereoRecordingKeepsTransparentTapWhenWrapped verifies the
// documented limitation: if a Bridge wrapped the chain on top of the tap,
// Close cannot surgically remove it, so it leaves a (stopped, transparent)
// tap in place rather than corrupting the outer chain.
func TestStartStereoRecordingKeepsTransparentTapWhenWrapped(t *testing.T) {
	d, _, _ := tapTestDialog()
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	rec, err := d.StartStereoRecording(f)
	require.NoError(t, err)

	// Simulate the bridge replacing the head with a wrapper on top.
	outer := &bytes.Buffer{} // any distinct reader stands in for the wrapper
	d.SetAudioReader(outer)

	require.NoError(t, rec.Close())
	assert.Equal(t, io.Reader(outer), chainReader(d), "Close must not remove a tap that is no longer outermost")
}

// TestStereoRecordingCloseIdempotent pins double-Close safety.
func TestStereoRecordingCloseIdempotent(t *testing.T) {
	d, _, _ := tapTestDialog()
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	rec, err := d.StartStereoRecording(f)
	require.NoError(t, err)
	require.NoError(t, rec.Close())
	require.NoError(t, rec.Close(), "Close must be idempotent")
}

// TestStereoRecordingPauseResumeAfterClose checks the lifecycle guards.
func TestStereoRecordingPauseResumeAfterClose(t *testing.T) {
	d, _, _ := tapTestDialog()
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	rec, err := d.StartStereoRecording(f)
	require.NoError(t, err)
	require.NoError(t, rec.Close())

	assert.ErrorIs(t, rec.Pause(), ErrRecordingClosed)
	assert.ErrorIs(t, rec.Resume(), ErrRecordingClosed)
}

// TestStereoRecordingWritesWav runs audio through both directions of the tap
// and asserts the finalized WAV is a valid 2-channel file with data on both
// channels - the behavior orbit's recorder depends on.
func TestStereoRecordingWritesWav(t *testing.T) {
	fakePCM := bytes.Repeat([]byte("0123456789"), 32)
	alawFrame := make([]byte, 160)
	_, err := audio.EncodeAlawTo(alawFrame, fakePCM)
	require.NoError(t, err)
	encoded := bytes.Repeat(alawFrame, 4)

	d := &DialogMedia{
		mediaSession: &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
		audioReader:  bytes.NewBuffer(encoded),
		audioWriter:  bytes.NewBuffer([]byte{}),
	}

	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	rec, err := d.StartStereoRecording(f)
	require.NoError(t, err)

	// Pull the inbound (reader) direction and push the outbound (writer) one
	// through the public API - these resolve to the installed tap.
	ar, err := d.AudioReader()
	require.NoError(t, err)
	aw, err := d.AudioWriter()
	require.NoError(t, err)

	_, err = media.ReadAll(ar, 160)
	require.NoError(t, err)
	_, err = media.WriteAll(aw, encoded, 160)
	require.NoError(t, err)

	require.NoError(t, rec.Close())

	f.Seek(0, 0)
	wav := audio.NewWavReader(f)
	require.NoError(t, wav.ReadHeaders())
	// 4 reader frames + 4 writer frames interleaved = 2*4*320 bytes. A single
	// channel would only be 4*320, so the doubling proves both directions
	// landed in the stereo WAV.
	assert.Equal(t, 2*4*320, wav.DataSize)
}

// TestStartStereoRecordingFailOpenDefault pins that the new API is fail-open
// by default and that the option flips it off.
func TestStartStereoRecordingFailOpenDefault(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
	require.NoError(t, err)
	defer f.Close()

	d, _, _ := tapTestDialog()
	rec, err := d.StartStereoRecording(f)
	require.NoError(t, err)
	assert.True(t, rec.mon.MonitorPCMReader.FailOpen, "new API defaults to fail-open")
	assert.NoError(t, rec.Close())

	d2, _, _ := tapTestDialog()
	rec2, err := d2.StartStereoRecording(f, WithRecordingFailOpen(false))
	require.NoError(t, err)
	assert.False(t, rec2.mon.MonitorPCMReader.FailOpen)
	assert.False(t, rec2.mon.MonitorPCMWriter.FailOpen)
	assert.NoError(t, rec2.Close())
}
