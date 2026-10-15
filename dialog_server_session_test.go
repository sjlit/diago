// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/siptest"
	"github.com/pion/rtp"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInboundCallReferBeforeAnswer locks the nil-InviteResponse guard:
// Refer on a dialog that has not sent any response must return an error, not
// panic — the state check has to run before d.InviteResponse is dereferenced.
func TestInboundCallReferBeforeAnswer(t *testing.T) {
	inviteReq, err := diagotestSafeNewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: 5060, User: "bob"})
	require.NoError(t, err)
	inviteReq.AppendHeader(sip.NewHeader("Contact", "<sip:127.0.0.1:11111>"))

	ua := sipgo.DialogUA{
		Client:     &sipgo.Client{},
		ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "tester", Host: "127.0.0.1", Port: 5060}},
	}
	sess, err := ua.ReadInvite(inviteReq, siptest.NewServerTxRecorder(inviteReq))
	require.NoError(t, err)
	d := InboundCall{DialogServerSession: sess}

	err = d.Refer(context.TODO(), sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: 5060})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "answered")
}

func TestIntegrationDialogServerEarlyMedia(t *testing.T) {
	skipShort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialer *Agent
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		// Run listener to accepte reinvites, but it should not receive any request
		dialer, _ = servingAgent(t, ctx, nil,
			WithUserAgent(ua),
			WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	waitDialog := make(chan *InboundCall)
	_, srvPort := servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
		t.Log("Call received")
		waitDialog <- d
		<-d.Context().Done()
		return nil
	}, WithUserAgent(ua), WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))

	allResponses := []sip.Response{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		dialog, err := dialer.DialURI(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: srvPort}, WithOnResponse(
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

	err := d.ProgressMedia(ctx)
	require.NoError(t, err)

	// It is valid to also send 180
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, d.Ringing(ctx))

	// We can play some file ringtone
	playback, err := d.NewPlayback()
	require.NoError(t, err)
	_, err = playback.PlayFileContext(context.Background(), "testdata/files/demo-echodone.wav")
	require.NoError(t, err)

	// We can now answer
	err = d.Answer(ctx)
	require.NoError(t, err)

	// New playback is needed to follow new media session
	playback, err = d.NewPlayback()
	require.NoError(t, err)
	_, err = playback.PlayFileContext(context.Background(), "testdata/files/demo-echodone.wav")
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

	waitDialog := make(chan *InboundCall)
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		// Run listener to accepte reinvites, but it should not receive any request
		dialer, _ := servingAgent(t, ctx, nil,
			WithUserAgent(ua),
			WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		)

		uaSrv, _ := sipgo.NewUA()
		defer uaSrv.Close()

		_, srvPort := servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
			t.Log("Call received")
			waitDialog <- d
			<-d.Context().Done()
			return nil
		}, WithUserAgent(uaSrv), WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))

		go func() {
			dialog, err := dialer.DialURI(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: srvPort})
			require.NoError(t, err)
			<-dialog.Context().Done()
			t.Log("Dialog done")
		}()
	}

	d := <-waitDialog

	err := d.Answer(ctx)
	require.NoError(t, err)
	err = d.ReInvite(d.Context())
	require.NoError(t, err)

	d.Hangup(context.TODO())
}

