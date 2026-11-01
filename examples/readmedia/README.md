# examples/readmedia

Read the raw decoded media stream from an answered call.

```bash
cd examples && go run ./readmedia
```

Dial in:

```sh
gophone dial -media=speaker sip:123@127.0.0.1
```

The example uses `Media.ReaderWithProps` to resolve the negotiated codec at
read time and prints the decoded PCM frames.
