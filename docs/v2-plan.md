# Diago v2 Plan

Status: draft for review. Breaking changes are explicitly allowed in v2.

This plan is based on a source-level interface review and a follow-up
re-verification pass. File/line references point at the v1 tree and are
indicative, not normative. For every user-visible rename or removal there is a
corresponding row in [v2-migration.md](v2-migration.md).

## Release strategy

| Item | Decision | Notes |
|---|---|---|
| Go module | `github.com/sjlit/diago/v2` | Major version requires the `/v2` path |
| Branches | `main` (v1 maintenance) + `v2` (development) | v1 gets correctness fixes only |
| Release train | `v2.0.0-beta.1` → `rc` → `v2.0.0` | Collect migration feedback during beta |
| Migration aids | `v2-migration.md` + optional `gofmt -r` codemod | Renames are codemod-able; signatures are not |
| Dependency | Upgrade sipgo (or confirm the upstream ACK-race fix) | Decides whether P0-2 can drop the 100ms heuristic |

## Priority overview

| Phase | Theme | May break API |
|---|---|---|
| P0 | Correctness and safety fixes | Mostly no (P0-1B/P4 carry the type changes) |
| P1 | Handler and dialog lifecycle redesign | Yes (core) |
| P2 | client/server symmetry and contexts | Yes |
| P3 | Option system rework | Yes |
| P4 | Configuration model rework | Yes |
| P5 | Public surface cleanup | Yes |
| P6 | Docs, migration, release | No |

---

## P0 - Correctness and safety fixes

Goal: eliminate defects that currently break real calls, before reshaping the
signatures. These can be shipped on the v1 line first to reduce v2 regression
surface.

### P0-1 SRTP/DTLS semantics and validation

Labels: A = v1-compatible, B = v2-breaking.
Files: `diago.go:86-89,155-170,895-902`; `dialog_media.go:306-320`;
`media/media_session.go:195,268-269,474,520,670-674`.

Steps:

1. Add untyped constants `SecureRTPNone=0 / SecureRTPSDES=1 / SecureRTPDTLS=2`
   (untyped so they can be assigned to the current `int` fields on the v1
   line); validate the enum in `WithTransport`/`WithMediaConfig` and fail
   early on anything else.
2. Auto-enable DTLS: when `DTLSConf` is non-zero and the mode is `none`, set
   the mode to DTLS. `media.DTLSConfig` contains a `func` field
   (`media/dtls.go:58`) so it cannot be compared with `==`; use a
   `dtlsConfIsZero()` field check.
3. Fix the comments on `Transport.MediaSRTP` and `MediaConfig.SecureRTP`: they
   claim `0/1` while `examples/dtls` uses `2` for DTLS.
4. [B] Introduce `type SecureRTP int`; fold `Transport.MediaSRTP` into
   `Transport.Media` (single source of truth); replace `SecureRTPAlg uint16`
   with a typed `SRTPAlg`, and document its precedence against
   `DTLSConfig.SRTPProfiles` (current media behavior: `SecureRTP > 0 &&
   SRTPAlg == 0` selects the default profile,
   `media/media_session.go:268`).

Acceptance: `TestTransportSecureRTPValidation`,
`TestMediaConfigDTLSAutoEnable`, `TestIntegrationDTLSAutoNegotiation`
(reference `media/dtls_test.go:18` and `examples/dtls`); extend
`media_config_propagation_test.go`.

Risk: `media.MediaSession.SecureRTP` is also a public `int`; v2 must type it
too, otherwise diago still needs conversions.

### P0-2 Remove the ACK grace heuristic

Labels: v1-compatible.
Files: `dialog_server_session.go:37-77,160-222`; `diago.go:365-393`.

Steps:

1. Check whether a sipgo release after v1.4.3 fixed the `WriteResponse`
   `No ACK received` race. If yes, upgrade and delete `isAckWaitRaceError`
   and the 100ms branch (`:199-208`) entirely.
2. If not, keep the event-driven `ackCh`, but promote
   `100 * time.Millisecond` to an injectable internal `ackGracePeriod`
   (same default), so tests can control it and correctness does not depend
   on a fixed sleep. Keep the non-blocking channel read before the timer.
3. Define the failure contract: no ACK beyond the grace window returns the
   original error; the P1 handler error mapping decides the SIP response.

