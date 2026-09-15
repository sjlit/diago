// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

// StopFunc stops a background media operation and reports any error observed
// while it was running or while shutting down. It is the unified stop handle
// returned by ListenBackground, Bridge.ProxyMediaControl,
// AudioRingtone.PlayBackground and the PauseAudioRead/PauseAudioWrite release
// functions. Call semantics follow the operation that returned it; the
// operations document whether the stop may be called more than once.
type StopFunc func() error
