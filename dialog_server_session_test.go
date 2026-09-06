// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationDialogServerEarlyMedia(t *testing.T) {
	skipShort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialer *Diago
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15020,
			},
		))

		// Run listener to accepte reinvites, but it should not receive any request
		err := dg.ServeBackground(ctx, nil)
		require.NoError(t, err)

		dialer = dg
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15010,
		},
	))

	waitDialog := make(chan *DialogServerSession)
	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
		t.Log("Call received")
		waitDialog <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)

	allResponses := []sip.Response{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		dialog, err := dialer.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15010}, WithOnResponse(
			func(res *sip.Response) error {
				t.Log("Received resp", res.StatusCode)
				allResponses = append(allResponses, *res.Clone())
				return nil
			},
		))
		if err != nil {
			t.Log("Failed to dial", err)
			return
		}
		defer dialog.Close()
		<-dialog.Context().Done()
		t.Log("Dialog done")
	}()

	d := <-waitDialog

	err = d.ProgressMedia()
	require.NoError(t, err)

	// It is valid to also send 180
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, d.Ringing())

	// We can play some file ringtone
	playback, err := d.PlaybackCreate()
	require.NoError(t, err)
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)

	// We can now answer
	err = d.Answer()
	require.NoError(t, err)

	// New playback is needed to follow new media session
	playback, err = d.PlaybackCreate()
	require.NoError(t, err)
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	d.Hangup(context.TODO())

	wg.Wait()
	require.Len(t, allResponses, 3)
	assert.Equal(t, 183, allResponses[0].StatusCode)
	assert.Equal(t, 180, allResponses[1].StatusCode)
	assert.Equal(t, 200, allResponses[2].StatusCode)
}

func TestIntegrationDialogServerReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15070,
			},
		))

		// Run listener to accepte reinvites, but it should not receive any request
		err := dg.ServeBackground(ctx, nil)
		require.NoError(t, err)

		go func() {
			dialog, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060})
			require.NoError(t, err)
			<-dialog.Context().Done()
			t.Log("Dialog done")
		}()
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15060,
		},
	))

	waitDialog := make(chan *DialogServerSession)
	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
		t.Log("Call received")
		waitDialog <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)
	d := <-waitDialog

	err = d.Answer()
	require.NoError(t, err)
	err = d.ReInvite(d.Context())
	require.NoError(t, err)

	d.Hangup(context.TODO())
}

