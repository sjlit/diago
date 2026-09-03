# examples/moh

Music on Hold example.

Build and run:

```
go run ./examples/moh
```

Dial `sip:alice@127.0.0.1`. Once the call is up:

- `1` → Hold; `MediaConfig.MusicOnHold` (a 425Hz tone at 30ms cadence) starts
  looping automatically after the hold re-INVITE succeeds.
- `2` → Unhold; the hold music stops.

Manual control (start a goroutine anywhere after `Answer`):

```go
inDialog.PlayMusicOnHold(ctx, diago.WithMoHTone(customTone))
// ...
inDialog.StopMusicOnHold()
```

`PlayMusicOnHold`/`StopMusicOnHold` can also be driven by an out-of-band
trigger (DTMF event, web hook, etc.). The dialog holds at most one music
loop — a second start returns `ErrMusicOnHoldActive`.