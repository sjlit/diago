// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

// TestIntegrationMusicOnHoldServerHold is the end-to-end check that Hold
// starts looping music through the negotiated codec, and Unhold stops it. The
// UAC's RTP reader counts incoming packets: growth while held, plateau after
// Unhold. The 40ms cadence tone + 20ms packet interval gives a ~2 pkt/40ms
// flow the assertion can latch onto within a couple of seconds.
//
// The UAC must have a registered server to handle the UAS's incoming
// re-INVITEs; newDialer alone does not register one. The test wires
// newDialer.InviteDialog to a serve handler so sipgo answers the re-INVITE.
func TestIntegrationMusicOnHoldServerHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mohTone := audio.Tone{Segments: []audio.ToneSegment{{Freqs: []float64{425}, On: 40 * time.Millisecond}}}

	uasCh := make(chan *DialogServerSession, 1)

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas := NewDiago(ua,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 15082}),
		WithMediaConfig(MediaConfig{
			Codecs:      []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
			MusicOnHold: mohTone,
		}),
	)
	err := uas.ServeBackground(ctx, func(d *DialogServerSession) {
		require.NoError(t, d.Answer())
		uasCh <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)

	ua2, _ := sipgo.NewUA()
	defer ua2.Close()
	caller := newDialer(ua2)
	// The UAC needs a SIP server registered so it can answer re-INVITEs from
	// the UAS after our Hold. ServeBackground with an empty handler is the
	// minimal hook.
	require.NoError(t, caller.ServeBackground(ctx, func(*DialogServerSession) {}))

	dialog, err := caller.Invite(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: 15082})
	require.NoError(t, err)
	defer dialog.Hangup(ctx)

	d := <-uasCh

	var packets atomic.Uint64
	go func() {
		buf := make([]byte, media.RTPBufSize)
		for {
			if _, err := dialog.RTPPacketReader.Read(buf); err != nil {
				return
			}
			packets.Add(1)
		}
	}()

	require.NoError(t, d.Hold(ctx))

	// Held window: music must be flowing, so the packet count keeps growing.
	require.Eventually(t, func() bool { return packets.Load() >= 5 }, 3*time.Second, 20*time.Millisecond)
	heldCount := packets.Load()

	require.NoError(t, d.Unhold(ctx))

	// After Unhold the server stops sending; no further packets should
	// accumulate within the bookkeeping window.
	time.Sleep(300 * time.Millisecond)
	assert.LessOrEqual(t, packets.Load(), heldCount+2, "no hold music after Unhold")
}

// TestIntegrationMusicOnHoldRemoteHoldIsDetected is the converse: UAC holds,
// UAS receives a sendonly answer and flips IsRemoteHeld() inside the
// OnMediaUpdate callback. The next re-INVITE (UAC Unhold) clears the flag.
func TestIntegrationMusicOnHoldRemoteHoldIsDetected(t *testing.T) {
	skipShort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var uasHeld atomic.Bool

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas := NewDiago(ua,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 15084}),
	)
	err := uas.ServeBackground(ctx, func(d *DialogServerSession) {
		heldCb := func(dm *DialogMedia) {
			// Negotiated recvonly means the remote put us on hold.
			if dm.IsRemoteHeld() {
				uasHeld.Store(true)
			} else {
				uasHeld.Store(false)
			}
		}
		require.NoError(t, d.Answer(WithOnMediaUpdate(heldCb)))
		<-d.Context().Done()
	})
	require.NoError(t, err)

	ua2, _ := sipgo.NewUA()
	defer ua2.Close()
	caller := newDialer(ua2)

	dialog, err := caller.Invite(ctx, sip.Uri{User: "bob", Host: "127.0.0.1", Port: 15084})
	require.NoError(t, err)
	defer dialog.Hangup(ctx)

	require.NoError(t, dialog.Hold(ctx))
	require.Eventually(t, uasHeld.Load, 3*time.Second, 20*time.Millisecond,
		"UAS must observe IsRemoteHeld() once the UAC sends sendonly")

	require.NoError(t, dialog.Unhold(ctx))
	require.Eventually(t, func() bool { return !uasHeld.Load() }, 3*time.Second, 20*time.Millisecond,
		"UAS must clear IsRemoteHeld() after the UAC unholds")
}