// SPDX-License-Identifier: MPL-2.0
package diago

import (
	"bytes"
	"context"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/require"
)

type fakeBridgeDialog struct {
	id string
	dm *DialogMedia
}

func (f *fakeBridgeDialog) ID() string               { return f.id }
func (f *fakeBridgeDialog) Context() context.Context { return context.Background() }
func (f *fakeBridgeDialog) Hangup(ctx context.Context, opts ...SignalOption) error {
	return nil
}
func (f *fakeBridgeDialog) Media() *DialogMedia      { return f.dm }
func (f *fakeBridgeDialog) DialogSIP() *sipgo.Dialog { return nil }
func (f *fakeBridgeDialog) Close() error             { return nil }
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
		RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
		RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
	}
	return &fakeBridgeDialog{id: id, dm: dm}
}

func TestBridgeLifecycle_RemoveClose(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 99 // no auto proxy
	d1 := newBridgeTestDialog("a")
	d2 := newBridgeTestDialog("b")
	require.NoError(t, b.AddDialogSession(d1))
	require.NoError(t, b.AddDialogSession(d2))
	require.Len(t, b.GetDialogs(), 2)

	// Third add rejected atomically (no stale append).
	d3 := newBridgeTestDialog("c")
	require.Error(t, b.AddDialogSession(d3))
	require.Len(t, b.GetDialogs(), 2)

	// Remove one; originator falls back to remaining.
	require.NoError(t, b.RemoveDialogSession(d1))
	require.Len(t, b.GetDialogs(), 1)
	require.Equal(t, "b", b.Originator.ID())
	// Missing ID is no-op.
	require.NoError(t, b.RemoveDialogSession(d1))

	// Close idempotent; Add after Close rejected.
	require.NoError(t, b.Close())
	require.NoError(t, b.Close())
	require.Empty(t, b.GetDialogs())
	require.ErrorIs(t, b.AddDialogSession(newBridgeTestDialog("d")), ErrBridgeClosed)
}