func TestIntegrationDialogServerPeerCodecPruneReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The UAS serve handler runs on library goroutines: failures must be
	// reported through a channel, never with require inside the handler.
	handlerErr := make(chan error, 1)

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	_, uasPort := servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
		// This is the reported role: the peer sends the initial INVITE and
		// Agent answers it as the UAS. RTP NAT must not change the SIP flow.
		err := d.Answer(ctx,
			WithRTPNAT(media.RTPNATSymetric),
			WithOnMediaUpdate(func(*Media) {}),
		)
		if err != nil {
			// Serve handler goroutine: report through the channel, never
			// through require (FailNow would abandon the handler).
			handlerErr <- err
			return err
		}
		reader, err := d.AudioReader()
		if err != nil {
			handlerErr <- err
			return err
		}
		go func() {
			_, _ = reader.Read(make([]byte, 160))
		}()
		<-d.Context().Done()
		return nil
	}, WithUserAgent(ua),
		WithMediaProfile("public", MediaConfig{
			Codecs:  []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
			Network: NetworkConfig{ExternalIP: net.IPv4(203, 0, 113, 10)},
		}),
		WithTransport(Transport{
			Network:  "udp",
			BindHost: "127.0.0.1",
			BindPort: 0,
			Profile:  "public",
		}),
	)

	peerUA, _ := sipgo.NewUA(sipgo.WithUserAgent("peer"))
	defer peerUA.Close()
	peer := newDialer(t, peerUA)
	err := peer.ServeBackground(ctx, func(ctx context.Context, _ *InboundCall) error {
		return nil
	})
	require.NoError(t, err)

	dialog, err := peer.DialURI(ctx, sip.Uri{User: "service", Host: "127.0.0.1", Port: uasPort})
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
	// The UAS answers 491 while the initial INVITE transaction is still
	// pending (RFC 3261 §14.2): this re-INVITE races with ACK processing on
	// the server. Retry on 491 like a real UAC would (cf. reInviteExchange).
	// Each attempt clones the template as Do mutates the request in place.
	var res *sip.Response
	for {
		res, err = dialog.Do(reinviteCtx, reinvite.Clone())
		require.NoError(t, err)
		if res.StatusCode != sip.StatusRequestPending {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-reinviteCtx.Done():
			require.NoError(t, reinviteCtx.Err())
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

	select {
	case err := <-handlerErr:
		t.Fatalf("uas handler failed: %v", err)
	default:
	}
}

func TestIntegrationDialogServerRefer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitDialog := make(chan *InboundCall)
	env := newReferEnv(t, ctx, func(ctx context.Context, d *InboundCall) error {
		t.Log("Call received")
		waitDialog <- d
		<-d.Context().Done()
		return nil
	})

	dialCall := func() {
		dialog, err := env.dialer.NewCall(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: env.transferorPort})
		require.NoError(t, err)

		go func() {
			err := dialog.Invite(ctx, WithOnRefer(func(referDialog *OutboundCall) error {
				if err := referDialog.Invite(ctx); err != nil {
					return err
				}
				if err := referDialog.Ack(ctx); err != nil {
					return err
				}

				return referDialog.Hangup(ctx)
			}))
			// Dial goroutine: Error (not require) is goroutine-safe.
			if err != nil {
				t.Error(err)
				return
			}

			dialog.Ack(ctx)
			<-dialog.Context().Done()
			t.Log("Dialog done")
		}()
	}

	t.Run("successfull", func(t *testing.T) {
		dialCall()
		d := <-waitDialog
		defer d.Hangup(ctx)

		err := d.Answer(ctx)
		require.NoError(t, err)

		notify := newReferNotifier()
		err = d.Refer(d.Context(), env.uasTarget(""), notify.option())
		require.NoError(t, err)

		notify.expect(t, 100, 200)
	})

	t.Run("unreachable refer", func(t *testing.T) {
		dialCall()
		d := <-waitDialog
		defer d.Hangup(ctx)

		err := d.Answer(ctx)
		require.NoError(t, err)

		notify := newReferNotifier()
		err = d.Refer(d.Context(), env.uasTarget("noanswer"), notify.option())
		require.NoError(t, err)

		notify.expect(t, 100, sip.StatusTemporarilyUnavailable)
	})

	t.Run("busy refer", func(t *testing.T) {
		dialCall()
		d := <-waitDialog
		defer d.Hangup(ctx)

		err := d.Answer(ctx)
		require.NoError(t, err)

		notify := newReferNotifier()
		err = d.Refer(d.Context(), env.uasTarget("busy"), notify.option())
		require.NoError(t, err)

		notify.expect(t, 100, sip.StatusBusyHere)
	})
}

