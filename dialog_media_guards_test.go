// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogLoggerPropagation verifies that WithLogger reaches the media
// config (and therefore every media session) unless MediaConfig.Logger
// explicitly overrides it.
func TestDialogLoggerPropagation(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dg, err := NewDiago(ua, WithDefaultTransport(), WithLogger(logger))
	require.NoError(t, err)
	require.Same(t, logger, dg.mediaConf.Logger)

	tran, ok := dg.getTransport("udp")
	require.True(t, ok)
	require.Same(t, logger, dg.mediaConfigForTransport(tran).Logger)

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	dg2, err := NewDiago(ua, WithDefaultTransport(), WithLogger(logger), WithMediaConfig(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Logger: custom,
	}))
	require.NoError(t, err)
	require.Same(t, custom, dg2.mediaConf.Logger, "an explicit MediaConfig.Logger wins")
}

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
	err = d.initRTPSessionUnsafe(sess, rtpSess)
	d.mu.Unlock()
	require.NoError(t, err)
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
	t.Run("EchoContext", func(t *testing.T) {
		assert.ErrorIs(t, d.EchoContext(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("ListenBackground", func(t *testing.T) {
		_, err := d.ListenBackground()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("ListenContext", func(t *testing.T) {
		assert.ErrorIs(t, d.ListenContext(context.Background()), ErrDialogNotAnswered)
	})
	t.Run("CreatePlayback", func(t *testing.T) {
		_, err := d.CreatePlayback()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("CreatePlaybackControl", func(t *testing.T) {
		_, err := d.CreatePlaybackControl()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("CreatePlaybackDTMF", func(t *testing.T) {
		_, err := d.CreatePlaybackDTMF()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("CreateRingtonePlayback", func(t *testing.T) {
		_, err := d.CreateRingtonePlayback()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("CreateDTMFReader", func(t *testing.T) {
		_, err := d.CreateDTMFReader()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("CreateDTMFWriter", func(t *testing.T) {
		_, err := d.CreateDTMFWriter()
		assert.ErrorIs(t, err, ErrDialogNotAnswered)
	})
	t.Run("DiscardQueuedRTP", func(t *testing.T) {
		_, err := d.DiscardQueuedRTP()
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
	t.Run("EchoContext", func(t *testing.T) {
		assert.ErrorIs(t, d.EchoContext(context.Background()), ErrDialogClosed)
	})
	t.Run("ListenBackground", func(t *testing.T) {
		_, err := d.ListenBackground()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("ListenContext", func(t *testing.T) {
		assert.ErrorIs(t, d.ListenContext(context.Background()), ErrDialogClosed)
	})
	t.Run("CreatePlayback", func(t *testing.T) {
		_, err := d.CreatePlayback()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("CreatePlaybackControl", func(t *testing.T) {
		_, err := d.CreatePlaybackControl()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("CreatePlaybackDTMF", func(t *testing.T) {
		_, err := d.CreatePlaybackDTMF()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("CreateRingtonePlayback", func(t *testing.T) {
		_, err := d.CreateRingtonePlayback()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("CreateDTMFReader", func(t *testing.T) {
		_, err := d.CreateDTMFReader()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("CreateDTMFWriter", func(t *testing.T) {
		_, err := d.CreateDTMFWriter()
		assert.ErrorIs(t, err, ErrDialogClosed)
	})
	t.Run("DiscardQueuedRTP", func(t *testing.T) {
		_, err := d.DiscardQueuedRTP()
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

// TestDialogMediaPlaybackTimestampResetHook verifies that playback factories
// arm the stable writer handle's ResetTimestamp hook, including the control
// variant (replay must advance the RTP timeline like a fresh play).
func TestDialogMediaPlaybackTimestampResetHook(t *testing.T) {
	d := newTestDialogMedia(t)

	pb, err := d.CreatePlayback()
	require.NoError(t, err)
	require.NotNil(t, pb.onPlay)

	pbc, err := d.CreatePlaybackControl()
	require.NoError(t, err)
	require.NotNil(t, pbc.AudioPlayback.onPlay)
}

// TestDialogMediaPlaybackWithoutRTPWriterHandle locks the custom-IO pipeline
// behavior: without a stable RTP writer handle there is no timestamp reset to
// arm, and the playback factories must not panic on the nil handle.
func TestDialogMediaPlaybackWithoutRTPWriterHandle(t *testing.T) {
	sess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	defer sess.Close()

	d := &DialogMedia{}
	d.mu.Lock()
	d.mediaSession = sess
	d.audioWriter = io.Discard
	d.mu.Unlock()

	pb, err := d.CreatePlayback()
	require.NoError(t, err)
	require.Nil(t, pb.onPlay)

	pbc, err := d.CreatePlaybackControl()
	require.NoError(t, err)
	require.Nil(t, pbc.AudioPlayback.onPlay)
}

// TestDialogMediaClosedRejectsInstall locks the teardown contract: once the
// dialog media is closed, setup paths must refuse to create/install media
// again instead of leaving orphaned sockets and monitors behind.
func TestDialogMediaClosedRejectsInstall(t *testing.T) {
	d := &DialogMedia{}
	require.NoError(t, d.Close())

	// No listeners may be created after close.
	err := d.initMediaSessionFromConf(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}})
	assert.ErrorIs(t, err, ErrDialogClosed)
	assert.Nil(t, d.mediaSession)

	// No RTP session/handles may be installed after close.
	sess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	defer sess.Close()
	rtpSess := media.NewRTPSession(sess)
	defer rtpSess.Close()
	err = d.initRTPSessionUnsafe(sess, rtpSess)
	assert.ErrorIs(t, err, ErrDialogClosed)
	assert.Nil(t, d.rtpSession)
	assert.Nil(t, d.rtpPacketReader)
	assert.Nil(t, d.rtpPacketWriter)

	// A late media update must not replace sessions after close either.
	d2 := newTestDialogMedia(t)
	require.NoError(t, d2.Close())
	replacement, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	defer replacement.Close()
	d2.mu.Lock()
	err = d2.replaceRTPSessionUnsafe(replacement)
	d2.mu.Unlock()
	assert.ErrorIs(t, err, ErrDialogClosed)
}
