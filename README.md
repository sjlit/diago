# diago

SIP user agent library for building B2BUA and media server applications on top
of [sipgo](https://github.com/emiago/sipgo).

This is the **v2** module (`github.com/sjlit/diago/v2`). It is a breaking
release: see [Migrating from v1](#migrating-from-v1).

## Install

```sh
go get github.com/sjlit/diago/v2
```

Requires Go 1.23+.

## Quickstart

Inbound call: answer, read audio, hang up when the handler returns.

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	// v2 has no implicit transport: configure at least one.
	dg, err := diago.New(ua, diago.WithDefaultTransport())
	if err != nil {
		panic(err)
	}

	err = dg.Serve(ctx, func(ctx context.Context, d *diago.DialogServerSession) error {
		if err := d.Trying(ctx); err != nil {
			return err
		}
		if err := d.Ringing(ctx); err != nil {
			return err
		}
		if err := d.Answer(ctx); err != nil {
			return err
		}

		// d.DialogMedia() owns the media pipeline: playback, recording,
		// DTMF, MoH. Block until the call ends.
		<-d.Context().Done()
		return nil
	})
	if err != nil {
		slog.Error("Serve stopped", "error", err)
	}
}
```

Outbound call:

```go
d, err := dg.Dial(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060})
if err != nil {
	return err
}
defer d.HangupAndClose(ctx)
```

See [`examples/`](examples) for runnable programs (playback, recording, DTMF,
bridge, bridge mix, MoH, TLS/DTLS, REGISTER, ...).

## Key concepts

- **DialogHandler** (`diago.DialogHandler`): `Serve` installs one handler per
  inbound dialog. Returning `nil` hangs the call up (BYE once confirmed,
  otherwise a 480 decline). Returning an error maps it to a final response
  (`ErrDecline` → 480, `ErrBusy` → 486, `ErrNotFound` → 404, `*ResponseError`
  → custom, anything else → 500); a confirmed call is ended with BYE + an
  RFC 3326 `Reason` header. Wrap `ErrResponseSent` when the handler already
  wrote its own final response.
- **Serve lifecycle**: `Serve` returns only after every listener stopped, and
  joins the listener errors. `Shutdown` hangs up tracked dialogs and stops the
  listeners; the `sipgo.UserAgent` is caller-owned and never closed.
  `ServeBackground` is the non-blocking variant.
- **DialogMedia**: single owner of the active media session. Components resolve
  the reader/writer at use time through `DialogMedia`, so re-INVITEs hot-swap
  the internals without replacing installed handles. Media entry points return
  `ErrDialogNotAnswered`/`ErrDialogClosed` instead of panicking.
- **MediaConfig**: construction-time media settings (`Codecs`, `SRTP`,
  `Network`, `SDP`, `MusicOnHold`, `Logger`). Transport-scoped overrides live in
  `Transport.Media`. Zero fields inherit the documented defaults.
- **StopFunc** (`func() error`): teardown handle returned by background media
  helpers (`ListenBackground`, `PlayBackground`, `PauseAudioRead/Write`,
  `ProxyMediaControl`). It is idempotent unless documented otherwise.

Package-level docs: `go doc github.com/sjlit/diago/v2`.

## Migrating from v1

v1 imported `github.com/sjlit/diago`; v2 is `github.com/sjlit/diago/v2`.

| v1 | v2 |
| --- | --- |
| `diago.NewDiago(ua, opts...) *Diago` | `diago.New(ua, opts...) (*Diago, error)` — fails without a transport |
| implicit loopback transport | `WithDefaultTransport()` or `WithTransport(...)` |
| `ServeDialogFunc func(d)` | `DialogHandler func(ctx, d) error` (error → SIP response) |
| `HandleFunc(f)` | handler is passed to `Serve`/`ServeBackground` |
| `Invite` / `InviteBridge` | `Dial` / `DialBridge` (accepts a `Bridger`; nil-safe) |
| `WithAuth` | removed (was unused); use `WithAuthCredentials` or server-side `DigestAuthServer` |
| `InviteOptions` / `NewDialogOptions` / `ProgressMediaOptions` / `AnswerOptions` | removed; use `SignalOption`s |
| `Answer/Progress/...` without ctx | all signaling methods take `ctx` |
| `WaitAnswer(ctx, sipgo.AnswerOptions)` | `WaitAnswer(ctx, ...SignalOption)` |
| `MediaConfig.SecureRTP/SecureRTPAlg/BindIP/ExternalIP/RTPNAT/DTLSConf/...` | grouped into `MediaConfig.SRTP/Network/SDP` |
| `Transport.MediaSRTP/MediaExternalIP/MediaDTLSConf` | `Transport.Media` (`TransportMediaConfig`) |
| `AudioStereoRecordingCreate` | `DialogMedia.StartStereoRecording` |
| `DialogCacheServer/Client`, `MatchDialog*` on the cache | removed; `Diago.MatchDialog(req)` remains |
| `Bridge.GetDialogs` | removed; track the sessions you add (the bridge has `AddDialogSession/RemoveDialogSession`) |
| `dg.serveHandler`/`DialogStore`-style internals | unexported |

Behavioral notes:

- `WithOnRefer`/`WithOnMediaUpdate` are accepted by `Answer` **and**
  `AnswerLate`; other APIs reject unsupported options with
  `*UnsupportedOptionError` instead of silently ignoring them.
- REGISTER: 423 Interval Too Brief is retried with `Min-Expires`; a transient
  failure no longer pins the re-registration interval (Retry-After is consumed
  once). Transport failures are retried until ctx is canceled.
- `Hangup` sends the 480 decline even when the passed ctx is already canceled.

## License

MPL-2.0. See [LICENSE.txt](LICENSE.txt).
