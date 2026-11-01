# examples/auth_server

Digest-authenticated inbound calls.

```bash
cd examples && go run ./auth_server
```

Dial with credentials:

```sh
gophone dial -media=speaker -authuser=alice -authpass=wonderland sip:123@127.0.0.1
```

Calls without valid credentials receive `401 Unauthorized` with a
`WWW-Authenticate` challenge and never reach the answer path.
