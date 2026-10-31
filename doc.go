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
// Server dialogs are delivered to a CallHandler:
//   - Returning before a final response was sent declines the pending INVITE
//     (nil -> 480, or the mapped final response for an error:
//     ErrDecline -> 603, ErrTemporarilyUnavailable -> 480, ErrBusy -> 486,
//     ErrNotFound -> 404, *ResponseError -> custom, anything else -> 500);
//     wrap ErrResponseSent when the handler already wrote the final response.
//   - Answering and returning keeps the call alive: the framework owns the
//     dialog until the peer ends it, Close runs or the handler hangs it
//     up. Once a 2xx was sent, teardown is always by BYE.
//
// Inbound dialogs are delivered to a CallHandler as *InboundCall; outbound
// calls are created with Dial("sip:user@host") (or NewCall+Invite for full
// control) and are *OutboundCall. Hangup ends the signaling; Close releases
// local resources. The framework closes a dialog when it ends.
//
// # Media model
//
// Media is the single owner of the active media session. Components
// resolve the session, reader or writer at use time through Media (or
// its stable RTPPacketReader/RTPPacketWriter handles) instead of capturing
// pointers: a re-INVITE hot-swaps the internals without replacing the
// handles. PauseAudioRead/PauseAudioWrite are refcounted gates used to stop
// or throttle a component without disturbing others.
//
// Construction-time media settings live in MediaConfig: codecs, SRTP/DTLS,
// RTP bind/advertise addresses, port range, SDP name, the optional Logger
// (media-pipeline and dialog-component logs) and the playback HTTPClient.
// MediaConfig.Debug (optionally with MediaConfig.Tracer) enables per-dialog
// RTP/RTCP tracing; DTLS handshake logging is media.DTLSConfig.Debug. There
// is no process-wide mutable media configuration.
//
// Transports can carry a different, complete configuration: register one with
// Config.MediaProfiles and select it with Transport.Profile. Profiles never merge
// field-by-field; only the transport-derived bind/advertise addresses fill
// fields a profile leaves nil.
//
// # Options
//
// Per-call behavior is configured with the With* option constructors. Every
// API takes the option interface for its operation (ResponseOption,
// AnswerOption, HangupOption, InviteOption, DialOption, RegisterOption, ...)
// and each constructor implements only the interfaces it is valid for:
// passing an option an API does not consume is a compile error.
//
// # Concurrency and contexts
//
// Dialog signaling methods are safe for concurrent use unless documented
// otherwise. Media entry points return ErrDialogNotAnswered/ErrDialogClosed
// before media exists or after teardown.
//
// Most signaling methods honor ctx while sending. Answer, AnswerLate and
// RespondSDP return once the 2xx was handed to the transaction layer;
// retransmission until the ACK and the ACK wait continue in the background
// (RFC 3261 §13.3.1.4). Call InboundCall.WaitAck to wait for the ACK
// to be processed (media finalized), with AckReceived/AckErr for the raw
// outcome.
//
// # Lifecycle
//
// The UserAgent is optional: without Config.UserAgent, New creates a private
// one and Close closes it. A UserAgent, Server or Client supplied through
// Config stays caller-owned and is not closed.
// Close hangs up and closes every tracked dialog, unregisters and stops the
// registrations created by Register, stops the listeners started by Serve and
// then closes the private UserAgent. In-flight handlers keep running
// concurrently with the dialog teardown (the framework closes each dialog when
// it ends). Registrations created with WithRegistrationDetached are
// caller-owned: stop them with Registration.Close (or cancel their context).
//
// The sipgo types in public signatures are aliased in aliases.go (URI, Header,
// Request, Response, ...), so applications can build on diago without
// importing sipgo; escape hatches stay explicit (Agent.Server, Agent.UA,
// call.SIP, Config.UserAgent, Config.Server, Config.Client). API conventions are
// documented in API.md. The media and audio packages belong to this module and
// are imported explicitly; only sipgo is aliased.
package diago