func TestIntegrationDialogServerPlayback(t *testing.T) {
	skipShort(t)
	rtpBuf := &rtpBufferTestWriter{}
	dialog := &InboundCall{
		callMedia: Media{
			mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
			rtpPacketWriter: media.NewRTPPacketWriter(rtpBuf, media.CodecAudioUlaw),
		},
	}

	playback, err := dialog.NewPlayback()
	require.NoError(t, err)

	initTS := dialog.rtpPacketWriter.InitTimestamp()
	_, err = playback.PlayFileContext(context.Background(), "testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS := dialog.rtpPacketWriter.PacketHeader.Timestamp - initTS
	assert.Greater(t, diffTS, uint32(1000))

	time.Sleep(100 * time.Millisecond) // 4 frames
	initTS = dialog.rtpPacketWriter.InitTimestamp()
	_, err = playback.PlayFileContext(context.Background(), "testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS2 := dialog.rtpPacketWriter.PacketHeader.Timestamp - initTS
	t.Log(initTS, diffTS2)

	// Timestamp should be offset more than previous diff by Sleep
	assert.Greater(t, diffTS2, diffTS+5*media.CodecAudioUlaw.SampleTimestamp())
}

// TestIntegrationDialogServerAnswerLate covers the late-offer flow: INVITE
// without SDP, 200 OK carrying our offer, ACK carrying the remote answer.
// AnswerLate returns only after the ACK was processed, so media must be fully
// negotiated (RemoteSDP applied) and monitoring started when it returns.
// waitUDPBound blocks until a listener is up on addr. sipgo starts its
// listeners in a background goroutine and exposes no ready signal, so tests
// that talk to the socket right after ListenAndServe need a probe: binding the
// same UDP address succeeds until the listener takes it.
func waitUDPBound(t *testing.T, addr string) {
	t.Helper()
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			return true // address in use: the listener under test is up
		}
		conn.Close()
		return false
	}, 3*time.Second, 10*time.Millisecond, "no listener came up on %s", addr)
}

// lateOfferUAC is a raw sipgo user agent that drives a late-offer call: diago
// clients always send early offers, so it sends the INVITE without a body and
// the answer travels in the ACK it builds. The two late-offer tests need the
// same scaffolding - listener, client, INVITE and the wait for the 200 OK - and
// differ only in what they put into the ACK.
type lateOfferUAC struct {
	cli     *sipgo.Client
	invite  *sip.Request
	res200  *sip.Response
	target  sip.Uri
	addr    string
	rtpSock *net.UDPConn
	byeNum  atomic.Int32
}

// rtpPort is the local RTP endpoint the answer SDP must reference.
func (u *lateOfferUAC) rtpPort() int {
	return u.rtpSock.LocalAddr().(*net.UDPAddr).Port
}

// startLateOfferUAC starts the UAC against the agent on srvPort and returns
// once its 200 OK carrying the offer has arrived.
func startLateOfferUAC(t *testing.T, ctx context.Context, srvPort int, userAgent string) *lateOfferUAC {
	t.Helper()
	u := &lateOfferUAC{}

	uacPort := freeUDPPort(t)
	u.addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(uacPort))

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent(userAgent))
	t.Cleanup(func() { ua.Close() })

	srv, err := sipgo.NewServer(ua)
	require.NoError(t, err)
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		u.byeNum.Add(1)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
	go func() {
		if err := srv.ListenAndServe(ctx, "udp", u.addr); err != nil {
			t.Log("UAC listener stopped", err)
		}
	}()
	waitUDPBound(t, u.addr)

	cli, err := sipgo.NewClient(ua, sipgo.WithClientAddr(u.addr))
	require.NoError(t, err)
	u.cli = cli

	// Our RTP endpoint referenced by the answer SDP
	rtpSock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { rtpSock.Close() })
	u.rtpSock = rtpSock

	recipient := sip.Uri{User: "service", Host: "127.0.0.1", Port: srvPort}
	invite := sip.NewRequest(sip.INVITE, recipient)
	invite.AppendHeader(sip.NewHeader("Contact", "<sip:"+u.addr+">"))
	require.NoError(t, sipgo.ClientRequestBuild(cli, invite))
	u.invite = invite

	tx, err := cli.TransactionRequest(ctx, invite)
	require.NoError(t, err)

	deadline := time.After(10 * time.Second)
	for u.res200 == nil {
		select {
		case res := <-tx.Responses():
			if res.IsSuccess() {
				u.res200 = res
			}
		case <-deadline:
			t.Fatal("no 200 OK for late offer")
		}
	}
	require.NotEmpty(t, u.res200.Body(), "200 OK must carry our SDP offer")

	u.target = recipient
	if c := u.res200.Contact(); c != nil {
		u.target = c.Address
	}
	return u
}

