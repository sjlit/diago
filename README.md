<img src="icons/diago-icon-text.svg" width="300" alt="DIAGO">

[![Go Report Card](https://goreportcard.com/badge/github.com/sjlit/diago)](https://goreportcard.com/report/github.com/sjlit/diago)
![Coverage](https://img.shields.io/badge/coverage-60.6%25-blue)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/sjlit/diago)

Short of **dialog + GO**.  
**Library for building VOIP solutions in GO!**

Built on top of optimized [SIPgo library](https://github.com/emiago/sipgo)!  
In short it allows developing fast and easy testable VOIP apps to handle calls, registrations and more... 
It is offering High Level APIs to make **telephony easier** while you can go low and control every packet if needed.

**Media state ownership and dialog lifecycle contracts are documented in [docs/contracts.md](docs/contracts.md).**

> This branch hosts **diago v2**. It is a breaking release: the module path
> becomes `github.com/sjlit/diago/v2`, transports must be configured
> explicitly, serve handlers return errors, and the media configuration moved
> into grouped sub-structs. See [docs/v2-migration.md](docs/v2-migration.md)
> for the full v1 -> v2 mapping and [docs/v2-plan.md](docs/v2-plan.md) for the
> phased plan.

*Diago is mainly project driven lib, so lot of API design will/should be challenged with real working apps needs*

**For more information and documentation visit [the website](https://emiago.github.io/diago/docs)**

Quick links:
- [Getting started](https://emiago.github.io/diago/docs/getting_started/)
- [Demo Examples](https://emiago.github.io/diago/docs/examples/)
- [API Docs](https://emiago.github.io/diago/docs/api_docs/)
- [GO Docs](https://pkg.go.dev/github.com/sjlit/diago/v2)
- [Roadmap](https://emiago.github.io/diago/docs/#core-roadmap)

## Media security (SDES / DTLS-SRTP)

`MediaConfig.SRTP.Mode` selects plain RTP (`SecureRTPNone`), SDES
(`SecureRTPSDES`) or DTLS-SRTP (`SecureRTPDTLS`); a non-zero
`MediaConfig.SRTP.DTLS` configuration auto-selects DTLS.
DTLS-SRTP verifies the peer certificate against every `a=fingerprint` in the
remote SDP (RFC 5763) and **fails closed** on mismatch: an offer/answer that
does not match the presented certificate aborts the media instead of being
silently accepted (behavior change after v0.0.5).

When diago acts as the DTLS **server** (it sent the offer
and the remote answered `setup:active` — the usual UAC path), client
certificates are requested automatically whenever the remote SDP carries
`a=fingerprint` (RFC 5763 §5). `media.ServerClientAuthRequireCert` maps to
`RequireAnyClientCert`. SDP without any `a=fingerprint` is rejected for
DTLS-SRTP instead of skipping verification.
The secure-media contract is documented in
[docs/contracts.md §14](docs/contracts.md).

*If you find this project useful and you want to support/sponzor or need help with your projects, you can contact me more on*
[mail](mailto:emirfreelance91@gmail.com).

Follow me on [X/Twitter](https://twitter.com/emiago123) for regular updates

**Tools/Service developed with diago:**
- <img width="20" src="https://github.com/sjlit/diagox/raw/main/images/diagox-icon-blue.png"> [github.com/sjlit/diagox](https://github.com/sjlit/diagox) simple Ingress/Egress and Registrar for SIP/RTP scaling
- <img width="20" src="https://github.com/emiago/gophone/raw/main/images/g2.png"> [github.com/emiago/gophone](https://github.com/emiago/gophone) CLI softphone for easy testing 

## RFCS

SIP: [RFC 3261](https://datatracker.ietf.org/doc/html/rfc3261)|[RFC3581](https://datatracker.ietf.org/doc/html/rfc3581)|[RFC6026](https://datatracker.ietf.org/doc/html/rfc6026)
> More refer to lib [github.com/emiago/sipgo](https://github.com/emiago/sipgo)
Full dialog control (client/server), Registering, Authentication ...

Digest authentication (server-side INVITE challenge/validate): [RFC 2617](https://datatracker.ietf.org/doc/html/rfc2617)

SDP: [RFC8866](https://datatracker.ietf.org/doc/html/rfc8866). 
> Parsing + Auto Generating for media session/audio

RTP/AVP: 
[RFC3550](https://datatracker.ietf.org/doc/html/rfc3550)
> RTP Packetizers, Media Forking, RTP Session control, RTCP Sender/Receiver reports, RTCP statistics tracking, DTMF reader/writer ...


REFER:
- [RFC3515](https://datatracker.ietf.org/doc/html/rfc3515)
- [RFC3892](https://datatracker.ietf.org/doc/html/rfc3892)

**NOTE**: For specifics and questions what is covered by RFC, please open Issue. Lot of functionality can be extended even if not built in library. 

## Contributions

Please avoid following:
- Creating BIG PR that creates feature or lot of refactoring without previously having Issue. Issue should explain problem or requirements you want to accomplish.
- Creating Change Log or some other textual files that add some sort of documentation. This is why we need Issue and ID of issue should be in your commit. If you want(think it is good) to have this better documented(like webpage or readme) pls open issue.
- English is main language for code and for comments. Any other language used PRs will be rejected.

## Usage 

Checkout more on [Getting started](https://emiago.github.io/diago/docs/getting_started/), but for quick view here is echotest (hello world) example.
```go 
ua, _ := sipgo.NewUA()
defer ua.Close()

dg, err := diago.NewDiago(ua, diago.WithDefaultTransport()) // 127.0.0.1:5060
if err != nil {
	log.Fatal(err)
}

ctx := context.Background()
err = dg.Serve(ctx, func(ctx context.Context, inDialog *diago.DialogServerSession) error {
	inDialog.Trying(ctx) // 100 Trying
	// Answer -> 200 OK with SDP
	if err := inDialog.Answer(ctx); err != nil {
		return err
	}

	// Make sure file below exists in work dir
	playfile, err := os.Open("demo-echotest.wav")
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer playfile.Close()

	// Create playback and play file.
	pb, err := inDialog.CreatePlayback()
	if err != nil {
		return err
	}
	if _, err := pb.Play(playfile, "audio/wav"); err != nil {
		return fmt.Errorf("playing failed: %w", err)
	}
	return nil
})
if err != nil {
	log.Fatal(err)
}
```

See more [examples in this repo](/examples)
### Tracing SIP, RTP

While openning issue, consider having some traces enabled.

```go
sip.SIPDebug = true // Enables SIP tracing
media.RTCPDebug = true // Enables RTCP tracing
media.RTPDebug = true // Enables RTP tracing. NOTE: It will dump every RTP Packet
```
