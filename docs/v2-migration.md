# Diago v2 Migration Guide

Status: v2 is implemented on the `v2` branch (module
`github.com/sjlit/diago/v2`). See [v2-plan.md](v2-plan.md) for the phased work
plan. v2 allows breaking changes; this document maps every user-visible
rename, removal, and behavior change.

## 1. Module and initialization

| v1 | v2 | Notes |
|---|---|---|
| `github.com/sjlit/diago` | `github.com/sjlit/diago/v2` | Major-version import path |
| `NewDiago(ua, opts...) *Diago` | `NewDiago(ua, opts...) (*Diago, error)` | Also fails on an invalid transport |
| Implicit `127.0.0.1:5060` transport when none configured | `NewDiago` errors; configure with `WithTransport`, or opt into the old default explicitly with `WithDefaultTransport()` | Removes a silent loopback default |
| `WithAuth(...)` | Removed | It was a no-op. Incoming calls: `DialogServerSession.Authorize`; outgoing/REGISTER: `WithAuthCredentials` |
| `WithTransport(Transport{...})` | `WithTransport(Transport{...})` with validation (`Transport.Validate`/`NewTransport`) | Invalid network values now fail at construction |

## 2. Server handler and lifecycle

| v1 | v2 |
|---|---|
| `Serve(ctx, func(d *DialogServerSession))` | `Serve(ctx, func(ctx context.Context, d *DialogServerSession) error)` |
| `type ServeDialogFunc` | `type DialogHandler` (same signature as above) |
| `dg.HandleFunc(f)` | Removed; pass the handler to `Serve` |
| Handler return -> framework sends `Hangup` | Return `nil` -> `Hangup`; return `error` -> mapped response + teardown |
| — | `ErrDecline` -> 480, `ErrBusy` -> 486, `ErrNotFound` -> 404, `ErrResponseSent` -> no second response, `*ResponseError{Code,Reason,Headers}` -> custom; a confirmed call gets BYE with a Reason header instead |
| `d.Close()` local-only | Unchanged; `Hangup` is signaling, `Close` is local, `HangupAndClose(ctx)` combines them |
| `DialogSession` interface | Adds `FromUser()/ToUser()/Transport()`; custom implementations must add the three methods |
| `Progress()` sent 100 Trying | `Trying(ctx)` sends 100; `Progress(ctx)` sends 183 without SDP |
| `Trying()/Ringing()/ProgressMedia()/Answer()/AnswerLate()/RespondSDP()` | First parameter is now `ctx` |
| `AnswerOptions`, `ProgressMediaOptions` | Removed; use `SignalOption` |

## 3. Outgoing calls and REGISTER

| v1 | v2 |
|---|---|
| `Diago.Invite(...)` (auto-ACK) | `Diago.Dial(...)` (auto-ACK) |
| `Diago.InviteBridge(..., *Bridge, ...)` | `Diago.DialBridge(..., Bridger, ...)` (`Bridge` and `BridgeMix`) |
| `DialogClientSession.Invite` + `Ack` | Unchanged (low-level, explicit ACK for pre-ACK bridging) |
| `WaitAnswer(ctx, sipgo.AnswerOptions)` | `WaitAnswer(ctx, ...SignalOption)` |
| `InviteOptions/InviteClientOptions/NewDialogOptions` | Removed; use `SignalOption` |
| `Bridge.GetDialogs()` | Removed; track the sessions you added |

## 4. Options

| v1 | v2 |
|---|---|
| Inapplicable options silently ignored | `UnsupportedOptionError` (for example `WithBody` on `Trying`) |
| `WithMediaSDPSessionName` | `WithSDPSessionName` |
| `WithAuthCredentials("", "")` accepted | Rejected (as are empty `WithCodecs`, `WithBody(nil)`, `WithHeader("", ...)`, nil callback options, out-of-range `WithRTPNAT`, negative REGISTER durations) |
| `MediaConfig.SDPSessionName == ""` | Unchanged: empty keeps the library default; `WithSDPSessionName("")` is rejected |
| `SignalParams` exported | Unexported; use the `With*` constructors (the `SignalDialogParams`/`SignalMediaParams`/`SignalMsgParams`/`SignalRegisterParams` sub-structs go away with it) |
| Custom `SignalOption(func(p *SignalParams))` closures | Not possible (`signalParams` is unexported); use `WithRequestMutator`/`WithResponseMutator` |
| `AnswerOptions` wrapper method | Removed; `Answer(ctx, ...SignalOption)` |

## 5. Media configuration

| v1 | v2 |
|---|---|
| `Transport.MediaSRTP int` (docs said 0/1; 2 meant DTLS), `Transport.MediaDTLSConf`, `Transport.MediaExternalIP` | `Transport.Media.SRTP{Mode SecureRTP, Alg SRTPAlg, DTLS media.DTLSConfig}` and `Transport.Media.Network{ExternalIP/BindIP/RTPNAT/RTPPortStart/RTPPortEnd}`; zero fields inherit `MediaConfig` |
| `MediaConfig.SecureRTP int`, `MediaConfig.SecureRTPAlg uint16` | `MediaConfig.SRTP{Mode SecureRTP, Alg SRTPAlg, DTLS media.DTLSConfig}` |
| `MediaConfig.RTPNAT int` | `MediaConfig.Network.RTPNAT RTPNATMode` (`RTPNATDisabled`/`RTPNATLearnSource`) |
| `MediaConfig.BindIP/ExternalIP/RTPPortStart/RTPPortEnd` | `MediaConfig.Network.{BindIP,ExternalIP,RTPPortStart,RTPPortEnd}` |
| `MediaConfig.SDPCodecPreferLocalOrder/SDPSessionName` | `MediaConfig.SDP.{CodecPreferLocalOrder,SessionName}` |
| `WithAudioReaderMediaProps(*MediaProps)` | `ReaderWithProps(...AudioReaderOption) (io.Reader, MediaProps, error)` |
| `WithAudioWriterMediaProps(*MediaProps)` | `WriterWithProps(...AudioWriterOption) (io.Writer, MediaProps, error)` |

