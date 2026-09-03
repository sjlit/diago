# Diago Contracts

This document is the authoritative contract for the two areas where informal
conventions have historically caused defects: **media state ownership** and
**dialog lifecycle**. If code and this document disagree, one of them is a bug —
open an issue referencing the section number.

Table of contents:

1. [Media state ownership](#1-media-state-ownership)
2. [MediaSession phases](#2-mediasession-phases)
3. [Fork contract](#3-fork-contract)
4. [Stable handles vs forbidden snapshots](#4-stable-handles-vs-forbidden-snapshots)
5. [Concurrency contract](#5-concurrency-contract)
6. [Dialog lifecycle](#6-dialog-lifecycle)
7. [Close vs Hangup](#7-close-vs-hangup)
8. [Error contract](#8-error-contract)
9. [Cancellation migration table](#9-cancellation-migration-table)
10. [API surface conventions](#10-api-surface-conventions)
11. [Shutdown and resource ownership](#11-shutdown-and-resource-ownership)
12. [Recording taps](#12-recording-taps)
13. [Music on Hold](#13-music-on-hold)

---

## 1. Media state ownership

Ownership is layered, top-down:

| Object | Owner | Notes |
|---|---|---|
| `*media.MediaSession` (the **active** one) | `DialogMedia` | Exactly one active session per dialog at any time. Guarded by `DialogMedia.mu`. |
| `*media.RTPSession` | `DialogMedia` | Bound 1:1 to the active session. Replaced together with it. |
| `*media.RTPPacketReader` / `*media.RTPPacketWriter` | `DialogMedia` | **Stable handles**: created once, never replaced; their internals are hot-swapped on media updates (see §4). |
| `audioReader` / `audioWriter` chain | `DialogMedia` | Single slot each, set via options or `SetAudioReader`/`SetAudioWriter`. |
| Forks produced by `MediaSession.Fork()` | The caller, **transiently** | A fork is a *draft* (see §3). It must not outlive the install call. |

Users of the library interact with media through `DialogMedia()` on a dialog
session. `DialogMedia` is the only component allowed to store, replace, or
install media sessions. User code must treat every `*media.MediaSession` it can
observe as read-only unless it created that session itself and has not handed it
to diago yet (see `WithMediaSession`).

## 2. MediaSession phases

A `MediaSession` moves through these phases:

```
Config ──Init()──> Listening ──first LocalSDP/RemoteSDP──> Active ──replace/Close──> Superseded/Closed
```

| Phase | Allowed operations |
|---|---|
| **Config** (before `Init`) | Set fields (`Codecs`, `Mode`, `Laddr`, security config, ...). |
| **Listening** (after `Init`, before negotiation) | Field mutation still allowed, but listeners are bound: changing `Laddr` requires `Init` again. |
| **Negotiating** (during `LocalSDP`/`RemoteSDP`) | Single-threaded by contract (see §5). Do not touch the session from other goroutines. |
| **Active** (after a completed offer/answer exchange) | **Frozen.** Only IO (`ReadRTP`, `WriteRTP`, `ReadRTCP`, `WriteRTCP`) and read-only queries (`CommonCodecs`, `DTMFCodec`). Any change (codecs, direction, addresses) requires `Fork()` + install via `DialogMedia`. `StopRTP`/`StartRTP` are deprecated legacy: the conn deadline is a stack-internal detail, use the pause gates instead (§5). |

Two non-obvious consequences of the phase model:

- `LocalSDP()` is **not a pure getter**. It allocates the `o=` session id on
  first call, increments the version on every subsequent call, and on SRTP
  offers it generates the local crypto context. Failures while generating the
  SDES attribute are logged and the SDP continues in plaintext — check logs when
  securing calls. Call it once per offer, as part of the negotiation flow.
- `SetRemoteAddr` writes `Raddr` and derives `rtcpRaddr` as *RTP port + 1*.
  The +1 pairing is a current limitation (no `a=rtcp-mux` support).

## 3. Fork contract

`Fork()` creates a **draft** copy that shares the underlying UDP connections
with its parent. Drafts are only useful as arguments to the install path of
`DialogMedia` (`sdpUpdateUnsafe` / `mediaUpdateUnsafe` → `replaceRTPSessionUnsafe`):
apply the remote SDP on the draft, then install. Installing is the only way a
draft becomes active; installing also swaps the RTP session (old monitor is
stopped, stats are carried over via `RTPSession.Fork`) and hot-swaps the stable
reader/writer handles.

Exact field semantics of `Fork()`:

| Category | Fields |
|---|---|
| **Copied** | `Laddr` (struct copy), `ExternalIP`, `Codecs` (slice clone), `Mode` (configured preference), `RTPNAT`, `sdp` (only set by `InitWithSDP`), `sessionID`, `sessionVersion`, `SecureRTP`, `SRTPAlg`, `remoteProto`, `srtpRemoteTag`, `DTLSConf` |
| **Shared with parent** | `rtpConn`, `rtcpConn` (same UDP sockets) |
| **Reset / empty** | `Raddr`, `rtcpRaddr` (set again by `RemoteSDP`), negotiated `mode`, `filterCodecs`, `localCtxSRTP`, `remoteCtxSRTP`, `dtlsConn`, `onFinalize`, `writeRTPBuf`, `ReadRTPFromAddr` |
| **Reset — known behavior** | `learnedRTPFrom`, `learnedRTCPFrom` (NAT-learned source addresses). After a re-INVITE the session re-learns the source; until the first packet arrives, writes go to the SDP address. |

Rules:

1. Mutating the **active** session in place is forbidden. Mutate a fork, then
   install.
2. Only `DialogMedia` installs forks. Code outside `dialog_media.go` must not
   assign `mediaSession`/`rtpSession`.
3. Known violation kept for compatibility: the *originator* (codec filter)
   path in `DialogClientSession.invite` rewrites `sess.Codecs` in place on the
   session it is about to install. It is single-threaded setup-phase code and is
   scheduled to move onto the fork/install model.

## 4. Stable handles vs forbidden snapshots

`DialogMedia.RTPPacketReader` and `DialogMedia.RTPPacketWriter` are **stable
handles**: their pointers are created once when media is set up and never
replaced. On every media update (re-INVITE, hold, early-media upgrade) their
inner reader/writer are hot-swapped via `UpdateRTPSession`. Code built on top of
them keeps working across media updates.

**Contract: components must not capture a `*media.MediaSession` pointer for
later use.** Sessions get replaced; captured pointers go stale and then target a
superseded session (dead direction control, wrong DTMF codec).

Historical offenders, now fixed by resolving the current session at use time
via `DialogMedia`, or by routing through the stable-handle gates:

- `DTMFReader`, `DTMFWriter` (deadline/interrupt control via the read gate)
- `AudioPlaybackDTMF` (close via ctx-driven gate interrupt)
- `AudioRingtone` (stop via the write gate)
- `bridgePCMStream` (mix-loop deadline control)
- `ListenBackground`/`ListenContext`/`ListenUntil`/`StopRTP`/`StartRTP` on
  `DialogMedia`

If you add a new component that needs media-session services (deadlines,
codecs), take a `*DialogMedia` (or call an accessor at use time), never a
`*MediaSession`.

## 5. Concurrency contract

`DialogMedia.mu` guards all mutable media state: `mediaSession`, `rtpSession`,
the `RTPPacketReader`/`RTPPacketWriter` fields, `audioReader`/`audioWriter`,
`remoteContactTarget`, the `onClose`/`onMediaUpdate`/`onReferNotify` hooks, and
`closed`.

- **Accessor methods lock.** `MediaSession()`, `RTPSession()`,
  `AudioReader()`/`AudioWriter()`, `RemoteContact()` are safe from any
  goroutine.
- **Option functions run under the lock** (they are invoked inside
  `AudioReader()`/`AudioWriter()`). They must not block, must not call back into
  locking `DialogMedia` methods, and must be used only through those two entry
  points.
- **Setup phase is single-threaded by contract.** From `Invite`/`Answer`/
  `ProgressMedia`/`AnswerLate` until they return, the dialog is owned by the
  calling goroutine. The library reads session fields without locking on these
  paths; no other goroutine may touch the dialog during setup.
- **One reader, one writer per direction.** `MediaSession` IO is funneled
  through the stable handles, which serialize access. Concurrent `WriteRTP` on
  the same `MediaSession` from two components is not supported (single shared
  marshal buffer).
- Internal long-running loops (`Listen*`, DTMF read loop, bridge mix loop)
  resolve the current session through `DialogMedia` at use time, so they survive
  media updates.

### Cancellation and pause (the gates)

Cancellation and pausing go through the stable handles, never through conn
deadlines. The conn deadline is a transient interrupt signal owned by the
handle; a *pause* is refcounted state resolved at use time.

| Primitive | Semantics |
|---|---|
| `DialogMedia.PauseAudioRead()` / `RTPPacketReader.PauseRead()` | Refcounted pause: reads return `media.ErrReadPaused` until every release is called. Concurrent pausers can not resume each other. |
| `DialogMedia.PauseAudioWrite()` / `RTPPacketWriter.PauseWrite()` | Refcounted write pause: writes return `media.ErrWritePaused`. In-flight write finishes first (bounded by one packet interval). |
| `RTPPacketReader.ArmReadInterrupt(ctx)` | Interrupts the in-flight read once when ctx is done; restore is owned by the returned disarm. |
| `RTPPacketReader.ReadContext(ctx, buf)` | Arm + read + restore: returns `ctx.Err()` on cancellation, reader stays usable. |
| `media.CopyContext` / `CopyWithBufContext` | Copy loops checking ctx between reads/writes. |

Rules:

1. Never expire conn deadlines from component code (`StopRTP`/`StartRTP` are
   deprecated for exactly this reason) — a durable deadline is global state on
   the shared conn and reintroduces the fight this mechanism replaces.
2. Pause-aware wrapped readers (ex. `RTPJitterBuffer`) receive the pause as a
   signal channel; their internal pumps are never interrupted — a conn poke
   would be terminal for them. Pause semantics stay consumer-facing: while a
   jitter buffer is installed, the pump keeps buffering (bounded by the jitter
   window) and playout timing gaps surface as loss.
3. Consumers treat `ErrReadPaused`/`ErrWritePaused` as "paused, retry or exit";
   treat `ctx.Err()` as cancellation.

## 6. Dialog lifecycle

### State machine

Dialog states come from `sipgo`/`sip` (`sip.DialogState*`):

```
           INVITE received / sent
(uninit=0) ─────────────────────> Established ──ACK──> Confirmed ──BYE/timeout──> Ended
                 (200 OK sent / received, waiting ACK)
```

`Ended` cancels the dialog context; every media read/write fails after that.

### Handler-return semantics (server side)

**When your `ServeDialogFunc` returns, the framework tears the call down**: it
attempts `Hangup` (BYE once confirmed, otherwise a 480 decline to the INVITE)
with a hardcoded 10-second timeout, then closes the dialog and its media. This
is intentional and documented behavior:

- A handler that wants the call alive **must block** until the call is over
  (read media, wait on `dialog.Context().Done()`, etc.) or call `Hangup` itself
  before returning.
- Goroutines spawned inside the handler must not rely on the dialog after the
  handler returns.
- After the remote sends BYE, the framework closes the dialog media and the
  dialog context is canceled; blocking media reads wake up with an error.

### Ownership responsibility

| Dialog kind | Signaling teardown | Local cleanup |
|---|---|---|
| **Server** (`DialogServerSession`) | Framework: auto-hangup when handler returns (or earlier if the user calls `Hangup`). | Framework: closes dialog + media when the handler returns. |
| **Client** (`DialogClientSession`) | **User.** The library never sends BYE for you (the `Diago.Invite`/`InviteBridge` helpers do it only on their own error paths). | **User.** Call `Close()` when done; it is idempotent. |

Warning specific to client dialogs: `Close()` does **not** send BYE. Closing
without hanging up leaves the remote leg up until it times out. Always `Hangup`
first if signaling teardown is wanted.

### Cancelling an unanswered outgoing call

`ClientHangup` before any response arrives cannot send BYE (there is no dialog
yet). To abort an outgoing call in progress, cancel the context passed to
`Invite`; sipgo sends CANCEL. `Hangup` works once a response (even provisional)
has been received (early dialog).

## 7. Close vs Hangup

`Close()` is **local cleanup only** (idempotent): closes the media stack and
runs sipgo dialog cleanup hooks. It sends no SIP message and changes no dialog
state. `Hangup(ctx)` is **signaling**. Actual behavior matrix:

| Dialog state | Client `Hangup` | Server `Hangup` | Either `Close` |
|---|---|---|---|
| No response yet (client) / not answered (server) | Error (`ErrDialogNotAnswered`) — cancel via `Invite` ctx instead | Responds **480** to the INVITE, returns nil | Local cleanup, no signaling |
| Early (provisional received, server sent 1xx) | Sends BYE on the early dialog | Responds 480 | Local cleanup |
| Established (200 sent/received, awaiting ACK) | Sends BYE | Sends BYE | Local cleanup |
| Confirmed | Sends BYE, waits 200 | Sends BYE, waits 200 | Local cleanup |
| Ended (BYE already exchanged) | Returns nil silently | Sends BYE (returns nil via sipgo) | Local cleanup, no-op |
| After `Close()` | Undefined — do not use | Undefined — do not use | No-op (idempotent) |

Note: server `Hangup` on a not-answered dialog returns **nil** after declining
with 480 — declining succeeded; there is no error to report.

`Media()` and every media method (`AudioReader`, playback factories, `Listen*`,
`Echo`) return `ErrDialogClosed` after `Close()` and `ErrDialogNotAnswered`
before media is set up (see §8).

## 8. Error contract

Sentinels (usable with `errors.Is`):

| Sentinel | Meaning |
|---|---|
| `diago.ErrDialogNotAnswered` | The operation requires an answered dialog with negotiated media (no active media session, or no invite response yet). |
| `diago.ErrDialogClosed` | The dialog media was already closed locally. |
| `media.ErrReadPaused` | The reader is paused (`PauseAudioRead`) or the read was interrupted; retry or exit. |
| `media.ErrWritePaused` | The writer is paused (`PauseAudioWrite`); retry or exit. |
| `diago.ErrClientEarlyMedia` | `Invite` stopped on a 183 with SDP (early media negotiated; continue with `WaitAnswer`). |
| `diago.ErrPlaybackStopped` / `ErrPlaybackReplayed` / `ErrSourceNotReplayable` | Playback control results (also match `io.EOF` for backward compatibility — prefer the sentinels). |
| `diago.ErrDigestAuthNoChallenge` / `ErrDigestAuthBadCreds` | Server-side digest auth failures. |

Behavior of media entry points on a dialog that was never answered or was
closed locally (all return wrapped sentinels, never panic):

| Method | Not answered | Closed |
|---|---|---|
| `Echo` | `ErrDialogNotAnswered` | `ErrDialogClosed` |
| `AudioReader` / `AudioWriter` (+ DTMF variants) | `ErrDialogNotAnswered` | `ErrDialogClosed` |
| `PlaybackCreate` / `PlaybackControlCreate` / `PlaybackDTMFCreate` | `ErrDialogNotAnswered` | `ErrDialogClosed` |
| `Listen` / `ListenBackground` / `ListenContext` / `ListenUntil` | `ErrDialogNotAnswered` | `ErrDialogClosed` |
| `ReInvite` / `Hold` / `Unhold` (client & server) | `ErrDialogNotAnswered` | network-level error (send failure) |
| `ClientHangup` without any response | `ErrDialogNotAnswered` | network-level error |

Everything else that fails mid-call returns the underlying transport/SIP error;
the sentinels above are reserved for the lifecycle states in the table.

## 9. Cancellation migration table

The deadline-based pause/stop APIs are superseded by the gates (§5). Old APIs
remain functional for compatibility but must not be mixed with the gates.

| Legacy (deadline based) | Replacement |
|---|---|
| `DialogMedia.StopRTP(rw, dur)` / `StartRTP(rw)` | `PauseAudioRead()` / `PauseAudioWrite()` + release |
| `media.MediaSession.StopRTP` / `StartRTP` | Same — via the dialog handles only |
| `DialogMedia.ListenUntil(dur)` | `ListenContext(ctx)` with a context deadline |
| `DialogMedia.ListenContext` (deadline-poke impl) | Same name; now gate-based and returns `ctx.Err()` |
| `DTMFReader.Listen(onDTMF, dur)` | `DTMFReader.ListenContext(ctx, onDTMF)` |
| `AudioPlaybackDTMF` close via conn deadlines | `Close()` — now interrupts through the reader gate |
| `Bridge.ProxyMediaControl` stop via write deadlines | Same name — stop now goes through the write gate |
| `BridgeMix` stop via conn deadlines | Same name — mixStop/mixStopWait use the read gate |
| `media.Copy` / `CopyWithBuf` in cancellable flows | `media.CopyContext` / `CopyWithBufContext` |
| `AudioPlayback.Play/PlayFile/PlayURL` | `PlayContext` / `PlayFileContext` / `PlayURLContext` (also on control and DTMF variants) |

## 10. API surface conventions

### One per-call option style

`SignalOption` is the single functional-option style for per-call customization.
It is accepted by every signaling method (server progress/answer, client
invite/bye/refer-side requests, REGISTER) and by `NewDialog`. Legacy option
structs survive only as migration input: `InviteOptions`, `NewDialogOptions`
and `InviteClientOptions` carry an `Options()` converter and are deprecated;
`ProgressMediaOptions` and `AnswerOptions` are consumed only through their
deprecated wrapper methods, which convert inline. `ReferClientOptions`/
`ReferServerOptions` are NOT deprecated — `Refer` has no SignalOption variant
and `ReferOptions` is the granular REFER API. `RegisterOptions` stays the
primary Register API and provides `Options()` for the signal-representable
fields (credentials, Contact, Headers).

### Single-execution guarantee

Every public API executes each user-supplied `SignalOption` func **exactly
once**. Multi-stage helpers (`Diago.Invite`, `Diago.InviteBridge`) compute the
`SignalParams` once and pass them through internal params-aware paths
(`newDialogWithParams`, `inviteWithParams`). Options with side effects are safe
to pass. Tests in `diago_test.go` lock this guarantee.

### Option scopes and per-method honors

`SignalParams` is grouped by concern: **msg** (Headers, Contact, Body,
MutateRequest, MutateResponse — outgoing message shaping), **media** (Codecs,
RTPNAT, MediaBindIP, MediaExternalIP, MediaDTLSConf, MediaSession — consumed
only via `signalMediaConfig` and the media-install sites), **dialog**
(Transport, TransportID, Originator, Username, Password, EarlyMediaDetect,
OnResponse, OnMediaUpdate, OnRefer). Fields irrelevant to the called method are
ignored; each method's godoc states which groups it honors. Credentials passed
via `WithAuthCredentials` are honored by `Invite` and by REGISTER
(`Register`/`Unregister`/`Qualify`), falling back to `RegisterOptions`.

### Getter-options

`WithAudioReaderMediaProps` / `WithAudioWriterMediaProps` are the only
output-options: they fill the caller's `MediaProps` with the negotiated codec
and addresses. Every other option is input-only.

### Global mutable variables (inventory, see recommendation 4)

Instance-scoped via `MediaConfig` (zero value inherits the package global):
`RTPPortStart`/`RTPPortEnd` (local RTP port range, fed into the session) and
`SDPCodecPreferLocalOrder` (per-session negotiation policy). Both are carried
through `Fork()`. The per-dialog config is built from the global `MediaConfig`
with transport values overlaid, so all fields propagate.

Kept process-global, deliberately:
`media.RTPBufSize` (pools and the jitter buffer capture it lazily at first
use — set once at startup), `media.RTPProfileSAVPDisable` (bool flag where
"unset" and "false" are indistinguishable, so no clean per-instance fallback),
debug/log knobs (`media.RTPDebug`, `RTCPDebug`, `RTPDebugTracer`,
`DTLSDebug`, `SetDefaultLogger`), the RTCP hook defaults and
`media.RTPSourceLock` (captured once per `NewRTPSession`), plus diago's
`PlaybackBufferSize`, `HTTPDebug`, `DefaultPlaybackHTTPClient`. Moving any of
these is a separate work item.

### Deprecated surface

Deprecated symbols (round-2 cancellation migration table in §9 plus the dead/near-dead
APIs annotated with `// Deprecated:`) are kept functional until the next
breaking release. Removed in this round: the unreachable `ReferTransaction`
cluster (`ReferTransaction`, `OnReferTransactionFunc`,
`dialogHandleReferTransaction`) — the live REFER path is
`OnReferDialogFunc`/`dialogHandleRefer`.

## 11. Shutdown and resource ownership

`Diago.Shutdown(ctx)` stops a Diago instance in two phases:

1. **Dialogs first** (signaling still alive): every dialog tracked in the
   dialog caches is hung up and closed, which also closes its media sockets
   and evicts the cache entry. Per-dialog hangup behavior:
   - Confirmed dialog → BYE sent and awaited.
   - Early server dialog (pre-confirmed) → 480 decline sent (a freshly
     received INVITE that has not yet reached a state is treated the same).
   - Confirmed client dialog without an Invite response → `Hangup` returns
     `ErrDialogNotAnswered`; the dialog is still closed.
   Each dialog gets a per-dialog hangup timeout bounded by `ctx`
   (default 3s, see `shutdownDialogHangupTimeout`); errors are joined into
   the Shutdown return value.
2. **Listeners last**: the SIP listeners started by `Serve`/`ServeBackground`
   are cancelled and awaited. Listener teardown races with phase 1: any
   INVITE admitted by a still-running listener during phase 1 is not part of
   the walk, but its natural lifecycle (handler return → BYE → `Close` →
   cache eviction) still cleans it up. The cache entry may therefore stay
   populated briefly after Shutdown returns; it is not a leak.

State-machine effects on the `Diago` instance:

- `Shutdown` sets the terminal flag immediately on entry, **before** phase 1
  or 2 run. Once `Shutdown` is called (regardless of outcome), further
  `Serve`/`ServeBackground` calls return an error and any in-flight `Serve`
  observes the flag at its second pre-lock check and refuses to spawn
  listeners.
- `Shutdown` is idempotent; a second call sees an empty cache and a nil
  `serveCancel`, returns `nil` (or the residual timeout error from the
  first call's listener wait).
- Listeners are not cancelled at Shutdown's first entry; they keep serving
  until phase 2. Concurrent callers of `Serve` are rejected, not delayed.

Ownership matrix:

| Resource | Owner | Shutdown behavior |
|---|---|---|
| Server dialogs (from INVITE) | Diago cache | Hangup + Close |
| Client dialogs (from ACK/Confirmed) | Diago cache | Hangup + Close |
| Client dialogs pre-ACK (inviting/early) | Caller (holds the Invite ctx) | Not reachable — cancel the Invite ctx (sends CANCEL), see §6 |
| RTP/RTCP media sockets | Per dialog | Closed with the dialog |
| SIP listeners | Diago serve loop | Closed and awaited |
| `sipgo.UserAgent` | Caller (may be shared by several Diago instances) | NOT closed — call `ua.Close()` yourself |
| Register transactions / qualify loops | Caller goroutines | Not stopped — cancel their ctx (triggers Unregister), ideally before Shutdown |
| Bridges | Caller | Reached indirectly through their dialogs |
| Registered SIP handlers | sipgo (cannot be deregistered) | Harmless: listeners are closed, so no new requests arrive |

Notes:
- In-flight serve handlers may run concurrently with phase 1; their own
  Hangup/Close on an already-torn-down dialog is a tolerated no-op path
  (same semantics as a handler hanging up while the framework tears down,
  §6). Media I/O after `Close` returns EOF/ErrDialogClosed; handlers should
  treat those as terminal.
- If `ctx` expires before the listeners exit, Shutdown returns the timeout
  error but already-hung-up dialogs stay torn down; media sockets close with
  dialogs regardless. The terminal flag and `serving` reset are observed by
  callers even on the timeout path.
- Passing an already-canceled `ctx` to `Shutdown` makes every per-dialog
  `Hangup` fail immediately with `context.Canceled`, producing one such error
  per cached dialog (joined into the return value). For a fast best-effort
  shutdown, use `context.Background()`; supply a small but positive deadline
  if you want a hard cap on phase 1.
- `Shutdown` does not deregister SIP handlers from sipgo — that registry is
  immutable for the server's lifetime. Once the listeners are closed, no new
  requests arrive and the handlers become unreachable.

## 12. Recording taps

`DialogMedia.StartStereoRecording` installs a stereo WAV recording tap by
wrapping the dialog's current audio reader and writer. Two contracts govern
where and when it can be attached.

**Install before the dialog joins a Bridge.** `BridgeMix.addDialogStream`
resolves each leg's reader/writer exactly once, at `AddDialogSession`, and
mixes bridged traffic through those captured handles. A tap installed after
the join sits behind the captured handle and never sees bridged audio, so the
recording is silent. The supported window is between `Answer()` and adding the
leg to a bridge — an application event hook that fires in that window is the
intended attachment point.

**Self-install is atomic.** Unlike the deprecated `SetAudioReader` /
`SetAudioWriter` pair (two separate lock acquisitions, and the writer setter
deprecated with no stereo replacement), `StartStereoRecording` wraps both
heads and rewires them under a single `DialogMedia.mu` hold, so there is no
half-wired window and no deprecated setter is touched. Each direction decodes
with its own codec; the two PCM spools share one interleaved timeline, so the
codecs must agree on sample rate and frame duration (a codec-name difference
is fine — PCMA in, PCMU out records).

**Fail-open by default.** A stereo tap sits inline in the media path, so a
PCM write failure (disk full) would otherwise propagate into the bridge mix
loop and interrupt the call. `StartStereoRecording` therefore swallows the
first recording write error, stops taking writes, and keeps media flowing; the
degradation surfaces via `StereoRecording.Err()` and again from `Close()`.
`WithRecordingFailOpen(false)` restores propagation. `WithRecordingSpoolDir`
moves the per-direction raw spool off `os.TempDir()` onto the recording
partition (spool files are created `0600`).

**Uninstall is best-effort.** `Close` removes the tap from the chain only
while it is still the outermost head. If a Bridge wrapped the chain on top
after `Start` (for example the `RTPRealTimeReader` the bridge may install),
`Close` cannot remove the tap from the middle of that chain without corrupting
the outer handle, so it leaves a *stopped, hence transparent* tap in place:
media keeps flowing, no further bytes are recorded. This is intentional; do
not assume the reader/writer chain is byte-for-byte restored after `Close`.

The caller retains ownership of the WAV writer: neither `StartStereoRecording`
nor `Close` closes it. Close the underlying fd after `Close` returns.

## 13. Music on Hold

Music on Hold is the optional hold-music loop started by `Hold` (or
explicitly by `DialogMedia.PlayMusicOnHold`). Its behavior is constrained by
several contracts to keep it from fighting the rest of the dialog.

**One loop per dialog.** `PlayMusicOnHold` returns `ErrMusicOnHoldActive`
when a loop is already running. `StopMusicOnHold` is a no-op when nothing is
playing. There is no "switch tone mid-loop"; stop first, then start again.

**Tone source precedence.** `Hold(ctx, WithMusicOnHold(tone))` overrides the
dialog-level `MediaConfig.MusicOnHold` for that hold only; a zero tone (no
segments) explicitly disables music for the call. With no option and no
dialog default, `Hold` succeeds but plays nothing — the re-INVITE behavior is
unchanged.

**Tone is re-rendered at the negotiated codec every frame.** The loop does
not capture a `*media.MediaSession`; it resolves the writer and codec on
every iteration through `audioWriterProps` (§4). A re-INVITE that changes the
negotiated codec is picked up on the next frame by re-rendering the tone at
the new sample rate — a `Tone` needs no resampler, unlike recorded sources.
The tone restarts from its first segment on such a switch (phase is not
preserved). This is also why a `MediaSession` getter (`NegotiatedDirection`)
is exposed: the loop queries it at startup and warns when the peer put us on
hold (recvonly/inactive negotiated), in which case the RTP direction gate
drops the audio. The loop keeps running so an unhold on either side resumes
audibly without a restart.

**Write-gate cooperation.** The loop writes through the same `audioWriter`
chain that other components use; a `PauseAudioWrite` refcounted gate is
waited out (the frame is replayed unchanged after release, same as
`PlayTone`). This means hold music survives a transient pause by another
component and is cancelled cleanly when the dialog closes (`Close` cancels
without waiting on it, because the loop takes `d.mu` per frame — the loop
self-clears `d.moh` on exit).

**Best-effort auto start/stop on Hold/Unhold.** `Hold` starts hold music
automatically only after its re-INVITE succeeds. The music lifetime is
detached from the `Hold` call's context (which typically carries the
re-INVITE timeout): it runs until `Unhold`, `Stop`/`StopMusicOnHold`, or
dialog `Close`. Failures are logged at warn and never surface as a `Hold`
error (the re-INVITE already succeeded; returning an error would invite a
retry straight into 491 glare). An active manual loop is left untouched.
`Unhold` stops the loop `Hold` started automatically and only that one —
manually started music is under the caller's `Stop`/ctx control.

**Remote-hold detection.** `DialogMedia.IsRemoteHeld()` reports whether the
peer put us on hold (negotiated recvonly/inactive). It is refreshed on every
media install — inbound offer SDP (`sdpUpdateUnsafe`) and answers to our own
offers (`mediaUpdateUnsafe`/`initRTPSessionUnsafe`, including the initial
INVITE answer). Our own `Hold`/`Unhold` negotiate sendonly/sendrecv and never
trip it. Combine with `WithOnMediaUpdate` to react to remote hold/unhold
without polling. By RFC convention, the held peer is silent; an active MoH on
a remote-held dialog is correct (the direction gate drops our audio until the
peer unholds).

**Bridge limitation.** The proxy media loop in `Bridge` resolves
reader/writer once at `AddDialogSession` and exits on the first
`ErrWritePaused`. Stacking MoH on a bridged leg would race the bridge copy
loop with the music frames and is **not** supported in this revision. To
hold a bridged call, stop the bridge or apply hold music before the legs
are bridged.
