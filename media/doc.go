// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

// Package media implements the RTP/RTCP/SDP layer used by diago dialogs:
// session negotiation, secure RTP (SDES and DTLS-SRTP), packet reader/writer
// handles, jitter buffering and statistics.
//
// # Session lifecycle
//
// A MediaSession goes through a Config phase (fields may be set before Init)
// and an Active phase: after a completed offer/answer exchange the session is
// FROZEN and only IO is allowed. Any change (codecs, addresses, direction)
// requires Fork followed by installing the fork through diago's DialogMedia.
//
// Negotiation (RemoteSDP/LocalSDP/Finalize) must be funneled through a single
// goroutine. IO goes through the RTPPacketReader/RTPPacketWriter stable
// handles, which hot-swap their internals on media updates and carry the
// refcounted PauseRead/PauseWrite gates; callers resolve the current session
// at use time instead of capturing it.
//
// # RTCP
//
// RTCP uses a separate socket at RTP port + 1; rtcp-mux is not supported.
// RTPSession.MonitorBackground runs the RTCP reader/writer loops and reports
// quality via ReadStats/WriteStats and the OnReadRTCP/OnWriteRTCP hooks.
//
// # Logging and process-wide settings
//
// Every session logs through MediaSession.Logger (falling back to
// SetDefaultLogger/slog.Default), and RTPSession and the packet handles
// inherit it. RTPBufSize is process-wide: pools capture it at first use, so
// set it once at startup. RTPDebug/RTCPDebug (optionally with
// RTPDebugTracer) enable synchronous packet tracing.
package media
