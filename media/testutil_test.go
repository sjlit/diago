// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"testing"
	"time"
)

// recvWithin receives one value from ch, failing the test instead of hanging
// forever when the value never arrives. Test goroutines that feed a channel
// are easy to lose track of (an early return skips the send), and an
// unbounded receive then stalls the test until the global timeout without
// saying what it waited for.
func recvWithin[T any](t *testing.T, ch <-chan T, d time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("no value received after %s", d)
		var zero T
		return zero
	}
}
