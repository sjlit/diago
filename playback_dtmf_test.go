// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eofReader is source that is immediately finished, used for DTMF reader stub
type eofReader struct{}

func (eofReader) Read(b []byte) (int, error) {
	return 0, io.EOF
}

func newTestDTMFPlayback(t *testing.T, w io.Writer, opts ...PlaybackDTMFOption) *AudioPlaybackDTMF {
	t.Helper()
	codec := media.CodecAudioUlaw
	control := NewAudioPlaybackControl(NewAudioPlayback(w, codec))

	// DTMF reader stub with immediately finished audio source. DTMF keys are
	// delivered manually via handleDTMF
	dtmfReader := &DTMFReader{
		dtmfReader: media.NewRTPDTMFReader(media.CodecTelephoneEvent8000, &media.RTPPacketReader{}, eofReader{}),
	}

	p := &AudioPlaybackDTMF{
		AudioPlaybackControl: control,
		dtmfCh:               make(chan rune, dtmfChSize),
		dtmfReader:           dtmfReader,
		stopCh:               make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func TestPlaybackDTMFInterruptAnyKey(t *testing.T) {
	data := make([]byte, 6400)
	cw := &countWriter{}
	p := newTestDTMFPlayback(t, cw)

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p.AudioPlaybackControl)
	require.Eventually(t, func() bool {
		_, bytesW := cw.stats()
		return bytesW > 320
	}, 2*time.Second, time.Millisecond)

	// Any key interrupts by default
	p.handleDTMF('5')

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(3 * time.Second):
		t.Fatal("play was not interrupted")
	}

	select {
	case dtmf := <-p.DTMF():
		assert.Equal(t, '5', dtmf)
	case <-time.After(time.Second):
		t.Fatal("DTMF was not delivered")
	}
	assert.Equal(t, PlaybackStateStopped, p.State())
}

func TestPlaybackDTMFInterruptKeysOnly(t *testing.T) {
	data := make([]byte, 32000)
	cw := &countWriter{}
	// Only '9' interrupts, other keys are delivered but do not stop
	p := newTestDTMFPlayback(t, cw, WithInterruptKeys("9"))

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p.AudioPlaybackControl)
	require.Eventually(t, func() bool {
		_, bytesW := cw.stats()
		return bytesW > 320
	}, 2*time.Second, time.Millisecond)

	p.handleDTMF('1')
	select {
	case err := <-done:
		t.Fatalf("play should continue, but got error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	p.handleDTMF('9')
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(3 * time.Second):
		t.Fatal("play was not interrupted")
	}

	assert.Equal(t, '1', <-p.DTMF())
	assert.Equal(t, '9', <-p.DTMF())
}

func TestPlaybackDTMFNoInterrupt(t *testing.T) {
	data := make([]byte, 3200)
	cw := &countWriter{}
	p := newTestDTMFPlayback(t, cw, WithInterruptKeys(""))

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p.AudioPlaybackControl)
	p.handleDTMF('1')

	select {
	case err := <-done:
		// Natural finish
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("play should not be interrupted")
	}
	assert.Equal(t, '1', <-p.DTMF())
}

func TestPlaybackDTMFReplay(t *testing.T) {
	data := make([]byte, 3200)
	cw := &countWriter{}
	p := newTestDTMFPlayback(t, cw, WithReplayKeys("*"))

	done := make(chan error, 1)
	var written int64
	go func() {
		var err error
		written, err = p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p.AudioPlaybackControl)
	require.Eventually(t, func() bool {
		_, bytesW := cw.stats()
		return bytesW > 320
	}, 2*time.Second, time.Millisecond)

	// Replay key must not interrupt
	p.handleDTMF('*')

	// Wait for replay to take effect: the second pass must have written at
	// least one chunk beyond the first pass (which was interrupted at the
	// first chunk). The 640B threshold (first chunk + one chunk) fires
	// early in the second pass so the interrupt below is delivered while
	// the second pass is still in flight (otherwise the test races with
	// natural end-of-stream on small data).
	require.Eventually(t, func() bool {
		_, bytesW := cw.stats()
		return bytesW > 640
	}, 5*time.Second, 2*time.Millisecond)

	// Non replay key interrupts after replay
	p.handleDTMF('#')
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(3 * time.Second):
		t.Fatal("play was not interrupted")
	}

	// Play was interrupted mid-second-pass, so written is a partial first
	// pass (one chunk) plus a partial second pass. The fact that written
	// exceeds the first chunk proves replay happened.
	assert.Greater(t, written, int64(320))
	assert.Equal(t, '*', <-p.DTMF())
	assert.Equal(t, '#', <-p.DTMF())
}

