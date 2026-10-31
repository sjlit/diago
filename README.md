# diago

SIP user agent library for building B2BUA and media server applications on top
of [sipgo](https://github.com/emiago/sipgo): inbound/outbound calls, RTP media
with playback/recording/DTMF, bridging and REGISTER.

Module: `github.com/sjlit/diago/v2`.

## Install

```sh
go get github.com/sjlit/diago/v2
```

Requires Go 1.23+.

## Quickstart

Inbound call: ring, answer, keep the call up while the handler runs.

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/sjlit/diago/v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// No sipgo UserAgent needed: New creates a private one and Close owns it.
	// (Config.UserAgent supplies a shared, caller-owned UserAgent instead.)
	agent, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		panic(err)
	}
	defer agent.Close(context.Background())

	err = agent.Serve(ctx, func(ctx context.Context, call *diago.InboundCall) error {
		if err := call.Ringing(ctx); err != nil {
			return err
		}
		if err := call.Answer(ctx); err != nil {
			return err
		}
		// Returning after Answer keeps the call alive: the framework closes
		// the dialog and its media when the peer hangs up. Block here while
		// the call is in progress (playback, DTMF, recording, ...).
		<-call.Context().Done()
		return nil
	})
	if err != nil {
		slog.Error("Serve stopped", "error", err)
	}
}
```

Outbound call:

```go
call, err := agent.Dial(ctx, "sip:alice@127.0.0.1:5060")
if err != nil {
	return err
}
defer call.Close()

// ... use call.Media() for playback, DTMF, recording, ...

