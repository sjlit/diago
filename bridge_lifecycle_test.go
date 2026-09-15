// SPDX-License-Identifier: MPL-2.0
package diago

import (
	"bytes"
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/require"
)

type fakeBridgeDialog struct {
	id string
	dm *DialogMedia
}

func (f *fakeBridgeDialog) ID() string               { return f.id }
func (f *fakeBridgeDialog) Context() context.Context { return context.Background() }
func (f *fakeBridgeDialog) FromUser() string         { return "fake-from" }
func (f *fakeBridgeDialog) ToUser() string           { return "fake-to" }
func (f *fakeBridgeDialog) Transport() string        { return "UDP" }
func (f *fakeBridgeDialog) Hangup(ctx context.Context, opts ...SignalOption) error {
	return nil
}
func (f *fakeBridgeDialog) HangupAndClose(ctx context.Context, opts ...SignalOption) error {
	return f.Close()
}
func (f *fakeBridgeDialog) DialogMedia() *DialogMedia { return f.dm }
func (f *fakeBridgeDialog) DialogSIP() *sipgo.Dialog  { return nil }
func (f *fakeBridgeDialog) Close() error              { return nil }
func (f *fakeBridgeDialog) Do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return nil, nil
}

func newBridgeTestDialog(id string) DialogSession {
	dm := &DialogMedia{
		mediaSession: &media.MediaSession{
			Codecs: []media.Codec{media.CodecAudioAlaw},
		},
		audioReader:     bytes.NewBuffer(make([]byte, 0)),
		audioWriter:     bytes.NewBuffer(make([]byte, 0)),
		rtpPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
		rtpPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
	}
	return &fakeBridgeDialog{id: id, dm: dm}
}

// bridgeDialogs snapshots the bridge dialog list for assertions. GetDialogs
// was removed from the public API in v2 P5; tests stay white-box.
func bridgeDialogs(b *Bridge) []DialogSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.dialogs)
}

func TestBridgeLifecycle_RemoveClose(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 99 // no auto proxy
	d1 := newBridgeTestDialog("a")
	d2 := newBridgeTestDialog("b")
	require.NoError(t, b.AddDialogSession(d1))
	require.NoError(t, b.AddDialogSession(d2))
	require.Len(t, bridgeDialogs(b), 2)

	// Third add rejected atomically (no stale append).
	d3 := newBridgeTestDialog("c")
	require.Error(t, b.AddDialogSession(d3))
	require.Len(t, bridgeDialogs(b), 2)

	// Remove one; originator falls back to remaining.
	require.NoError(t, b.RemoveDialogSession(d1))
	require.Len(t, bridgeDialogs(b), 1)
	require.Equal(t, "b", b.Originator.ID())
	// Missing ID is no-op.
	require.NoError(t, b.RemoveDialogSession(d1))

	// Close idempotent; Add after Close rejected.
	require.NoError(t, b.Close())
	require.NoError(t, b.Close())
	require.Empty(t, bridgeDialogs(b))
	require.ErrorIs(t, b.AddDialogSession(newBridgeTestDialog("d")), ErrBridgeClosed)
}

// endlessReader produces PCM forever so the proxy loop keeps running until its
// pause-aware writer is interrupted.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// newBridgePausableDialog builds a dialog whose proxy writer is the real
// pause-aware RTP writer handle (no fake audioWriter), so bridge Close/Remove can
// interrupt the copy loop through the write gate.
func newBridgePausableDialog(t *testing.T, id string) DialogSession {
	t.Helper()

	msess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = msess.Close() })
	msess.SetRemoteAddr(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})

	writer := media.NewRTPPacketWriterSession(media.NewRTPSession(msess))
	writer.ClockDisable() // no 20ms pacing: the test only needs the pause gate

	dm := &DialogMedia{
		mediaSession:    msess,
		audioReader:     endlessReader{},
		rtpPacketWriter: writer,
		rtpPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioUlaw),
	}
	return &fakeBridgeDialog{id: id, dm: dm}
}

// Regression: ProxyMediaControl never claimed the proxy generation, so Close
// could not interrupt it (goroutine and writes kept running) and a second
// control start was accepted, running two proxies over the same media.
func TestBridgeProxyMediaControlClaimAndClose(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 99 // no auto proxy
	require.NoError(t, b.AddDialogSession(newBridgePausableDialog(t, "a")))
	require.NoError(t, b.AddDialogSession(newBridgePausableDialog(t, "b")))

	_, err := b.ProxyMediaControl()
	require.NoError(t, err)

	// A second control start must be rejected while the generation is claimed.
	_, err = b.ProxyMediaControl()
	require.Error(t, err)
	require.Contains(t, err.Error(), "already running")

	// Close must interrupt the claimed generation through the write gate and
	// wait for it, not time out after 5s.
	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not stop the ProxyMediaControl proxy")
	}
}

// A second mix must not start while one is already running: two generations
// would read the same streams concurrently.
func TestBridgeMixStartRequiresIdleState(t *testing.T) {
	b := NewBridgeMix()

	b.mixState = 1
	require.NoError(t, b.mixStart())
	require.Equal(t, 1, b.mixState)
	require.Nil(t, b.mixWG, "no second mix generation may start")

	b.mixState = 2
	require.NoError(t, b.mixStart())
	require.Equal(t, 2, b.mixState)
	require.Nil(t, b.mixWG)
}