func TestPlaybackDTMFOnDTMFCallback(t *testing.T) {
	data := make([]byte, 3200)
	cw := &countWriter{}

	cb := &dtmfCollector{}
	p := newTestDTMFPlayback(t, cw, WithOnDTMF(cb.append))

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p.AudioPlaybackControl)
	p.handleDTMF('3')
	p.handleDTMF('4')
	<-done

	require.Eventually(t, func() bool {
		return cb.len() == 2
	}, time.Second, time.Millisecond)
	assert.Equal(t, []rune{'3', '4'}, cb.get())
}

// dtmfCollector collects dtmf keys thread safe
type dtmfCollector struct {
	mu   sync.Mutex
	keys []rune
}

func (c *dtmfCollector) append(dtmf rune) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, dtmf)
}

func (c *dtmfCollector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keys)
}

func (c *dtmfCollector) get() []rune {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rune{}, c.keys...)
}

func TestParseDTMFKeys(t *testing.T) {
	keys := parseDTMFKeys("*#9")
	assert.Len(t, keys, 3)
	_, ok := keys['*']
	assert.True(t, ok)
	_, ok = keys['#']
	assert.True(t, ok)
	_, ok = keys['9']
	assert.True(t, ok)
	_, ok = keys['1']
	assert.False(t, ok)

	assert.Empty(t, parseDTMFKeys(""))
}

// sendDTMFInjection sends in band DTMF event as RTP payload
func sendDTMFInjection(t *testing.T, conn *net.UDPConn, raddr *net.UDPAddr, seq *uint16, event rune) {
	t.Helper()
	events := media.RTPDTMFEncode8000(event)
	ts := uint32(time.Now().UnixNano())
	for i, ev := range events {
		payload := media.DTMFEncode(ev)
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         i == 0,
				PayloadType:    media.CodecTelephoneEvent8000.PayloadType,
				SequenceNumber: *seq,
				Timestamp:      ts,
				SSRC:           0x1234,
			},
			Payload: payload,
		}
		*seq++
		buf, err := pkt.Marshal()
		require.NoError(t, err)
		_, err = conn.WriteToUDP(buf, raddr)
		require.NoError(t, err)
		if i == 0 {
			// Small gap between start and end event
			time.Sleep(30 * time.Millisecond)
		}
	}
}

func TestIntegrationPlaybackDTMF(t *testing.T) {
	sess, err := media.NewMediaSession(net.IPv4(127, 0, 0, 1), 0)
	require.NoError(t, err)
	defer sess.Close()

	// Injector plays remote party: receives our RTP and sends DTMF
	injector, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer injector.Close()
	sess.SetRemoteAddr(injector.LocalAddr().(*net.UDPAddr))

	// Drain written RTP, so playback is not blocked
	go io.Copy(io.Discard, injector)

	dialog := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession:    sess,
			audioWriter:     media.NewRTPPacketWriter(sess, media.CodecAudioUlaw),
			RTPPacketReader: media.NewRTPPacketReader(sess, media.CodecAudioUlaw),
			RTPPacketWriter: media.NewRTPPacketWriter(sess, media.CodecAudioUlaw),
		},
	}

	pb, err := dialog.PlaybackDTMFCreate(WithReplayKeys("*"))
	require.NoError(t, err)
	defer pb.Close()

	done := make(chan error, 1)
	var written int64
	go func() {
		var err error
		written, err = pb.PlayFile("testdata/files/demo-echodone.wav")
		done <- err
	}()

	waitPlaying(t, &pb.AudioPlaybackControl)

	// Replay with star key
	sendDTMFInjection(t, injector, &sess.Laddr, new(uint16), '*')
	select {
	case dtmf := <-pb.DTMF():
		assert.Equal(t, '*', dtmf)
	case <-time.After(3 * time.Second):
		t.Fatal("replay DTMF not detected")
	}
	assert.Equal(t, PlaybackStatePlaying, pb.State(), "replay key must not interrupt")

	// Interrupt with number key
	time.Sleep(300 * time.Millisecond)
	sendDTMFInjection(t, injector, &sess.Laddr, new(uint16), '5')

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(3 * time.Second):
		t.Fatal("play was not interrupted")
	}
	assert.Equal(t, '5', <-pb.DTMF())
	assert.Equal(t, PlaybackStateStopped, pb.State())
	// Interrupted well before file end (full pass writes more than 15000)
	assert.Less(t, written, int64(15000))

	// After stop, media reading can be taken over again after Close
	require.NoError(t, pb.Close())
}
