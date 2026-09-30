# diago API guidelines

Conventions used by the public API. New code (and reviews) should follow them;
`doc.go` holds the narrative overview.

## Naming

- Call legs are `InboundCall` (UAS) and `OutboundCall` (UAC); the shared
  contract is the `Call` interface. Do not re-introduce `Dialog*` types.
- The engine is `Agent`, constructed by `New`. Options are per-domain types
  (`AgentOption`, `SignalOption`, `MediaConfig`, `ReaderOption`, ...).
- Methods follow one verb per intent:
  - `New*` constructs an object (`New`, `NewCall`, `NewBridge`).
  - `Start*` starts background work (`StartPlayback`, `StartMusicOnHold`,
    `StartStereoRecording`).
  - `Play*`/`Send*` perform a blocking action (`PlayTone`, `SendDTMF`).
  - `Close` releases local resources and never sends SIP; `Hangup` performs
    the signaling teardown.
- No stutter: prefer `Transport.Network` over `Transport.Transport`,
  `call.Media()` over `call.DialogMedia()`, `call.SIP()` over
  `call.DialogSIP()`.

## Options

- Every option constructor is `WithX` and returns a concrete (unexported)
  type. APIs take the per-operation interface for what they consume
  (`AnswerOption`, `HangupOption`, `InviteOption`, `DialOption`,
  `RegisterOption`, ...); passing an option an API does not consume is a
  compile error. Keep `TestOptionInterfaceMatrix` in sync when adding one.
- Option constructors validate their own input and return an error from the
  option function (surfaced by the API call). Nil callbacks, empty names and
  out-of-range values are rejected instead of silently ignored.
- Nil options are skipped; slices of options must use the operation's
  interface type (`[]diago.RegisterOption`, not `[]diago.SignalOption`).
- Agent-level options validate in `New` and abort construction
  (`AgentOption` returns an error).

## Errors

- Every user-visible condition has an `Err*` sentinel; returned errors must be
  `errors.Is`-compatible (`%w` wrapping, `errors.Join` for multiple failures).
- Typed errors carry structured data: `*ResponseError` (handler to SIP
  response), `*RegisterResponseError` (REGISTER status).
- `DialogHandler` errors map to SIP: `ErrDecline` 480, `ErrBusy` 486,
  `ErrNotFound` 404, `*ResponseError` custom, anything else 500; wrap
  `ErrResponseSent` when the handler already wrote the final response.
- Do not return bare `fmt.Errorf` for conditions callers must branch on.

## Contexts

- Signaling methods honor the context while sending. `Answer`, `AnswerLate`
  and `RespondSDP` return once the 2xx was handed to the transaction layer;
  the ACK wait continues in the background and is observed with
  `WaitAck`/`AckReceived`/`AckErr`.
- Background operations either take a context, return a `StopFunc`, or return
  a handle with `Close`; the contract is documented per method.

## Lifecycle

- `Hangup` tears down signaling; `Close` releases local resources; `Close` is
  idempotent everywhere.
- Server dialogs are owned by the framework from the incoming INVITE: a handler
  that answers and returns keeps the call alive; the framework closes the
  dialog (media, cache entry) when it ends.
- `Agent.Close` hangs up dialogs, stops listeners and closes the private
  UserAgent. A UserAgent supplied with `WithUserAgent` stays caller-owned.
- Caller-owned resources are explicit: the UserAgent (when supplied),
  registration handles, outgoing calls that never got a response, bridge
  objects.

## Configuration

- No package-level mutable configuration: settings live in `AgentOption`,
  `MediaConfig`, `media.MediaSession` fields or per-call options. Tracers,
  debug switches, loggers and HTTP clients are per instance.
- Media configs are complete values; `Transport.Profile` selects a named
  profile registered with `WithMediaProfile` and there is no field-level merge
  between profiles. Transport-derived bind/advertise addresses only fill
  fields a profile leaves nil.
- Defaults are documented next to the field (`NetworkConfig.RTPPortStart`,
  `MediaConfig.Debug`, `WithHTTPClient`, ...).

## Concurrency

- Signaling methods on `Call`/`Agent` are safe for concurrent use; media
  accessors resolve state at use time. Document exceptions (for example
  `OutboundCall.Invite` updates the invite request and is not thread safe).
- Re-INVITE swaps media internals behind stable handles
  (`RTPPacketReader`/`RTPPacketWriter`): components must resolve them per use
  instead of capturing sessions.
- Single-writer rule: one component writes audio at a time (playback, MoH,
  bridge, recording tap). Pause gates (`PauseAudioRead/Write`) are refcounted
  and their release must be called exactly once.

## Public surface

- sipgo types are exposed through aliases in `aliases.go` (`URI`, `Header`,
  `Request`, `Response`, `ServerTransaction`, `RequestHandler`, ...); keep new
  signatures on the aliases so applications can use diago without importing
  sipgo. Escape hatches are explicit: `WithUserAgent`, `WithServer`,
  `WithClient`, `Agent.Server`, `Agent.UA`, `call.SIP`.
- `git grep 'sipgo\.' -- '*_test.go'` is fine; production signatures should
  not grow new direct sipgo types.
- Exported identifiers need a doc comment; keep promises (contexts, errors,
  concurrency) in the comment next to the method. CI enforces this with
  `revive`'s `exported` rule on the root package.
- Run `scripts/apidiff.sh <git-ref>` before opening a PR: it prints the public
  API diff against the ref. CI runs it against the PR base commit
  (informational until the API is frozen).
