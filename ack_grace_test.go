// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
)

func TestWaitAckOrGrace(t *testing.T) {
	ctx := context.Background()

	t.Run("already closed wins immediately", func(t *testing.T) {
		ch := make(chan struct{})
		close(ch)
		start := time.Now()
		assert.True(t, waitAckOrGrace(ctx, ch, time.Hour))
		assert.Less(t, time.Since(start), time.Second)
	})

	t.Run("late ack within grace", func(t *testing.T) {
		ch := make(chan struct{})
		go func() {
			// ACK arrives after the grace window started but before it ends.
			time.Sleep(10 * time.Millisecond)
			close(ch)
		}()
		assert.True(t, waitAckOrGrace(ctx, ch, time.Second))
	})

	t.Run("no ack beyond grace", func(t *testing.T) {
		ch := make(chan struct{})
		start := time.Now()
		assert.False(t, waitAckOrGrace(ctx, ch, 20*time.Millisecond))
		assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	})

	t.Run("cancelled context", func(t *testing.T) {
		ch := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		start := time.Now()
		assert.False(t, waitAckOrGrace(cctx, ch, time.Hour))
		assert.Less(t, time.Since(start), time.Second)
	})

	t.Run("ack racing cancellation still wins", func(t *testing.T) {
		ch := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		close(ch)
		cancel()
		assert.True(t, waitAckOrGrace(cctx, ch, time.Hour))
	})
}

func TestIsAckWaitRaceError(t *testing.T) {
	assert.False(t, isAckWaitRaceError(nil))
	assert.True(t, isAckWaitRaceError(errors.New("No ACK received")))
	assert.True(t, isAckWaitRaceError(errors.New("wrap: No ACK received: more")))
	assert.False(t, isAckWaitRaceError(errors.New("transport closed")))
}

func TestAckWaitFailure(t *testing.T) {
	base := errors.New("No ACK received")

	t.Run("ended dialog maps to sentinel", func(t *testing.T) {
		err := ackWaitFailure(sip.DialogStateEnded, base)
		assert.ErrorIs(t, err, ErrDialogEnded)
		assert.ErrorIs(t, err, base)
	})

	t.Run("established dialog keeps error", func(t *testing.T) {
		err := ackWaitFailure(sip.DialogStateEstablished, base)
		assert.NotErrorIs(t, err, ErrDialogEnded)
		assert.ErrorIs(t, err, base)
	})
}
