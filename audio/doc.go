// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

// Package audio provides the PCM, codec, tone and WAV building blocks used by
// diago's media pipelines.
//
// PCM data is 16-bit little-endian interleaved samples (S16LE), mono or
// stereo matching the negotiated codec. The encoder/decoder readers and
// writers translate between encoded RTP payloads (PCMU/PCMA/opus) and PCM,
// and are designed to sit inline in a pipeline.
//
// Tones are declarative segment lists (on/off with frequencies and volume)
// rendered by ToneReader at any sample rate, which makes them safe to re-render
// when a re-INVITE changes the negotiated codec. DTMF digits can be rendered
// as dual-frequency tones via ToneDTMFDigit.
//
// MonitorPCMReader/MonitorPCMWriter tap one pipeline direction, decode a copy
// of the encoded stream and write PCM into a writer (recording) with optional
// fail-open behavior, and MonitorPCMStereo interleaves both directions into a
// stereo WAV file with pause/resume.
//
// opus support is build-tag gated: without the with_opus_c tag the opus
// encoder/decoder return an error on Init and the media layer filters opus
// out of negotiation and answers.
package audio
