# examples/dtls

DTLS-SRTP client and server pair. Run the server first, then the client:

```bash
cd examples && go run ./dtls/dtls_server
```

In another terminal:

```bash
cd examples && go run ./dtls/dtls_client
```

Both programs use the test certificates embedded in the `testdata` package.
The server echoes received audio; the client sends a short tone and checks the
echo.
