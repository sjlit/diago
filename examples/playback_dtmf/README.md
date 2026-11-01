# examples/playback_dtmf

Playback with DTMF control.

```bash
cd examples && go run ./playback_dtmf
```

Dial in:

```sh
gophone dial -media=audio sip:123@127.0.0.1
```

While the prompt plays, press `*` to replay it from the beginning. Pressing any
other key ends the call.