Acceptance: `TestDialogServerSessionRespondSignalAckWithinGrace`,
`TestDialogServerSessionRespondSignalAckBeyondGraceFails`,
plus the existing `TestDialogServerSessionMatchesInviteAck`
(`dialog_server_session_test.go:681`) as regression.

Risk: if the upstream fix exists but changes behavior, pin the sipgo version.

### P0-3 REGISTER retry semantics

Labels: v1-compatible.
Files: `diago.go:1047-1083`; `register_transaction.go:34-47,250-314`.

Steps:

1. Add a runtime `retryAfter time.Duration` field to `RegisterTransaction`,
   separate from `expiry` (the server `Expires` value).
2. Precedence: explicit `WithRegisterRetryInterval` > server `Retry-After` >
   `0.75*expiry`. Rework `calcRetry` to read `retryAfter`; clear it after a
   successful round.
3. In `Diago.Register` write `t.retryAfter = retryAfter` instead of
   poisoning `t.expiry` (`diago.go:1082`).
4. Extract the loop sleep into an injectable `sleepFunc`; otherwise the
   timing tests are slow and flaky.

Acceptance: `TestRegisterRetryAfterHonored`,
`TestRegisterRetryIntervalBeatsRetryAfter`, `TestRegisterTerminal4xxStops`,
`TestRegisterTransient408Retries`, extending the fake-transaction harness in
`diago_test.go:61`.

Risk: do not regress the 408/491 transient exceptions already handled at
`diago.go:1058-1060`.

### P0-4 Error sentinels

Labels: v1-compatible (additive; some message text changes).
Files: `errors.go`; `dialog_server_session.go:356,467,516`;
`dialog_client_session.go:480,497`; `dialog_media.go:32`.

Steps:

1. Add `ErrSDPMissing`, `ErrMediaSessionMissing`, `ErrRemoteContactMissing`.
2. Wrap the bare errors `"no sdp present in INVITE"`,
   `"no SDP in response"`, `"reinvite: 2xx has no Contact..."` with `%w`.
3. Replace the `err != errNoRTPSession` comparison
   (`dialog_client_session.go:497`) with `errors.Is`; delete the unexported
   `errNoRTPSession`.

Acceptance: `TestErrorsIsSDPMissing`, `TestErrorsIsMediaSessionMissing`,
`TestReinviteNoContactError`; an audit pass for bare `fmt.Errorf` on these
paths.

Risk: users matching error strings see new text (acceptable in v2; noted in
the migration guide).

### P0-5 Media lifecycle gaps

Labels: v1-compatible.
Files: `dialog_media.go:149-204,1153-1185`; `audio_ringtone.go:27-63`.

Steps:

1. Clear `dtmfReaders` inside `DialogMedia.Close` under the lock so
   `deliverDTMF` cannot reach stale readers. The existing copy-then-unlock
   logic makes the residual window harmless.
2. Keep `AudioRingtone.PlayBackground` self-terminating on write failure
   (an `onClose` hook would risk lock-order deadlocks); lock the behavior
   with a test instead.
3. Optional: route `deliverDTMF` logging through the injected logger.

Acceptance: `TestDialogMediaCloseClearsDTMFReaders`,
`TestDeliverDTMFAfterCloseNoCallback`,
`TestRingtonePlayBackgroundStopsOnDialogClose`.

Risk: low. Do not introduce new locking around the MoH cancel ordering
documented at `dialog_media.go:165-171`.

### P0-6 serve semantics on partial transport failure

Labels: v1-compatible (behavior change, needs review).
Files: `diago.go:539-602`; `shutdown_test.go:174`.

Steps:

1. Write the failing test first: a `bogus` transport plus a working `udp`
   one, then probe the UDP port after `ServeBackground` returns.
2. On the first listener error, call `serveCancel()` and wait for `serveWG`
   before returning `errors.Join(...)`. Keep the existing protection against
   the sipgo listener startup race (comment at `:596-601`): only cancel once
   at least one listener has become ready.
3. P4 validates `Transport` values, so the `"bogus"` fixture moves to
   `WithTransport`; switch the test to an occupied-port failure.

Acceptance: extend `TestDiagoServePartialFailureReturnsError`
(`shutdown_test.go:174`) and add `TestServeAllListenersStoppedAfterFailure`.

Risk: run the new tests with `-count=20 -race`; historical sipgo
`ListenAndServe` cancellation races may resurface.

### P0 commit order