func TestIntegrationDialogServerPeerCodecPruneReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas := NewDiago(ua, WithTransport(Transport{
		Transport:       "udp",
		BindHost:        "127.0.0.1",
		BindPort:        15080,
		MediaExternalIP: net.IPv4(203, 0, 113, 10),
	}))
	err := uas.ServeBackground(ctx, func(d *DialogServerSession) {
		// This is the reported role: the peer sends the initial INVITE and
		// Diago answers it as the UAS. RTP NAT must not change the SIP flow.
		err := d.Answer(
			WithRTPNAT(media.RTPNATSymetric),
			WithOnMediaUpdate(func(*DialogMedia) {}),
		)
		require.NoError(t, err)
		reader, err := d.AudioReader()
		require.NoError(t, err)
		go func() {
			_, _ = reader.Read(make([]byte, 160))
		}()
		<-d.Context().Done()
	})
	require.NoError(t, err)

	peerUA, _ := sipgo.NewUA(sipgo.WithUserAgent("peer"))
	defer peerUA.Close()
	peer := newDialer(peerUA)
	err = peer.ServeBackground(ctx, func(*DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := peer.Invite(ctx, sip.Uri{User: "service", Host: "127.0.0.1", Port: 15080})
	require.NoError(t, err)
	defer dialog.Close()
	require.Contains(t, string(dialog.InviteRequest.Body()), " 0 8 101")

	// The initial peer offer contains PCMU, PCMA and telephone-event. The
	// post-answer offer intentionally prunes PCMA, matching the SBC behavior.
	prunedMedia := dialog.MediaSession().Fork()
	prunedMedia.Codecs = []media.Codec{
		media.CodecAudioUlaw,
		media.CodecTelephoneEvent8000,
	}
	prunedOffer := prunedMedia.LocalSDP()
	require.Contains(t, string(prunedOffer), " 0 101")
	require.NotContains(t, string(prunedOffer), " 0 8 101")
	reinvite := sip.NewRequest(sip.INVITE, dialog.RemoteContact().Address)
	reinvite.AppendHeader(dialog.InviteRequest.Contact())
	reinvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	reinvite.SetBody(prunedOffer)

	reinviteCtx, cancelReinvite := context.WithTimeout(ctx, 6*time.Second)
	defer cancelReinvite()
	// Under load the UAS may still consider the initial INVITE transaction
	// pending when the re-INVITE lands and correctly rejects it with 491
	// (RFC 3261 section 14.2). Retry briefly instead of failing the test.
	var res *sip.Response
	for {
		res, err = dialog.Do(reinviteCtx, reinvite.Clone())
		require.NoError(t, err)
		if res.StatusCode != sip.StatusRequestPending {
			break
		}
		select {
		case <-reinviteCtx.Done():
			t.Fatal("re-INVITE kept being rejected as request pending")
		case <-time.After(200 * time.Millisecond):
		}
	}
	require.Equal(t, sip.StatusOK, res.StatusCode)
	require.NotNil(t, res.Contact())
	contentType := res.ContentType()
	require.NotNil(t, contentType)
	require.Equal(t, "application/sdp", contentType.Value())
	require.NotEmpty(t, res.Body())
	require.Contains(t, string(res.Body()), "c=IN IP4 203.0.113.10")
	require.NotContains(t, string(res.Body()), "c=IN IP4 127.0.0.1")

	// Complete the re-INVITE transaction from the peer/UAC side.
	ack := sip.NewRequest(sip.ACK, res.Contact().Address)
	require.NoError(t, dialog.WriteRequest(ack))
	dialog.Hangup(ctx)
}

func TestIntegrationDialogServerRefer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialer *Diago
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("dialer"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15071,
				ID:        "udp",
			},
		))

		// Run listener to accepte reinvites, but it should not receive any request
		err := dg.ServeBackground(ctx, nil)
		require.NoError(t, err)
		dialer = dg
	}

	dialCall := func() {
		dialog, err := dialer.NewDialog(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15070})
		require.NoError(t, err)

		go func() {
			err := dialog.Invite(ctx, WithOnRefer(func(referDialog *DialogClientSession) error {
				// referDialog.
				if err := referDialog.Invite(ctx); err != nil {
					return err
				}
				if err := referDialog.Ack(ctx); err != nil {
					return err
				}

				return referDialog.Hangup(ctx)
			}))
			require.NoError(t, err)

			dialog.Ack(ctx)
			<-dialog.Context().Done()
			t.Log("Dialog done")
		}()
	}

	// UAS that accepts REFER
	// waitReferDialog := make(chan *DialogServerSession)
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15072,
			},
		))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			t.Log("Call INVITE due to REFER received")
			// waitReferDialog <- d
			switch d.ToUser() {
			case "busy":
				d.Respond(sip.StatusBusyHere, "Busy Here", nil)
				return
			case "noanswer":
				d.Ringing()
				return
			default:
				d.Answer()
			}

			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15070,
		},
	))

	waitDialog := make(chan *DialogServerSession)
	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
		t.Log("Call received")
		waitDialog <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)

	t.Run("Successfull", func(t *testing.T) {
		dialCall()
		d := <-waitDialog
		defer d.Hangup(ctx)

		err = d.Answer()
		require.NoError(t, err)

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{Host: "127.0.0.1", Port: 15072}, ReferServerOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, 200, <-referState)
	})

	t.Run("UnreachableRefer", func(t *testing.T) {
		dialCall()
		d := <-waitDialog
		defer d.Hangup(ctx)

		err = d.Answer()
		require.NoError(t, err)

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{User: "noanswer", Host: "127.0.0.1", Port: 15072}, ReferServerOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, sip.StatusTemporarilyUnavailable, <-referState)
	})

	t.Run("BusyRefer", func(t *testing.T) {
		dialCall()
		d := <-waitDialog
		defer d.Hangup(ctx)

		err = d.Answer()
		require.NoError(t, err)

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{User: "busy", Host: "127.0.0.1", Port: 15072}, ReferServerOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, sip.StatusBusyHere, <-referState)
	})
}

func TestIntegrationDialogServerPlayback(t *testing.T) {
	skipShort(t)
	rtpBuf := newRTPWriterBuffer()
	dialog := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
			RTPPacketWriter: media.NewRTPPacketWriter(rtpBuf, media.CodecAudioUlaw),
		},
	}

	playback, err := dialog.PlaybackCreate()
	require.NoError(t, err)

	initTS := dialog.RTPPacketWriter.InitTimestamp()
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS := dialog.RTPPacketWriter.PacketHeader.Timestamp - initTS
	assert.Greater(t, diffTS, uint32(1000))

	time.Sleep(100 * time.Millisecond) // 4 frames
	initTS = dialog.RTPPacketWriter.InitTimestamp()
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS2 := dialog.RTPPacketWriter.PacketHeader.Timestamp - initTS
	t.Log(initTS, diffTS2)

	// Timestamp should be offset more than previous diff by Sleep
	assert.Greater(t, diffTS2, diffTS+5*media.CodecAudioUlaw.SampleTimestamp())
}

