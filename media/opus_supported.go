// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

//go:build !with_opus_c

package media

// OpusSupported reports whether the opus codec can actually be used.
// Without the with_opus_c tag the audio package only contains a placeholder
// returning errors, so opus must be filtered from offers/answers (see #39).
const opusSupported = false

// OpusSupportedForNegotiation is the public gate for tests/docs.
func OpusSupportedForNegotiation() bool { return opusSupported }
