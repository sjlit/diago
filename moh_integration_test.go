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

	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
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

	uasCh := make(chan *InboundCall, 1)
	// The UAS handler runs on serve goroutines: Answer failures must be
	// reported to the test instead of calling require inside the handler
	// (FailNow there abandons the handler and hangs the test).
	handlerErr := make(chan error, 1)

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas, err := newAgent(withUserAgent(ua),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		withMediaConfig(MediaConfig{
			Codecs:      []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
			MusicOnHold: mohTone,
		}),
	)
	require.NoError(t, err)
	err = uas.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		if err := d.Answer(ctx); err != nil {
			handlerErr <- err
			return err
		}
		uasCh <- d
		<-d.Context().Done()
		return nil
	})
	require.NoError(t, err)
	uasPort := agentPort(t, uas)

	ua2, _ := sipgo.NewUA()
	defer ua2.Close()
	caller := newDialer(t, ua2)
	// The UAC needs a SIP server registered so it can answer re-INVITEs from
	// the UAS after our Hold. ServeBackground with an empty handler is the
	// minimal hook.
	require.NoError(t, caller.ServeBackground(ctx, func(ctx context.Context, _ *InboundCall) error {
		return nil
	}))

	dialog, err := caller.DialURI(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: uasPort})
	require.NoError(t, err)
	defer dialog.Close(ctx)

	var d *InboundCall
	select {
	case d = <-uasCh:
	case err := <-handlerErr:
		t.Fatalf("uas handler failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("uas never received the call")
	}

	var packets atomic.Uint64
	go func() {
		buf := make([]byte, media.RTPBufSize)
		for {
			if _, err := dialog.rtpPacketReader.Read(buf); err != nil {
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

	// The UAS handler runs on serve goroutines: report its failures from the
	// test goroutine instead of calling require inside the handler.
	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas, err := newAgent(withUserAgent(ua),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
	)
	require.NoError(t, err)
	err = uas.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		heldCb := func(dm *Media) {
			// Negotiated recvonly means the remote put us on hold.
			if dm.IsRemoteHeld() {
				uasHeld.Store(true)
			} else {
				uasHeld.Store(false)
			}
		}
		if err := d.Answer(ctx, AnswerOptions{OnMediaUpdate: heldCb}); err != nil {
			return err
		}
		<-d.Context().Done()
		return nil
	})
	require.NoError(t, err)

	ua2, _ := sipgo.NewUA()
	defer ua2.Close()
	caller := newDialer(t, ua2)

	dialog, err := caller.DialURI(ctx, sip.Uri{User: "bob", Host: "127.0.0.1", Port: agentPort(t, uas)})
	require.NoError(t, err)
	defer dialog.Close(ctx)

	require.NoError(t, dialog.Hold(ctx))
	require.Eventually(t, uasHeld.Load, 3*time.Second, 20*time.Millisecond,
		"UAS must observe IsRemoteHeld() once the UAC sends sendonly")

	require.NoError(t, dialog.Unhold(ctx))
	require.Eventually(t, func() bool { return !uasHeld.Load() }, 3*time.Second, 20*time.Millisecond,
		"UAS must clear IsRemoteHeld() after the UAC unholds")
}