```
P0-4 (pure additive errors) -> P0-5 (internal) -> P0-3 (self-contained)
-> P0-1A (constants + validation) -> P0-2 (depends on sipgo decision)
-> P0-6 (after lifecycle review)
```

---

## P1 - Handler and dialog lifecycle (core break)

| # | Item | New design | Break | Size |
|---|---|---|---|---|
| 1.1 | Handler signature | `type DialogHandler func(ctx context.Context, d *DialogServerSession) error`; `Serve(ctx, handler)` | every serve callback | M |
| 1.2 | Error-to-SIP mapping | Sentinels `ErrDecline` (480), `ErrBusy` (486), `ErrNotFound` (404), `ErrResponseSent` (handler already responded), plus `*ResponseError{Code,Reason,Headers}`. Handler error -> mapped response, or BYE with a Reason header when confirmed; nil -> `Hangup` | silent logging becomes responses | M |
| 1.3 | Handler context | handler ctx = serve ctx + dialog lifetime; returning cancels it | semantics made explicit | S |
| 1.4 | Remove `HandleFunc` | `Serve` only; pre-registration via `WithDialogHandler` option if needed | all `HandleFunc` call sites | S |
| 1.5 | Close/Hangup contract | `Hangup(ctx)` sends BYE/decline; `Close()` is local-only; add `HangupAndClose(ctx)` | client code that only called `Close` was already leaking the remote leg | S |
| 1.6 | Shutdown contract | Diago owns listeners and dialogs; playback/recording remain user-owned but all teardown hooks flow through `DialogMedia.onClose` | no compile break | S |

Acceptance: handler returning `ErrBusy` produces 486; generic error produces
500 or the requested code; nil produces BYE; race test of Shutdown concurrent
with an active handler.

## P2 - client/server symmetry and contexts

| # | Item | New design | Break | Size |
|---|---|---|---|---|
| 2.1 | ctx everywhere | `Trying/Ringing/ProgressMedia/Answer/AnswerLate/RespondSDP(ctx, opts...)` | all call sites | M |
| 2.2 | `Progress()` corrected | `Trying(ctx)` = 100; `Progress(ctx)` = 183 without SDP; `ProgressMedia(ctx)` = 183 with SDP. Delete `ProgressMediaOptions` | old `Progress` semantics | S |
| 2.3 | Invite unification | High-level `Diago.Dial` (NewDialog+Invite+Ack, auto-ACK); low-level `DialogClientSession.Invite` unchanged (explicit `Ack` needed for pre-ACK bridging); `InviteBridge` -> `DialBridge` | renames | M |
| 2.4 | Internal methods | `ReadAck` unexported; `FromUser/ToUser/Transport` move into `DialogSession` (or an `Info()` accessor) | references | S |
| 2.5 | `WaitAnswer` options | `WaitAnswer(ctx, opts ...SignalOption)`; stop exposing `sipgo.AnswerOptions` | call sites | S |

## P3 - Option system

| # | Item | New design | Break | Size |
|---|---|---|---|---|
| 3.1 | Scope validation | Keep `SignalOption func(*SignalParams) error`, but options record touched groups; each API declares allowed groups and `newSignalParams` rejects misuse with `UnsupportedOptionError` | silent ignore becomes an error | M |
| 3.2 | Consistent validation | All `With*` reject empty/nil/out-of-range uniformly; `WithAuthCredentials("", "")` errors; empty `SDPSessionName` means "no override" everywhere | previously tolerated calls fail | S |
| 3.3 | Delete legacy structs | `InviteOptions/InviteClientOptions/NewDialogOptions/AnswerOptions/ProgressMediaOptions` and their `Options()` converters | compile | S |
| 3.4 | Naming | `WithMediaSDPSessionName` -> `WithSDPSessionName`; keep `WithMedia*` for media overrides | rename | S |
| 3.5 | Hide params | `SignalParams` unexported; scope table replaces the "Honors:" prose | direct struct construction | S |

Alternative (compile-time split into `InviteOption`/`AnswerOption`/... ) is
rejected: it explodes the type set and makes cross-cutting options such as
`WithHeaders` awkward. Runtime scope validation is the chosen trade-off.

## P4 - Configuration model