func TestIntegrationDialogServerAnswerLate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitDialog := make(chan *InboundCall, 1)
	answerLateErr := make(chan error, 1)
	audioGot := make(chan int, 1)

	uaSrv, _ := sipgo.NewUA(sipgo.WithUserAgent("lateoffer-uas"))
	defer uaSrv.Close()

	_, srvPort := servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
		waitDialog <- d
		// AnswerLate starts the 200 OK; WaitAck blocks until the ACK is
		// processed (RemoteSDP applied).
		if err := d.AnswerLate(ctx); err != nil {
			answerLateErr <- err
			return nil
		}
		answerLateErr <- d.WaitAck(ctx)

		reader, err := d.AudioReader()
		if err != nil {
			t.Log("AudioReader failed", err)
			return nil
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
		return nil
	}, WithUserAgent(uaSrv), WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))

	uac := startLateOfferUAC(t, ctx, srvPort, "lateoffer-uac")

	// Target the exact connection line from the offer (it may be a non-loopback
	// interface IP resolved for the media session).
	offer := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(uac.res200.Body(), &offer))
	offerMd, err := offer.MediaDescription("audio")
	require.NoError(t, err)
	offerCi, err := offer.ConnectionInformationFor("audio")
	require.NoError(t, err)

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
`, uac.rtpPort())

	// ACK carrying the answer (same construction as sipgo's newAckRequestUAC)
	ack := sip.NewRequest(sip.ACK, uac.target)
	ack.SipVersion = uac.invite.SipVersion
	ack.AppendHeader(sip.HeaderClone(uac.invite.From()))
	ack.AppendHeader(sip.HeaderClone(uac.res200.To()))
	ack.AppendHeader(sip.HeaderClone(uac.invite.CallID()))
	ack.AppendHeader(sip.HeaderClone(uac.invite.CSeq()))
	cseq := ack.CSeq()
	cseq.MethodName = sip.ACK
	maxFwd := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxFwd)
	ack.AppendHeader(sip.HeaderClone(uac.invite.Contact()))
	ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	ack.SetBody([]byte(answerSDP))
	ack.SetTransport(uac.invite.Transport())
	require.NoError(t, uac.cli.WriteRequest(ack))

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
		_, err = uac.rtpSock.WriteToUDP(data, udpAddr)
		require.NoError(t, err)
		time.Sleep(20 * time.Millisecond)
	}

	n := recvWithin(t, audioGot, 5*time.Second)
	require.Equal(t, 160, n, "audio reader must receive RTP payload")

	// Terminate with BYE; the handler and the wrapper hangup unwind cleanly.
	bye := sip.NewRequest(sip.BYE, uac.target)
	bye.AppendHeader(sip.HeaderClone(uac.invite.From()))
	bye.AppendHeader(sip.HeaderClone(uac.res200.To()))
	bye.AppendHeader(sip.HeaderClone(uac.invite.CallID()))
	byeCSeq := sip.CSeqHeader{SeqNo: uac.invite.CSeq().SeqNo + 1, MethodName: sip.BYE}
	bye.AppendHeader(&byeCSeq)
	bye.SetTransport(uac.invite.Transport())
	require.NoError(t, uac.cli.WriteRequest(bye))
}

// A replayed ACK can be dispatched concurrently; notifyAck must not panic with
// "close of closed channel" and must leave the channel closed.
func TestInboundCallNotifyAckConcurrent(t *testing.T) {
	d := &InboundCall{}
	d.ackChannel()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.notifyAck()
		}()
	}
	wg.Wait()

	select {
	case <-d.ackChannel():
	default:
		t.Fatal("ack channel must be closed")
	}
}

// Only an ACK matching the INVITE CSeq may close ackCh: a stray ACK must not
// turn a real sipgo wait-loop timeout into a successful Answer.
func TestInboundCallMatchesInviteAck(t *testing.T) {
	inviteReq, err := diagotestSafeNewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	require.NoError(t, err)
	inviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.INVITE})

	d := &InboundCall{
		DialogServerSession: &sipgo.DialogServerSession{
			Dialog: sipgo.Dialog{InviteRequest: inviteReq},
		},
	}

	ack, err := diagotestSafeNewRequest(sip.ACK, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	require.NoError(t, err)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.ACK})
	assert.True(t, d.matchesInviteAck(ack))

	stray, err := diagotestSafeNewRequest(sip.ACK, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	require.NoError(t, err)
	stray.AppendHeader(&sip.CSeqHeader{SeqNo: 11, MethodName: sip.ACK})
	assert.False(t, d.matchesInviteAck(stray))
	assert.False(t, d.matchesInviteAck(nil))
}

// Regression: readAck used to process every ACK of the dialog, so a late-offer
// call that later received a peer re-INVITE ACK (no SDP, different CSeq) was
// misread as a missing late-offer answer and torn down. Foreign ACKs must be
// ignored, and a retransmitted initial ACK must not re-run the media apply.
func TestInboundCallReadAckIgnoresForeignAck(t *testing.T) {
	inviteReq, err := diagotestSafeNewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	require.NoError(t, err)
	inviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.INVITE})
	// Late offer: the initial INVITE carried no SDP.
	require.Nil(t, inviteReq.Body())

	d := &InboundCall{
		DialogServerSession: &sipgo.DialogServerSession{
			Dialog: sipgo.Dialog{InviteRequest: inviteReq},
		},
	}

	foreign, err := diagotestSafeNewRequest(sip.ACK, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	require.NoError(t, err)
	foreign.AppendHeader(&sip.CSeqHeader{SeqNo: 11, MethodName: sip.ACK})
	require.NoError(t, d.readAck(foreign, nil), "re-INVITE ACK must be ignored")
	require.Nil(t, d.ackResult())
	require.False(t, d.ackProcessed.Load(), "foreign ACK must not consume the initial-ACK slot")
}

// Regression: the late-offer ACK install path stopped refreshing the
// dialog-level remote-held flag, so IsRemoteHeld() stayed false after a peer
// answered our offer with sendonly (and stayed true after it unheld).
func TestMediaLateOfferAnswerUpdatesRemoteHeld(t *testing.T) {
	sess := &media.MediaSession{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Mode:   sdp.ModeSendrecv,
	}
	d := &Media{mediaSession: sess}

	answer := func(mode string) []byte {
		return []byte(`v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
