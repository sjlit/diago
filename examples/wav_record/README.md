Run record as server
```bash
cd examples && go run ./wav_record
```

Dial in and after terminating call you should find recording under 
`/tmp/diago_record_<callid>.wav`

```sh 
gophone dial -media=file=demo_echotest.wav sip:112@127.0.0.1
```
