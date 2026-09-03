// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
)

func mohTone() audio.Tone {
	return audio.Tone{Segments: []audio.ToneSegment{{Freqs: []float64{425}, On: 40 * time.Millisecond}}}
}

func TestMusicOnHoldLoopsUntilStop(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())

	moh, err := m.PlayMusicOnHold(context.Background(), WithMoHTone(mohTone()))
	require.NoError(t, err)

	// 40ms cadence of 20ms ulaw packets: several packets must arrive and keep
	// arriving (loop), riding the negotiated audio payload type.
	require.Eventually(t, func() bool { return sink.count() >= 3 }, 2*time.Second, 10*time.Millisecond)
	for _, p := range sink.packets {
		if p.PayloadType != media.CodecAudioUlaw.PayloadType {
			t.Fatalf("hold music must ride PT %d, got %d", media.CodecAudioUlaw.PayloadType, p.PayloadType)
		}
	}

	require.NoError(t, moh.Stop())
	select {
	case <-moh.Done():
	default:
		t.Fatal("Done must be closed after Stop")
	}

	// No growth after stop (one in-flight frame tolerated).
	n := sink.count()
	time.Sleep(100 * time.Millisecond)
	assert.LessOrEqual(t, sink.count(), n+1, "packets must stop after Stop")
}

func TestMusicOnHoldDoubleStart(t *testing.T) {
	m, _ := newFakeDialogMedia(t, ulawCodecs())
	ctx := context.Background()

	first, err := m.PlayMusicOnHold(ctx, WithMoHTone(mohTone()))
	require.NoError(t, err)

	_, err = m.PlayMusicOnHold(ctx, WithMoHTone(mohTone()))
	require.ErrorIs(t, err, ErrMusicOnHoldActive)

	require.NoError(t, first.Stop())

	second, err := m.PlayMusicOnHold(ctx, WithMoHTone(mohTone()))
	require.NoError(t, err, "restart must be possible after Stop")
	require.NoError(t, second.Stop())
	require.NoError(t, second.Stop(), "Stop must be idempotent")
}

func TestMusicOnHoldNoToneConfigured(t *testing.T) {
	m, _ := newFakeDialogMedia(t, ulawCodecs())
	ctx := context.Background()

	_, err := m.PlayMusicOnHold(ctx)
	require.ErrorIs(t, err, ErrMusicOnHoldNoTone)

	// Dialog-level default (what MediaConfig.MusicOnHold installs) applies
	// when no option overrides it.
	m.mohTone = mohTone()
	moh, err := m.PlayMusicOnHold(ctx)
	require.NoError(t, err)
	require.NoError(t, moh.Stop())
}

func TestMusicOnHoldCtxCancel(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())

	ctx, cancel := context.WithCancel(context.Background())
	moh, err := m.PlayMusicOnHold(ctx, WithMoHTone(mohTone()))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return sink.count() >= 2 }, 2*time.Second, 10*time.Millisecond)
	cancel()

	select {
	case <-moh.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("loop must exit on ctx cancel")
	}
	require.NoError(t, moh.Stop(), "ctx-canceled stop is a clean stop")
}

func TestMusicOnHoldCloseStopsLoop(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())

	moh, err := m.PlayMusicOnHold(context.Background(), WithMoHTone(mohTone()))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return sink.count() >= 2 }, 2*time.Second, 10*time.Millisecond)

	// Close cancels the loop without waiting on it (lock-order contract):
	// from the caller's view the loop must simply be gone afterwards.
	require.NoError(t, m.Close())

	select {
	case <-moh.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("loop must exit after Close")
	}

	_, err = m.PlayMusicOnHold(context.Background(), WithMoHTone(mohTone()))
	require.ErrorIs(t, err, ErrDialogClosed)
}

func TestMusicOnHoldWaitsWritePause(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	release, err := m.PauseAudioWrite()
	require.NoError(t, err)

	// Starting while the write gate is held succeeds; no frame may pass the
	// gate until it is released (same cooperation as PlayTone).
	moh, err := m.PlayMusicOnHold(context.Background(), WithMoHTone(mohTone()))
	require.NoError(t, err)

	time.Sleep(80 * time.Millisecond)
	if n := sink.count(); n != 0 {
		t.Fatalf("paused gate must block hold music, got %d packets", n)
	}

	release()
	require.Eventually(t, func() bool { return sink.count() >= 2 }, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, moh.Stop())
}