c=IN IP4 127.0.0.1
t=0 0
m=audio 4000 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=` + mode)
	}

	got, err := d.applyLateOfferAnswerUnsafe(answer("sendonly"))
	require.NoError(t, err)
	require.Same(t, sess, got)
	require.True(t, d.IsRemoteHeld(), "sendonly answer means the remote holds us")

	_, err = d.applyLateOfferAnswerUnsafe(answer("sendrecv"))
	require.NoError(t, err)
	require.False(t, d.IsRemoteHeld(), "sendrecv answer must clear the flag")
}

// rtpBufferTestWriter records RTP packets written through it; it replaces the
// former production helper that was only ever used by tests.
type rtpBufferTestWriter struct {
	buf []*rtp.Packet
}

func (w *rtpBufferTestWriter) WriteRTP(p *rtp.Packet) error {
	w.buf = append(w.buf, p)
	return nil
}

// Regression: when the late-offer ACK carries an unusable SDP answer, readAck
// fails. The ACK fast-path used to mask that failure: AnswerLate blocked for a
// full 64*T1 retransmission window and returned a misleading error while the
// peer (already past its ACK) received neither BYE nor any final response.
// The dialog must be confirmed and BYE'd, and AnswerLate must fail promptly.
func TestIntegrationDialogServerLateOfferInvalidAckSDP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	answerErr := make(chan error, 1)
	stateCh := make(chan sip.DialogState, 1)

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("late-ack-uas"))
	defer ua.Close()

	_, srvPort := servingAgent(t, ctx, func(hctx context.Context, d *InboundCall) error {
		// AnswerLate initiates the 200 OK; WaitAck surfaces the ACK
		// processing failure (unusable SDP answer) promptly.
		err := d.AnswerLate(hctx)
		if err == nil {
			err = d.WaitAck(hctx)
		}
		answerErr <- err
		stateCh <- d.LoadState()
		select {
		case <-d.Context().Done():
		case <-time.After(3 * time.Second):
		}
		return nil
	}, WithUserAgent(ua), WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))

	uac := startLateOfferUAC(t, ctx, srvPort, "late-ack-uac")

	ack := sip.NewRequest(sip.ACK, uac.target)
	ack.SipVersion = uac.invite.SipVersion
	ack.AppendHeader(sip.HeaderClone(uac.invite.From()))
	ack.AppendHeader(sip.HeaderClone(uac.res200.To()))
	ack.AppendHeader(sip.HeaderClone(uac.invite.CallID()))
	ack.AppendHeader(sip.HeaderClone(uac.invite.CSeq()))
	cseq := ack.CSeq()
	cseq.MethodName = sip.ACK
	maxFwd := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxFwd)
	ack.AppendHeader(sip.HeaderClone(uac.invite.Contact()))
	ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	ack.SetBody([]byte("v=0\r\nthis-line-is-not-sdp\r\n"))
	ack.SetTransport(uac.invite.Transport())
	require.NoError(t, uac.cli.WriteRequest(ack))

	err := recvWithin(t, answerErr, 8*time.Second)
	require.Error(t, err, "unusable ACK answer must surface as an error")

	st := recvWithin(t, stateCh, 3*time.Second)
	require.GreaterOrEqual(t, st, sip.DialogStateConfirmed, "dialog must be confirmed")

	require.Eventually(t, func() bool { return uac.byeNum.Load() >= 1 }, 3*time.Second, 20*time.Millisecond,
		"peer must receive a BYE after the media failure")
}

// TestInboundCallAckObservation locks the non-blocking answer
// contract: WaitAck on an unanswered dialog fails fast, a second 2xx is
// rejected with ErrAlreadyAnswered, and the 64*T1 no-ACK outcome is recorded
// as ErrAckTimeout by the background 2xx exchange.
func TestInboundCallAckObservation(t *testing.T) {
	newSession := func(t *testing.T) *InboundCall {
		t.Helper()
		inviteReq, err := diagotestSafeNewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
		require.NoError(t, err)
		inviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.INVITE})
		return &InboundCall{
			DialogServerSession: &sipgo.DialogServerSession{
				Dialog: sipgo.Dialog{InviteRequest: inviteReq},
			},
		}
	}

	t.Run("wait ack on unanswered", func(t *testing.T) {
		d := newSession(t)
		require.ErrorIs(t, d.WaitAck(context.Background()), ErrDialogNotAnswered)
	})

	t.Run("second answer rejected", func(t *testing.T) {
		d := newSession(t)
		d.InitWithState(sip.DialogStateEstablished)
		res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
		require.ErrorIs(t, d.writeSuccessAsync(res), ErrAlreadyAnswered)
	})

	t.Run("ended dialog rejects answer", func(t *testing.T) {
		d := newSession(t)
		d.InitWithState(sip.DialogStateEnded)
		res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
		require.ErrorIs(t, d.writeSuccessAsync(res), ErrDialogEnded)
	})

	t.Run("no ack timeout recorded", func(t *testing.T) {
		d := newSession(t)
		// Simulate sipgo returning after the 64*T1 window without an ACK.
		d.finishSuccessWrite(nil)
		require.ErrorIs(t, d.AckErr(), ErrAckTimeout)

		// AckReceived must still report that no ACK was observed.
		select {
		case <-d.AckReceived():
			t.Fatal("ACK channel must stay open without an ACK")
		default:
		}
	})

	t.Run("concurrent answers serialized", func(t *testing.T) {
		inviteReq, err := diagotestSafeNewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
		require.NoError(t, err)
		inviteReq.AppendHeader(sip.NewHeader("Contact", "<sip:127.0.0.1:11111>"))
		inviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.INVITE})

		ua := sipgo.DialogUA{
			Client:     &sipgo.Client{},
			ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "tester", Host: "127.0.0.1", Port: 5060}},
		}
		tx := siptest.NewServerTxRecorder(inviteReq)
		sess, err := ua.ReadInvite(inviteReq, tx)
		require.NoError(t, err)
		defer tx.Terminate()

		d := &InboundCall{DialogServerSession: sess}
		res := sip.NewResponseFromRequest(sess.InviteRequest, sip.StatusOK, "OK", nil)

		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { errs <- d.writeSuccessAsync(res.Clone()) }()
		}

		var accepted, rejected int
		for i := 0; i < 2; i++ {
			err := <-errs
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrAlreadyAnswered):
				rejected++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		require.Equal(t, 1, accepted, "exactly one answer must be accepted")
		require.Equal(t, 1, rejected, "the concurrent answer must be rejected")

		// Complete the 2xx exchange so the background write loop exits.
		ack, err := diagotestSafeNewRequest(sip.ACK, sess.InviteRequest.Recipient)
		require.NoError(t, err)
		ack.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.ACK})
		require.NoError(t, d.DialogServerSession.ReadAck(ack, tx))
	})
}
