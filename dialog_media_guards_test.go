// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDialogMedia returns a DialogMedia with a real (loopback) media
// session, RTP session and stable reader/writer handles, mimicking a dialog
// right after Answer. The caller is responsible for closing it.
func newTestDialogMedia(t *testing.T) *DialogMedia {
	t.Helper()

	sess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		sess.Close()
	})

	d := &DialogMedia{}
	rtpSess := media.NewRTPSession(sess)
	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	d.mu.Unlock()
	return d
}

func TestDialogMediaGuardsNotAnswered(t *testing.T) {
	// Zero-value media: no session ever created (dialog was never answered).
	d := &DialogMedia{}

	t.Run("AudioReader", func(t *testing.T) {
		_, err := d.AudioReader()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("AudioWriter", func(t *testing.T) {
		_, err := d.AudioWriter()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("Echo", func(t *testing.T) {
		assert.ErrorIs(t, d.Echo(), ErrDialogNotAnswered)
	})
	t.Run("ListenBackground", func(t *testing.T) {
		_, err := d.ListenBackground()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("ListenContext", func(t *testing.T) {
		assert.ErrorIs(t, d.ListenContext(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("ListenUntil", func(t *testing.T) {
		assert.ErrorIs(t, d.ListenUntil(0), ErrDialogNotAnswered)
	})
	t.Run("StopStartRTP", func(t *testing.T) {
		assert.ErrorIs(t, d.StopRTP(1, 0), ErrDialogNotAnswered)
		assert.ErrorIs(t, d.StartRTP(1, 0), ErrDialogNotAnswered)
	})
	t.Run("PlaybackCreate", func(t *testing.T) {
		_, err := d.PlaybackCreate()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("PlaybackControlCreate", func(t *testing.T) {
		_, err := d.PlaybackControlCreate()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("PlaybackDTMFCreate", func(t *testing.T) {
		_, err := d.PlaybackDTMFCreate()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("PlaybackRingtoneCreate", func(t *testing.T) {
		_, err := d.PlaybackRingtoneCreate()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("AudioReaderDTMF", func(t *testing.T) {
		_, err := d.AudioReaderDTMF()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("AudioWriterDTMF", func(t *testing.T) {
		_, err := d.AudioWriterDTMF()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("AudioStereoRecordingCreate", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
		require.NoError(t, err)
		defer f.Close()
		_, err = d.AudioStereoRecordingCreate(f)
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
}

func TestDialogMediaGuardsClosed(t *testing.T) {
	d := newTestDialogMedia(t)
	require.NoError(t, d.Close())

	t.Run("AudioReader", func(t *testing.T) {
		_, err := d.AudioReader()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("AudioWriter", func(t *testing.T) {
		_, err := d.AudioWriter()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("Echo", func(t *testing.T) {
		assert.ErrorIs(t, d.Echo(), ErrDialogClosed)
	})
	t.Run("ListenBackground", func(t *testing.T) {
		_, err := d.ListenBackground()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("ListenContext", func(t *testing.T) {
		assert.ErrorIs(t, d.ListenContext(context.Background()), ErrDialogClosed)
	})
	t.Run("ListenUntil", func(t *testing.T) {
		assert.ErrorIs(t, d.ListenUntil(0), ErrDialogClosed)
	})
	t.Run("PlaybackCreate", func(t *testing.T) {
		_, err := d.PlaybackCreate()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("PlaybackControlCreate", func(t *testing.T) {
		_, err := d.PlaybackControlCreate()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("PlaybackDTMFCreate", func(t *testing.T) {
		_, err := d.PlaybackDTMFCreate()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("PlaybackRingtoneCreate", func(t *testing.T) {
		_, err := d.PlaybackRingtoneCreate()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("AudioReaderDTMF", func(t *testing.T) {
		_, err := d.AudioReaderDTMF()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("AudioWriterDTMF", func(t *testing.T) {
		_, err := d.AudioWriterDTMF()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("AudioStereoRecordingCreate", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
		require.NoError(t, err)
		defer f.Close()
		_, err = d.AudioStereoRecordingCreate(f)
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("CloseIsIdempotent", func(t *testing.T) {
		assert.NoError(t, d.Close())
	})
}

func TestDialogClientGuardsNotAnswered(t *testing.T) {
	d := &DialogClientSession{DialogClientSession: &sipgo.DialogClientSession{}}

	t.Run("Hold", func(t *testing.T) {
		assert.ErrorIs(t, d.Hold(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("Unhold", func(t *testing.T) {
		assert.ErrorIs(t, d.Unhold(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("ReInvite", func(t *testing.T) {
		assert.ErrorIs(t, d.ReInvite(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("HangupWithoutResponse", func(t *testing.T) {
		err := d.Hangup(context.Background())
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
		assert.ErrorContains(t, err, "bye:")
	})
}

func TestDialogServerGuardsNotAnswered(t *testing.T) {
	d := &DialogServerSession{DialogServerSession: &sipgo.DialogServerSession{}}

	t.Run("Hold", func(t *testing.T) {
		assert.ErrorIs(t, d.Hold(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("Unhold", func(t *testing.T) {
		assert.ErrorIs(t, d.Unhold(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("ReInvite", func(t *testing.T) {
		assert.ErrorIs(t, d.ReInvite(context.Background()), ErrDialogNotAnswered)
	})
}

// TestDialogMediaGuardSetupWindow ensures the reader/writer nil window during
// setup (mediaSession set via initMediaSessionFromConf, handles not yet
// created) also surfaces as ErrDialogNotAnswered instead of a typed-nil panic.
func TestDialogMediaGuardSetupWindow(t *testing.T) {
	sess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	defer sess.Close()

	d := &DialogMedia{}
	require.NoError(t, d.initMediaSessionFromConf(MediaConfig{Codecs: sess.Codecs}))

	_, err = d.AudioReader()
	assert.ErrorIs(t, err, ErrDialogNotAnswered)
	_, err = d.AudioWriter()
	assert.ErrorIs(t, err, ErrDialogNotAnswered)
}