// TestRemoteHeldTracksInboundSDP locks the direction semantics remoteHeld
// relies on: inbound recvonly/inactive SDP (the peer holding us) sets the
// flag; our own Hold path (sendonly fork negotiating against a recvonly
// answer) never trips it.
func TestRemoteHeldTracksInboundSDP(t *testing.T) {
	d := newTestDialogMedia(t)
	ip := net.ParseIP("127.0.0.1")
	require.False(t, d.IsRemoteHeld())

	// Peer offers sendonly: we are held.
	offer := sdp.GenerateForAudio(ip, ip, 41001, sdp.ModeSendonly, []string{sdp.FORMAT_TYPE_ULAW}, "")
	d.mu.Lock()
	err := d.sdpUpdateUnsafe(offer)
	d.mu.Unlock()
	require.NoError(t, err)
	assert.True(t, d.IsRemoteHeld(), "recvonly negotiated direction means the remote holds us")
	assert.Equal(t, sdp.ModeRecvonly, d.currentMediaSession().NegotiatedDirection())

	// Peer offers sendrecv again: unheld.
	offer = sdp.GenerateForAudio(ip, ip, 41001, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW}, "")
	d.mu.Lock()
	err = d.sdpUpdateUnsafe(offer)
	d.mu.Unlock()
	require.NoError(t, err)
	assert.False(t, d.IsRemoteHeld())

	// Our own Hold: fork with sendonly preference, remote answers recvonly
	// (the standard hold answer) — negotiated sendonly, flag untouched.
	msess := d.currentMediaSession().Fork()
	msess.Mode = sdp.ModeSendonly
	answer := sdp.GenerateForAudio(ip, ip, 41001, sdp.ModeRecvonly, []string{sdp.FORMAT_TYPE_ULAW}, "")
	require.NoError(t, msess.RemoteSDP(answer))
	d.mu.Lock()
	err = d.mediaUpdateUnsafe(msess)
	d.mu.Unlock()
	require.NoError(t, err)
	assert.False(t, d.IsRemoteHeld(), "our own Hold must not mark the dialog remote-held")
	assert.Equal(t, sdp.ModeSendonly, d.currentMediaSession().NegotiatedDirection())
}

// TestHoldAutoStartStopsOnUnhold exercises the Hold/Unhold wiring without
// signaling: the auto handle is started on Hold and stopped by Unhold, while
// a manually started loop survives Unhold.
func TestHoldAutoStartStopsOnUnhold(t *testing.T) {
	m, sink := newFakeDialogMedia(t, ulawCodecs())
	tone := mohTone()

	// What Hold does after its re-INVITE succeeds (auto=true), with the
	// dialog-level default tone configured.
	m.mohTone = tone
	m.mohAutoStart(context.Background(), nil)
	require.Eventually(t, func() bool { return sink.count() >= 2 }, 2*time.Second, 10*time.Millisecond)

	// What Unhold does: only the auto loop goes away.
	m.mohAutoStop()
	n := sink.count()
	time.Sleep(100 * time.Millisecond)
	assert.LessOrEqual(t, sink.count(), n+1, "auto music must stop on Unhold")

	// A manually started loop is not Unhold's business.
	manual, err := m.PlayMusicOnHold(context.Background(), WithMoHTone(tone))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return sink.count() >= n+2 }, 2*time.Second, 10*time.Millisecond)
	m.mohAutoStop()
	grown := sink.count()
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, sink.count(), grown, "manual music must survive Unhold")
	require.NoError(t, manual.Stop())

	// Loop goroutine self-clears d.moh after Stop returns; ensure the slot
	// is free before starting the next one (otherwise the assert below races
	// with the deferred cleanup).
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.moh == nil
	}, 2*time.Second, 5*time.Millisecond)

	// Hold's auto start is silent when nothing is configured (no dialog-level
	// tone yet) and is a no-op when no override and no default exist.
	m.mohTone = audio.Tone{}
	m.mohAutoStart(context.Background(), nil)

	// An active manual loop must not be stopped or replaced by a subsequent
	// auto start. The new auto start sees the slot is busy and skips.
	m.mohTone = tone
	manual2, err := m.PlayMusicOnHold(context.Background(), WithMoHTone(tone))
	require.NoError(t, err)
	pre := sink.count()
	m.mohAutoStart(context.Background(), nil) // manual loop already active: skip
	require.Equal(t, pre, sink.count(), "auto start must not stop an existing manual loop")
	m.mohAutoStop()                           // manual is not auto: no-op
	require.NoError(t, manual2.Stop())
}
