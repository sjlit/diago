// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import "errors"

// Lifecycle sentinels for dialog media operations. All media entry points
// (audio readers/writers, playback factories, Listen*, Echo) return errors
// matching these instead of panicking or returning bare errors when the dialog
// is in the corresponding lifecycle state. See docs/contracts.md §8.
var (
	// ErrDialogNotAnswered is returned when an operation requires an answered
	// dialog with negotiated media, but no active media session exists yet
	// (dialog not answered) or no invite response was received.
	ErrDialogNotAnswered = errors.New("dialog session not answered")

	// ErrDialogClosed is returned by media operations after the dialog media
	// was closed locally with Close (or the dialog ended and the framework
	// closed it).
	ErrDialogClosed = errors.New("dialog media closed")
)
