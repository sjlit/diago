// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaCloseClearsDTMFReaders(t *testing.T) {
	dm := &Media{
		mediaSession:    &media.MediaSession{},
		rtpPacketReader: &media.RTPPacketReader{},
	}

	r, err := dm.NewDTMFReader()
	require.NoError(t, err)

	var calls atomic.Int32
	r.OnDTMF(func(rune) error {
		calls.Add(1)
		return nil
	})

	dm.deliverDTMF('1', 0)
	require.EqualValues(t, 1, calls.Load())

	require.NoError(t, dm.Close())

	dm.mu.Lock()
	assert.Nil(t, dm.dtmfReaders)
	dm.mu.Unlock()
}

func TestDeliverDTMFAfterCloseNoCallback(t *testing.T) {
	dm := &Media{
		mediaSession:    &media.MediaSession{},
		rtpPacketReader: &media.RTPPacketReader{},
	}

	r, err := dm.NewDTMFReader()
	require.NoError(t, err)

	var calls atomic.Int32
	r.OnDTMF(func(rune) error {
		calls.Add(1)
		return nil
	})

	require.NoError(t, dm.Close())

	dm.deliverDTMF('2', 0)
	assert.EqualValues(t, 0, calls.Load(), "DTMF must not be delivered after Close")
}

func TestRingtonePlayBackgroundStopsOnDialogClose(t *testing.T) {
	// The downstream writer failing (as it does when the dialog media closes)
	// must make the background ringtone loop return on its own; there is no
	// onClose hook for it by design (avoiding lock-order cycles).
	pr, pw := io.Pipe()
	defer pw.Close()

	enc := &audio.PCMEncoderWriter{}
	require.NoError(t, enc.Init(media.CodecAudioUlaw, pw))

	dm := &Media{
		mediaSession:    &media.MediaSession{},
		rtpPacketWriter: &media.RTPPacketWriter{},
	}
	ar := &AudioRingtone{
		writer:     enc,
		ringtone:   make([]byte, 160),
		sampleSize: 160,
		dm:         dm,
	}

	stop, err := ar.PlayBackground()
	require.NoError(t, err)

	// Simulate dialog close from the writer's point of view.
	_ = pr.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stop()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ringtone background loop did not stop after writer close")
	}
}
