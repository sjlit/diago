Run the N-party mixer: every caller that dials in is added to the same
`BridgeMix`.
```bash
cd examples && go run ./bridge_mix
```

Dial the mixer from two or more softphones:

```sh
gophone dial -ua alice sip:112@127.0.0.1
gophone dial -ua bob sip:112@127.0.0.1
```

Once two callers are joined, audio is mixed between all legs; each leg is
removed when its call ends.
