Run playback as server
```bash
cd examples && go run ./playback_control
```

Dial in and you should:
- Hear audio
- Audio muted
- Audio umuted
- Audio stopped
```sh 
gophone dial -media=speaker sip:112@127.0.0.1
```