| # | Item | New design | Break | Size |
|---|---|---|---|---|
| 4.1 | Split `MediaConfig` | `MediaConfig{Codecs, SRTP SRTPConfig, Network NetworkConfig, SDP SDPConfig, MoH}`; `NetworkConfig{BindIP,ExternalIP,RTPNAT,RTPPortStart/End}` | config literals | M |
| 4.2 | Single source | Remove duplicated media fields on `Transport`; `Transport` carries overrides merged by a documented rule ("zero inherits global") | `WithTransport` usage | M |
| 4.3 | `Transport` validation | `NewTransport(network)`/`Validate()`; accepted: udp/tcp/tls/ws/wss plus 4/6 suffixes; invalid fails immediately | construction sites | M |
| 4.4 | Default transport | Remove the implicit `127.0.0.1:5060`; `NewDiago` errors without a transport (or requires explicit `WithDefaultTransport`) | silent loopback fixed | S |
| 4.5 | media globals | Keep `media.RTPPortStart` etc. as deprecated process fallbacks; config struct is the recommended path | additive | S |
| 4.6 | `MediaProps` output | `ReaderWithProps() (io.Reader, MediaProps, error)` / `WriterWithProps()`; delete `WithAudioReaderMediaProps`/`WithAudioWriterMediaProps` | call sites | S |

## P5 - Public surface cleanup

| # | Item | New design | Break | Size |
|---|---|---|---|---|
| 5.1 | Internal caches | Unexport `DialogCache/DialogCachePool/DialogData`; expose only `MatchDialog(req)`; delete `DialogCacheServer/Client` | references | S |
| 5.2 | Stable handles | Unexport `RTPPacketReader/Writer` fields; expose accessors | field references | S |
| 5.3 | Self accessor | Remove `DialogMedia.Media()`; rename the interface method to `DialogMedia()` (already used in `docs/contracts.md` §1) | method rename | S |
| 5.4 | Stop handles | One `type StopFunc func() error` for `ListenBackground`, `ProxyMediaControl`, `AudioRingtone.PlayBackground`, `PauseAudioRead/Write` | signatures | S |
| 5.5 | Playback returns | `CreatePlayback/CreatePlaybackControl/CreateRingtonePlayback` return pointers consistently | signatures | S |
| 5.6 | Bridge interface | `DialBridge` accepts `Bridger` (satisfied by `Bridge` and `BridgeMix`); delete `GetDialogs` | interface users | M |
| 5.7 | diagotest | `NewConnRecorder` returns exported `ConnRecorder` | test code | S |
| 5.8 | No-op option | Delete `WithAuth`; incoming auth stays on `Authorize`, outgoing on `WithAuthCredentials` | references | S |

## P6 - Docs, migration, release

| # | Item | Deliverable |
|---|---|---|
| 6.1 | `v2-migration.md` | Old -> new table for every rename/removal |
| 6.2 | Codemod | `gofmt -r` rules for renames (~60% mechanical) |
| 6.3 | Contracts | Replace "Honors:" prose in code with option scope declarations; fold `docs/contracts.md` references |
| 6.4 | Examples | Rewrite all 13 examples as v2 migration samples |
| 6.5 | Regression | `go test -race ./...` + `go vet` + examples build; add integration tests for handler error mapping, SRTP, REGISTER retry |
| 6.6 | Release | `v2.0.0-rc.1` -> `v2.0.0`; v1 branch maintenance window (suggest 6 months) |

## Execution order and dependencies

```
P0 (correctness, ship first) --+--> P4 config model --> P3 option system --+
                               +--> P1 handler/lifecycle --> P2 ctx/symmetry +--> P5 surface --> P6 release
```

Rationale:

1. P0 is signature-independent and its constants/errors feed P3/P4.
2. P4 precedes P3 because `SignalParams` references `MediaConfig`.
3. P1 and P4 can run in parallel (small overlap in `diago.go`).
4. P2 follows P1 immediately so signatures are changed once.
5. P5 is mechanical and low-risk; do it last before release.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| External users cannot migrate incrementally | MIGRATION guide + codemod + staged beta tags |
| Upstream sipgo ACK race remains unfixed | P0-2 fallback: ackCh-driven wait with a configurable internal grace |
| Config merge semantics regress under multiple transports | Table-driven tests for the global/transport/per-call precedence |
| `Progress` semantic change is missed by users | Rename and highlight it as the first migration entry |
| `Transport` validation breaks legitimate `tls`/`wss` users | Document the full accepted set and add it before enforcing |
