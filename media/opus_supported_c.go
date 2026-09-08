//go:build with_opus_c

package media

// OpusSupported reports whether the opus codec can actually be used.
const opusSupported = true

// OpusSupportedForNegotiation is the public gate for tests/docs.
func OpusSupportedForNegotiation() bool { return opusSupported }
