# diago API guidelines

Conventions used by the public API. New code (and reviews) should follow them;
`doc.go` holds the narrative overview.

## Naming

- Call legs are `InboundCall` (UAS) and `OutboundCall` (UAC); the shared
  contract is the `Call` interface. Do not re-introduce `Dialog*` types.
- The engine is `Agent`, constructed by `New`. Options are per-domain types
  (`Config`, `AnswerOptions`, `MediaConfig`, `ReaderOptions`, ...).
- Methods follow one verb per intent:
  - `New*` constructs an object or a media handle (`New`, `NewCall`,
    `NewBridge`, `NewPlayback`, `NewDTMFReader`, ...). A playback handle
    created by a `New*` factory streams nothing until one of its `Play*`
    methods is called.
  - `Start*` starts background work (`StartMusicOnHold`,
    `StartStereoRecording`).
  - `Play*`/`Send*` perform a blocking action (`PlayTone`, `SendDTMF`).
  - `Close` terminates the signaling (BYE, CANCEL or decline as appropriate)
    and releases local resources. Local-only resource release is internal.
- No stutter: prefer `Transport.Network` over `Transport.Transport`,
  `call.Media()` over `call.DialogMedia()`, `call.SIP()` over
  `call.DialogSIP()`.
- Targets: `Dial`, `DialBridge` and `StartRegistration` take a SIP URI string and have a
  `*URI` variant for an already parsed `URI`. Low-level constructors take a
  `URI` only (`NewCall`); Go has no overloading, so they
  have no string variant.

## Options

- Every operation takes its own `XxxOptions` struct (`AnswerOptions`,
  `InviteOptions`, `DialOptions`, `RegisterOptions`, `CloseOptions`, ...).
  Zero values select the documented defaults; at most one options value is
  accepted per call.
- Options are validated when the operation runs. Nil headers, empty names and
  out-of-range values are rejected instead of silently ignored; every such
  error wraps `ErrInvalidOption`.
- There is no shared option interface; pass the concrete operation struct.
- Agent-level configuration validates in `New` and aborts construction.

## Errors

- Every user-visible condition has an `Err*` sentinel; returned errors must be
  `errors.Is`-compatible (`%w` wrapping, `errors.Join` for multiple failures).
- Typed errors carry structured data: `*ResponseError` (handler to SIP
  response), `*RegisterResponseError` (REGISTER status).
- `CallHandler` errors map to SIP: `ErrDecline` 603,
  `ErrTemporarilyUnavailable` 480, `ErrBusy` 486, `ErrNotFound` 404,
  `*ResponseError` custom, anything else 500; wrap `ErrResponseSent` when the
  handler already wrote the final response.
- Configuration and lifecycle conditions have sentinels as well:
  `ErrInvalidOption` (every option validation failure), `ErrNoTransport`,
  `ErrTransportNotFound`, `ErrAgentServing`, `ErrAgentClosed`,
  `ErrManagedMethod`, `ErrHandleAfterServe` and `ErrBridgeNil`.
- Do not return bare `fmt.Errorf` for conditions callers must branch on.

## Contexts

- Signaling methods honor the context while sending. By default `Answer`,
  `AnswerLate` and `RespondSDP` return once the 2xx was handed to the
  transaction layer; the ACK wait continues in the background and is observed
  with `WaitAck`. `AnswerOptions.ACKPolicy = ACKPolicyWait` makes `Answer` and
  `AnswerLate` wait for the ACK.
- Background operations either take a context, return a `StopFunc`, or return
  a handle with `Close`; the contract is documented per method.

## Lifecycle

- `Call.Close(ctx)` tears down signaling and releases local resources; `Close`
  is idempotent everywhere.
- Server dialogs are owned by the framework from the incoming INVITE: a handler
  that answers and returns keeps the call alive; the framework closes the
  dialog (media, cache entry) when it ends.
- `Agent.Close` hangs up dialogs, unregisters and stops the registrations it
  created (`StartRegistration`/`StartRegistrationURI`), stops listeners and closes the private
  UserAgent. A UserAgent supplied with `Config.UserAgent` stays caller-owned.
- Caller-owned resources are explicit: the UserAgent (when supplied),
  registrations created with `RegisterOptions{Detached: true}`, REGISTER
  transactions, outgoing calls that never got a response, bridge objects.

## Configuration

- No package-level mutable configuration: settings live in `Config`,
  `MediaConfig`, `media.MediaSession` fields or per-call options. Tracers,
  debug switches, loggers and HTTP clients are per instance.
- Media configs are complete values; `Transport.Profile` selects a named
  profile registered in `Config.MediaProfiles` and there is no field-level merge
  between profiles. Transport-derived bind/advertise addresses only fill
  fields a profile leaves nil.
- Defaults are documented next to the field (`NetworkConfig.RTPPortStart`,
  `MediaConfig.Debug`, `Config.HTTPClient`, ...).

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
  sipgo. Escape hatches are explicit: `Config.UserAgent`, `Config.Server`,
  `Config.Client`, `Agent.Server`, `Agent.UA`, `call.SIP`.
- `media` and `audio` are subpackages of this module, not third-party
  dependencies: they stay explicit imports and are intentionally not aliased.
- Accessors: `Agent.UA` and `Agent.Server` never return nil; `Agent.Client` is
  nil unless `Config.Client` was used (each transport then uses its own client).
  `Agent.Transports` returns resolved copies of the transport configuration.
- `git grep 'sipgo\.' -- '*_test.go'` is fine; production signatures should
  not grow new direct sipgo types.
- Exported identifiers need a doc comment; keep promises (contexts, errors,
  concurrency) in the comment next to the method. CI enforces this with
  `revive`'s `exported` rule on the root package.
- Run `scripts/apidiff.sh <git-ref>` before opening a PR: it prints the public
  API diff against the ref. CI runs it against the PR base commit
  (informational until the API is frozen).
