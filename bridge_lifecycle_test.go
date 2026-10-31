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
	dm *Media
}

func (f *fakeBridgeDialog) ID() string               { return f.id }
func (f *fakeBridgeDialog) Context() context.Context { return context.Background() }
func (f *fakeBridgeDialog) State() CallState         { return CallStateConfirmed }
func (f *fakeBridgeDialog) FromUser() string         { return "fake-from" }
func (f *fakeBridgeDialog) ToUser() string           { return "fake-to" }
func (f *fakeBridgeDialog) Transport() string        { return "UDP" }
func (f *fakeBridgeDialog) Hangup(ctx context.Context, opts ...HangupOption) error {
	return nil
}
func (f *fakeBridgeDialog) Media() *Media      { return f.dm }
func (f *fakeBridgeDialog) SIP() *sipgo.Dialog { return nil }
func (f *fakeBridgeDialog) Close() error       { return nil }
func (f *fakeBridgeDialog) Do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return nil, nil
}

func newBridgeTestDialog(id string) Call {
	dm := &Media{
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
func bridgeDialogs(b *Bridge) []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.dialogs)
}

func TestBridgeLifecycle_RemoveClose(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 99 // no auto proxy
	d1 := newBridgeTestDialog("a")
	d2 := newBridgeTestDialog("b")
	require.NoError(t, b.Add(d1))
	require.NoError(t, b.Add(d2))
	require.Len(t, bridgeDialogs(b), 2)

	// Third add rejected atomically (no stale append).
	d3 := newBridgeTestDialog("c")
	require.Error(t, b.Add(d3))
	require.Len(t, bridgeDialogs(b), 2)

	// Remove one; originator falls back to remaining.
	require.NoError(t, b.Remove(d1))
	require.Len(t, bridgeDialogs(b), 1)
	require.Equal(t, "b", b.Originator.ID())
	// Missing ID is no-op.
	require.NoError(t, b.Remove(d1))

	// Close idempotent; Add after Close rejected.
	require.NoError(t, b.Close())
	require.NoError(t, b.Close())
	require.Empty(t, bridgeDialogs(b))
	require.ErrorIs(t, b.Add(newBridgeTestDialog("d")), ErrBridgeClosed)
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
func newBridgePausableDialog(t *testing.T, id string) Call {
	t.Helper()

	msess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = msess.Close() })
	msess.SetRemoteAddr(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})

	writer := media.NewRTPPacketWriterSession(media.NewRTPSession(msess))
	_ = writer.Close() // no 20ms pacing: the test only needs the pause gate

	dm := &Media{
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
	require.NoError(t, b.Add(newBridgePausableDialog(t, "a")))
	require.NoError(t, b.Add(newBridgePausableDialog(t, "b")))

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

// TestBridgeZeroValueUsable guards the zero-value contract: a Bridge built as
// a literal must lazily initialize instead of panicking on a nil logger or
// proxy channel.
func TestBridgeZeroValueUsable(t *testing.T) {
	b := &Bridge{}
	require.Error(t, b.ProxyMedia(), "empty bridge must report the precondition, not panic")
	require.NotNil(t, b.log)
	require.NotNil(t, b.proxyDone)
	require.Equal(t, 2, b.WaitDialogsNum)

	require.Error(t, b.Add(nil))
	require.Error(t, b.Remove(nil))

	// A single dialog must not start the proxy (WaitDialogsNum defaults to 2).
	d := &fakeBridgeDialog{id: "zero-value", dm: &Media{}}
	require.NoError(t, b.Add(d))

	require.NoError(t, b.Close())
}

// TestBridgeMixZeroValueUsable is the BridgeMix counterpart.
func TestBridgeMixZeroValueUsable(t *testing.T) {
	b := &BridgeMix{}
	require.NotPanics(t, func() { _ = b.String() })
	require.NotNil(t, b.log)
	require.Error(t, b.Add(nil))
	require.Error(t, b.Remove(nil))
	require.NoError(t, b.mixStart())
}

func TestBridgeMixClose(t *testing.T) {
	b := NewBridgeMix()
	require.NoError(t, b.Close())
	require.NoError(t, b.Close())

	// Add/Remove are rejected once the mixer is closed, before any dialog
	// state or media is touched.
	require.ErrorIs(t, b.Add(newBridgeTestDialog("late")), ErrBridgeClosed)
	require.ErrorIs(t, b.Remove(newBridgeTestDialog("late")), ErrBridgeClosed)
}
