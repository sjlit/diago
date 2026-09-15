# Diago v2 Migration Guide

Status: draft covering the planned v2 changes. See [v2-plan.md](v2-plan.md)
for the phased work plan. v2 allows breaking changes; this document maps every
user-visible rename, removal, and behavior change.

## 1. Module and initialization

| v1 | v2 | Notes |
|---|---|---|
| `github.com/sjlit/diago` | `github.com/sjlit/diago/v2` | Major-version import path |
| Implicit `127.0.0.1:5060` transport when none configured | `NewDiago` errors; configure with `WithTransport` | Removes a silent loopback default |
| `WithAuth(...)` | Removed | It was a no-op. Incoming calls: `DialogServerSession.Authorize`; outgoing/REGISTER: `WithAuthCredentials` |
| `WithTransport(Transport{...})` | `WithTransport(Transport{...})` with validation | Invalid network values now fail at construction |

## 2. Server handler and lifecycle

| v1 | v2 |
|---|---|
| `Serve(ctx, func(d *DialogServerSession))` | `Serve(ctx, func(ctx context.Context, d *DialogServerSession) error)` |
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
| `SignalParams` exported | Unexported; use the `With*` constructors |
| Custom `SignalOption(func(p *SignalParams))` closures | Not possible (`signalParams` is unexported); use `WithRequestMutator`/`WithResponseMutator` |
| `AnswerOptions` wrapper method | Removed; `Answer(ctx, ...SignalOption)` |

## 5. Media configuration

| v1 | v2 |
|---|---|
| `Transport.MediaSRTP int` (docs said 0/1; 2 meant DTLS) | `Transport.Media.SRTP.Mode SecureRTP` (`SecureRTPNone/SDES/DTLS`); `DTLSConf` non-zero auto-selects DTLS |
| `MediaConfig.SecureRTP int`, `MediaConfig.SecureRTPAlg uint16` | `MediaConfig.SRTP{Mode SecureRTP, Alg SRTPAlg, DTLS DTLSConfig}` |
| `MediaConfig.RTPNAT int` | `NetworkConfig.RTPNAT RTPNATMode` |
| `MediaConfig.BindIP/ExternalIP/RTPPortStart/RTPPortEnd` | `MediaConfig.Network` |
| `MediaConfig.SDPCodecPreferLocalOrder/SDPSessionName` | `MediaConfig.SDP` |
| `WithAudioReaderMediaProps(*MediaProps)` | `ReaderWithProps() (io.Reader, MediaProps, error)` |
| `WithAudioWriterMediaProps(*MediaProps)` | `WriterWithProps() (io.Writer, MediaProps, error)` |

## 6. Public surface

| v1 | v2 |
|---|---|
| `DialogCacheServer()/DialogCacheClient()` | Removed; use `MatchDialog(req)` |
| `DialogCache`, `DialogCachePool`, `DialogData` exported | Unexported/removed |
| `DialogServerSession.ReadAck` | Unexported (framework internal) |
| `DialogMedia.Media()` | `DialogMedia()` (method renamed on `DialogSession` too) |
| `DialogMedia.RTPPacketReader/RTPPacketWriter` fields | Unexported; use accessors |
| `AudioStereoRecordingCreate` | `StartStereoRecording` |
| `CreatePlayback/CreatePlaybackControl/CreateRingtonePlayback` return values | Return pointers |
| `ListenBackground` stop `func() error`, `PauseAudioRead/Write` `func()`, `ProxyMediaControl` `func() error` | Unified `StopFunc func() error` |
| `Bridger` defined but never accepted | `DialBridge` accepts `Bridger` |
| `diagotest.NewConnRecorder() *connRecorder` | `NewConnRecorder() *ConnRecorder` |

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
10. Run the codemod for renames, then `go build ./...` to catch the rest.

Renames are codemod-able with `gofmt -r`; the handler signature, option
structs, and config literals require manual edits.
