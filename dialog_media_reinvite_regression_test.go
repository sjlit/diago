// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogMediaSessionReplacementDTMF is the regression test for the
// snapshot-vs-replace design defect: a DTMF reader created before a media
// update (re-INVITE) must keep working after the update. The reader resolves
// the current media session at use time (docs/contracts.md §4) and the stable
// reader/writer handles are hot-swapped, never replaced.
func TestDialogMediaSessionReplacementDTMF(t *testing.T) {
	d := newTestDialogMedia(t)

	r, err := d.AudioReaderDTMF()
	require.NoError(t, err)

	oldSess := d.currentMediaSession()
	require.NotNil(t, oldSess)
	oldReader := d.RTPPacketReader
	oldWriter := d.RTPPacketWriter

	// Baseline: deadline control works through the resolver
	buf := make([]byte, 1600)
	_, err = r.readDeadline(buf, 5*time.Millisecond)
	require.True(t, errors.Is(err, os.ErrDeadlineExceeded), "expected deadline before replace, got %v", err)

	// Simulate a media update the way a re-INVITE does: fork, target it,
	// install through replaceRTPSessionUnsafe (under d.mu).
	msess := oldSess.Fork()
	msess.SetRemoteAddr(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 41000})

	d.mu.Lock()
	err = d.replaceRTPSessionUnsafe(msess)
	d.mu.Unlock()
	require.NoError(t, err)
	t.Cleanup(func() {
		d.Close()
	})

	// Active session is the new one
	assert.Same(t, msess, d.currentMediaSession())
	assert.NotSame(t, oldSess, d.currentMediaSession())

	// Stable handles kept their identity (hot-swapped, not replaced)
	assert.Same(t, oldReader, d.RTPPacketReader, "RTPPacketReader must be a stable handle")
	assert.Same(t, oldWriter, d.RTPPacketWriter, "RTPPacketWriter must be a stable handle")

	// The pre-existing DTMF reader still exercises deadline control through
	// the new session: the deadline lands on the conn the stable reader reads.
	_, err = r.readDeadline(buf, 5*time.Millisecond)
	assert.True(t, errors.Is(err, os.ErrDeadlineExceeded), "expected deadline after replace, got %v", err)
}