if err := call.Close(ctx); err != nil {
	return err
}
```

See [`examples/`](examples) for runnable programs (playback, recording, DTMF,
bridge, bridge mix, MoH, TLS/DTLS, REGISTER, ...).

## Key concepts

- **CallHandler**: `Serve` installs one handler per inbound dialog. A handler
  that returns before sending a final response declines the pending INVITE
  (`nil` → 480, or the mapped response for an error: `ErrDecline` → 603,
  `ErrTemporarilyUnavailable` → 480, `ErrBusy` → 486, `ErrNotFound` → 404,
  `*ResponseError` → custom, anything else → 500). **Answering and returning keeps the call alive**; an answered
  call ends with BYE carrying an RFC 3326 `Reason` header when the handler
  returns an error, or when the peer hangs up. Wrap `ErrResponseSent` when the
  handler already wrote its own final response.
- **Calls**: `InboundCall` (UAS) and `OutboundCall` (UAC) share the `Call`
  interface. `Dial` accepts a SIP URI string; `DialURI` takes a `diago.URI`.
  `Close` performs the signaling teardown (BYE/CANCEL/decline as appropriate)
  and releases local resources; it is idempotent.
- **Lifecycle**: `Agent.Close` hangs up tracked dialogs, unregisters the
  registrations it created, stops the listeners
  started by `Serve`/`Start` and closes the private UserAgent. A
  UserAgent supplied with `Config.UserAgent` (and servers/clients from
  `Config.Server`/`Config.Client`) stay caller-owned.
- **Media**: `call.Media()` is the single owner of the media session.
  Components (`NewPlayback`, `StartStereoRecording`, `StartMusicOnHold`,
  `SendDTMF`, `PlayTone`, ...) resolve the reader/writer at use time, so re-INVITEs
  hot-swap the internals without replacing installed handles. Media entry
  points return `ErrDialogNotAnswered`/`ErrDialogClosed` instead of panicking.
- **Answer/ACK**: `Answer`, `AnswerLate` and `RespondSDP` return once the 2xx
  was handed to the transaction layer; the ACK wait continues in the
  background. Use `WaitAck` (or `AckReceived`/`AckErr`) when the call must be
  fully established before continuing.
- **Options**: each API takes the option interface for its operation
  (`AnswerOption`, `CloseOptions`, `InviteOption`, `DialOption`,
  `RegisterOption`, ...); passing an option an API does not consume is a
  compile error. Constructors validate their values at call time; failures
  wrap `ErrInvalidOption`.
- **Configuration**: construction-time media settings live in `MediaConfig`
  (`Codecs`, `SRTP`, `Network`, `SDP`, `MusicOnHold`, `Logger`, `Debug`,
  `Tracer`, `HTTPClient`). Transports can select a complete named profile with
  `Config.MediaProfiles` + `Transport.Profile`; there is no package-level mutable
  configuration.
- **REGISTER**: `Agent.Register` returns a `*Registration` handle
  (`WaitReady`/`Wait`/`Close`) that keeps the registration refreshed and
  unregisters on `Close`; `Agent.Close` does the same for every registration
  it created unless `WithRegistrationDetached` is passed. `RegisterTransaction`
  exposes single attempts (`Register`/`Qualify`/`Unregister`).
- **sipgo interop**: sipgo types in signatures are aliases (`diago.URI`,
  `diago.Header`, `diago.Request`, `diago.Response`, ...), so importing sipgo
  is optional. Escape hatches: `Config.UserAgent`, `Config.Server`, `Config.Client`,
  `Agent.Server`, `Agent.UA`, `call.SIP`.
- **Background helpers**: `ListenBackground`, `AudioRingtone.PlayBackground`,
  `PauseAudioRead/Write` and `Bridge.ProxyMediaControl` return `StopFunc`
  (`func() error`).

Package docs: `go doc github.com/sjlit/diago/v2`; API conventions:
[API.md](API.md).

## Migrating from v1

v1 imported `github.com/sjlit/diago`; v2 is `github.com/sjlit/diago/v2`.

| v1 | current |
| --- | --- |
| `diago.NewDiago(ua, opts...) *Diago` | `diago.New(Config) (*Agent, error)`; `ua` is optional (`Config.UserAgent`) |
| implicit loopback transport | `Config.Transports` (`DefaultTransport()` for the loopback default) |
| `ServeDialogFunc func(d)` | `CallHandler func(ctx, *InboundCall) error` (error → SIP response) |
| `HandleFunc(f)` | handler is passed to `Serve`/`Start` |
| `Invite` / `InviteBridge` | `Dial` / `DialBridge` (string target; `DialURI` for a parsed URI) |
| `WithAuth` | removed (was unused); use `WithAuthCredentials` or `DigestAuthServer` |
| `InviteOptions` / `NewDialogOptions` / `ProgressMediaOptions` / `AnswerOptions` | removed; per-operation option interfaces |
| `Answer/Progress/...` without ctx | all signaling methods take `ctx` |
| `WaitAnswer(ctx, sipgo.AnswerOptions)` | `WaitAnswer(ctx, ...WaitAnswerOption)` |
| `MediaConfig.SecureRTP/SecureRTPAlg/BindIP/ExternalIP/RTPNAT/DTLSConf/...` | grouped into `MediaConfig.SRTP/Network/SDP` |
| `Transport.MediaSRTP/MediaExternalIP/MediaDTLSConf` | `Config.MediaProfiles` + `Transport.Profile` |
| `AudioStereoRecordingCreate` | `Media.StartStereoRecording` |
| `DialogCacheServer/Client`, cache `MatchDialog*` | removed; `Agent.FindCall(req)` |
| `Bridge.GetDialogs` | removed; track the sessions you add (`Bridge.Add/Remove`) |
| `dg.serveHandler`/`DialogStore`-style internals | unexported |

Breaking changes since the first v2 preview (same module path):

| first v2 preview | current |
| --- | --- |
| `Diago`, `Shutdown` | `Agent`, `Close` |
| `DialogClientSession` / `DialogServerSession` | `OutboundCall` / `InboundCall` |
| `DialogMedia` / `DialogHandler` | `Media` / `CallHandler` |
| `MatchDialog` / `NewDialog` | `FindCall` / `NewCall` |
| `HangupAndClose` | removed: `Close(ctx)` performs teardown and release |
| `WithHeader` | removed: `WithHeaders(sip.NewHeader(name, value))` |
| `CreatePlayback*` / `CreateDTMF*` / `CreateRingtonePlayback` | `NewPlayback*` / `NewDTMF*` / `NewRingtone` |
| `PlayMusicOnHold` | `StartMusicOnHold` |
| `WithAudioReader*` / `WithAudioWriter*` | `WithReader*` / `WithWriter*` |
| `Transport.Transport` | `Transport.Network` |
| runtime option scope (`UnsupportedOptionError`) | compile-time option interfaces |
| handler return hangs up the call | answering then returning keeps it alive |
| `Answer` blocks until ACK | returns after the 2xx; `WaitAck` opt-in |
| `Register(...) error` | `Register(...) (*Registration, error)` |

Behavioral notes:

- `WithOnRefer`/`WithOnMediaUpdate` are accepted by `Answer`, `AnswerLate`,
  `Invite` and `Dial`.
- REGISTER: 423 Interval Too Brief is retried with `Min-Expires`; transient
  failures honor `Retry-After` once; transport failures are retried until ctx
  is canceled (or `Registration.Close`).
- Server `Close` on a still-pending INVITE sends the 480 decline even when the
  passed ctx is already canceled.
- `Agent.Close` unregisters the REGISTER handles created by `Register` and
  `RegisterURI` (opt out with `WithRegistrationDetached`). Registering after
  `Close` returns `ErrAgentClosed`.

## License

MPL-2.0. See [LICENSE.txt](LICENSE.txt).
