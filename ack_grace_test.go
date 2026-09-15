// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWaitAckOrGrace(t *testing.T) {
	t.Run("AlreadyClosedWinsImmediately", func(t *testing.T) {
		ch := make(chan struct{})
		close(ch)
		start := time.Now()
		assert.True(t, waitAckOrGrace(ch, time.Hour))
		assert.Less(t, time.Since(start), time.Second)
	})

	t.Run("LateAckWithinGrace", func(t *testing.T) {
		ch := make(chan struct{})
		go func() {
			time.Sleep(10 * time.Millisecond)
			close(ch)
		}()
		assert.True(t, waitAckOrGrace(ch, time.Second))
	})

	t.Run("NoAckBeyondGrace", func(t *testing.T) {
		ch := make(chan struct{})
		start := time.Now()
		assert.False(t, waitAckOrGrace(ch, 20*time.Millisecond))
		assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	})
}

func TestIsAckWaitRaceError(t *testing.T) {
	assert.False(t, isAckWaitRaceError(nil))
	assert.True(t, isAckWaitRaceError(errors.New("No ACK received")))
	assert.True(t, isAckWaitRaceError(errors.New("wrap: No ACK received: more")))
	assert.False(t, isAckWaitRaceError(errors.New("transport closed")))
}
