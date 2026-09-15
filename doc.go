// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

// Package diago is a SIP user agent library for building B2BUA and media
// server applications on top of sipgo.
//
// It provides:
//   - UAC and UAS dialog handling (Invite/Answer/Ack/Hangup, re-INVITE, REFER
//     with NOTIFY tracking, hold/unhold, digest authentication).
//   - Media sessions: RTP/RTCP with PCMU/PCMA/opus/telephone-event, SDES and
//     DTLS-SRTP, RTP source learning for NAT, RTP statistics, jitter buffer,
//     direction-aware send blocking.
//   - Dialog-level audio components: playback (PCM/WAV/URL, with DTMF
//     control), stereo recording, tone/MoH, DTMF send/receive (RFC 4733 and
//     SIP INFO relay), PCM monitors.
//   - Bridging: two-party proxy (Bridge) and N-party mixer (BridgeMix).
//   - REGISTER transactions with retry/expiry supervision.
//
// # Dialog model
//
// Server dialogs are delivered to a DialogHandler:
//   - Returning nil hangs up the call (BYE once confirmed, otherwise a 480
//     decline).
//   - Returning an error maps it to a final response (ErrDecline/ErrBusy/
//     ErrNotFound/*ResponseError); wrap ErrResponseSent when the handler
//     already wrote the final response.
//
// A handler that wants the call alive must block until it ends.
//
// Client dialogs start with NewDialog+Invite or the Dial/DialBridge helpers.
// HangupAndClose is the safe one-call teardown for owners.
//
// # Media model
//
// DialogMedia is the single owner of the active media session. Components
// resolve the session, reader or writer at use time through DialogMedia (or
// its stable RTPPacketReader/RTPPacketWriter handles) instead of capturing
// pointers: a re-INVITE hot-swaps the internals without replacing the
// handles. PauseAudioRead/PauseAudioWrite are refcounted gates used to stop
// or throttle a component without disturbing others.
//
// Construction-time media settings live in MediaConfig: codecs, SRTP/DTLS,
// RTP bind/advertise addresses, port range, SDP name and the optional Logger.
// MediaConfig.Logger (defaulting to the WithLogger logger) receives
// media-pipeline and dialog-component logs.
//
// # Concurrency and contexts
//
// Dialog signaling methods are safe for concurrent use unless documented
// otherwise. Media entry points return ErrDialogNotAnswered/ErrDialogClosed
// before media exists or after teardown.
//
// Most signaling methods honor ctx while sending, but the 2xx ACK wait of
// Answer, AnswerLate and RespondSDP cannot be interrupted: the response is
// retransmitted until the ACK arrives or 64*T1 elapses (RFC 3261 §13.3.1.4).
//
// # Lifecycle
//
// Diago takes the sipgo UserAgent, Server and Client by reference; the UserAgent
// stays caller-owned and is not closed by Shutdown. Shutdown hangs up and
// closes every tracked dialog, then stops the listeners started by Serve.
// In-flight handlers keep running concurrently with the dialog teardown.
// Register loops are caller-owned goroutines: cancel their context around
// Shutdown.
package diago