// TestIntegrationDialogServerAnswerLate covers the late-offer flow: INVITE
// without SDP, 200 OK carrying our offer, ACK carrying the remote answer.
// AnswerLate returns only after the ACK was processed, so media must be fully
// negotiated (RemoteSDP applied) and monitoring started when it returns.
func TestIntegrationDialogServerAnswerLate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitDialog := make(chan *DialogServerSession, 1)
	answerLateErr := make(chan error, 1)
	audioGot := make(chan int, 1)

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("lateoffer-uas"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15100,
			},
		))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			waitDialog <- d
			// Blocks until the ACK is processed (RemoteSDP applied).
			answerLateErr <- d.AnswerLate()

			reader, err := d.AudioReader()
			if err != nil {
				t.Log("AudioReader failed", err)
				return
			}
			go func() {
				buf := make([]byte, media.RTPBufSize)
				n, err := reader.Read(buf)
				if err != nil {
					return
				}
				audioGot <- n
			}()
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	// Raw sipgo UAC: diago clients always send early offers, so the late
	// offer flow needs INVITE without body and ACK with the answer.
	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("lateoffer-uac"))
	defer ua.Close()

	uacAddr := "127.0.0.1:15101"
	srv, err := sipgo.NewServer(ua)
	require.NoError(t, err)
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
	go func() {
		if err := srv.ListenAndServe(ctx, "udp", uacAddr); err != nil {
			t.Log("UAC listener stopped", err)
		}
	}()
	time.Sleep(100 * time.Millisecond)

	cli, err := sipgo.NewClient(ua, sipgo.WithClientAddr(uacAddr))
	require.NoError(t, err)

	// Our RTP endpoint referenced by the answer SDP
	rtpSock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer rtpSock.Close()
	rtpPort := rtpSock.LocalAddr().(*net.UDPAddr).Port

	recipient := sip.Uri{User: "service", Host: "127.0.0.1", Port: 15100}
	invite := sip.NewRequest(sip.INVITE, recipient)
	invite.AppendHeader(sip.NewHeader("Contact", "<sip:127.0.0.1:15101>"))
	require.NoError(t, sipgo.ClientRequestBuild(cli, invite))

	tx, err := cli.TransactionRequest(ctx, invite)
	require.NoError(t, err)

	var res200 *sip.Response
	deadline := time.After(10 * time.Second)
	for res200 == nil {
		select {
		case res := <-tx.Responses():
			if res.IsSuccess() {
				res200 = res
			}
		case <-deadline:
			t.Fatal("no 200 OK received for late offer")
		}
	}
	require.NotEmpty(t, res200.Body(), "200 OK must carry our SDP offer")

	// Target the exact connection line from the offer (it may be a non-loopback
	// interface IP resolved for the media session).
	offer := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(res200.Body(), &offer))
	offerMd, err := offer.MediaDescription("audio")
	require.NoError(t, err)
	offerCi, err := offer.ConnectionInformationFor("audio")
	require.NoError(t, err)

	target := recipient
	if res200.Contact() != nil {
		target = res200.Contact().Address
	}

	answerSDP := fmt.Sprintf(`v=0
o=- 77 2 IN IP4 127.0.0.1
s=lateoffer-test
c=IN IP4 127.0.0.1
t=0 0
m=audio %d RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=sendrecv
`, rtpPort)

	// ACK carrying the answer (same construction as sipgo's newAckRequestUAC)
	ack := sip.NewRequest(sip.ACK, target)
	ack.SipVersion = invite.SipVersion
	ack.AppendHeader(sip.HeaderClone(invite.From()))
	ack.AppendHeader(sip.HeaderClone(res200.To()))
	ack.AppendHeader(sip.HeaderClone(invite.CallID()))
	ack.AppendHeader(sip.HeaderClone(invite.CSeq()))
	cseq := ack.CSeq()
	cseq.MethodName = sip.ACK
	maxFwd := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxFwd)
	ack.AppendHeader(sip.HeaderClone(invite.Contact()))
	ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	ack.SetBody([]byte(answerSDP))
	ack.SetTransport(invite.Transport())
	require.NoError(t, cli.WriteRequest(ack))

	select {
	case err := <-answerLateErr:
		require.NoError(t, err, "AnswerLate must succeed once the ACK answer is applied")
	case <-time.After(10 * time.Second):
		t.Fatal("AnswerLate did not return")
	}

	// Media must be live: stream RTP toward the negotiated address and
	// expect the audio reader to receive the payload.
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SSRC: 0x123456},
		Payload: make([]byte, 160),
	}
	udpAddr := &net.UDPAddr{IP: offerCi.IP, Port: offerMd.Port}
	for i := 0; i < 20; i++ {
		pkt.Header.SequenceNumber = uint16(i + 1)
		pkt.Header.Timestamp = uint32(i+1) * 160
		data, err := pkt.Marshal()
		require.NoError(t, err)
		_, err = rtpSock.WriteToUDP(data, udpAddr)
		require.NoError(t, err)
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case n := <-audioGot:
		require.Equal(t, 160, n, "audio reader must receive RTP payload")
	case <-time.After(5 * time.Second):
		t.Fatal("no RTP payload received through audio reader")
	}

	// Terminate with BYE; the handler and the wrapper hangup unwind cleanly.
	bye := sip.NewRequest(sip.BYE, target)
	bye.AppendHeader(sip.HeaderClone(invite.From()))
	bye.AppendHeader(sip.HeaderClone(res200.To()))
	bye.AppendHeader(sip.HeaderClone(invite.CallID()))
	byeCSeq := sip.CSeqHeader{SeqNo: invite.CSeq().SeqNo + 1, MethodName: sip.BYE}
	bye.AppendHeader(&byeCSeq)
	bye.SetTransport(invite.Transport())
	require.NoError(t, cli.WriteRequest(bye))
}