## 6. Public surface

| v1 | v2 |
|---|---|
| `DialogCacheServer()/DialogCacheClient()` | Removed; use `MatchDialog(req)` |
| `DialogCache`, `DialogCachePool`, `DialogData` exported | Unexported/removed |
| `DialogServerSession.ReadAck` | Unexported (framework internal) |
| `DialogMedia.Media()` | `DialogMedia()` (method renamed on `DialogSession` too) |
| `DialogClientSession.DialogMedia` / `DialogServerSession.DialogMedia` embedded field | Unexported; the exported `DialogMedia()` method is the only accessor |
| `DialogMedia.RTPPacketReader/RTPPacketWriter` fields | Unexported; use the `RTPPacketReader()`/`RTPPacketWriter()` accessors |
| `AudioStereoRecordingCreate` | `StartStereoRecording` |
| `CreatePlayback/CreatePlaybackControl/CreateRingtonePlayback` return values | Return pointers (and `AudioPlaybackDTMF` embeds `*AudioPlaybackControl`) |
| `ListenBackground` stop `func() error`, `PauseAudioRead/Write` `func()`, `ProxyMediaControl` `func() error` | Unified `StopFunc func() error` |
| `Bridger` defined but never accepted | `DialBridge` accepts `Bridger` |
| `diagotest.NewConnRecorder() *connRecorder` | `NewConnRecorder() *ConnRecorder` |
| Root `diago.NewConnRecorder()`/`connRecorder` (test helper) | Test-only (`diago_test_utils_test.go`); use `diagotest.NewConnRecorder()` |
| `diagotest.NewDiagoClientTest(ua, onRequest) *diago.Diago` | `NewDiagoClientTest(ua, onRequest) (*diago.Diago, error)` |

## 7. Errors

| v1 | v2 |
|---|---|
| `"no sdp present in INVITE"` | `ErrSDPMissing` (`errors.Is`) |
| `errNoRTPSession` (unexported, compared with `!=`) | `ErrMediaSessionMissing` |
| `"reinvite: 2xx has no Contact..."` | `ErrRemoteContactMissing` |
| `"no SDP in response"` | `ErrSDPMissing` |
| — | `ErrDecline/ErrBusy/ErrNotFound/ErrResponseSent` and `*ResponseError` for handler-to-response mapping |

## 8. Migration checklist

1. Change the import path to `.../v2`.
2. Update serve handlers to `func(ctx context.Context, d *DialogServerSession) error`.
3. Rename `Invite` -> `Dial`, `InviteBridge` -> `DialBridge` where the helpers
   were used.
4. Add `ctx` to server signaling calls (`Trying`, `Ringing`, `Answer`, ...).
5. Replace `Progress()` with `Trying(ctx)` or the new `Progress(ctx)`.
6. Replace removed option structs with `SignalOption` constructors, and stop
   passing options a method does not consume (they now return
   `*UnsupportedOptionError`).
7. Rewrite media config literals into the `SRTP/Network/SDP` sub-structs and
   drop any manual `SecureRTP`/DTLS mode pairing (now automatic).
8. Replace `DialogCacheServer/Client` usage with `MatchDialog`.
9. Configure at least one transport explicitly (the loopback default is gone).
10. Run the codemod (§9), then `go build ./...` to catch the rest.

Renames that keep the same arity are codemod-able with `gofmt -r`; the handler
signature, option structs, and config literals require manual edits.

## 9. Codemod

`gofmt -r` accepts a single rewrite rule per run and rewrites the whole tree
(`-w .`). Apply one rule at a time and review the diff.

Safe rules:

```sh
gofmt -r 'x.Media() -> x.DialogMedia()' -w .
gofmt -r 'x.WithMediaSDPSessionName(s) -> x.WithSDPSessionName(s)' -w .
gofmt -r 'ServeDialogFunc -> DialogHandler' -w .
```

The `WithMediaSDPSessionName` rule targets qualified calls
(`diago.WithMediaSDPSessionName(...)`, the common form). For dot-imports use
the unqualified variant
`gofmt -r 'WithMediaSDPSessionName(s) -> WithSDPSessionName(s)' -w .`.

`x.Media() -> x.DialogMedia()` rewrites every zero-argument `Media()` call, so
check the diff if your code has an unrelated type with that method.

Do **not** codemod the stable-handle fields with
`x.RTPPacketReader -> x.RTPPacketReader()`: the pattern also matches type
selectors (`media.RTPPacketReader`) and produces invalid code. Rename those
compile-driven instead (`sess.RTPPacketReader` -> `sess.RTPPacketReader()`).

Everything that changes call arity or return shape is not codemod-able:

- `ctx` parameters added to handlers and signaling calls,
- `Invite -> Dial` / `InviteBridge -> DialBridge` (variadic options),
- removed option structs (`InviteOptions`, `AnswerOptions`, ...),
- `CreatePlayback*` now returning pointers,
- `PauseAudioRead/Write` releases now returning `StopFunc`,
- `NewDiago` now returning an error.

Migrate those by running `go build ./...` and fixing the reported call sites
one by one.
