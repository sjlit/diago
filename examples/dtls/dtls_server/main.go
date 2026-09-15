package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"os/signal"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/examples"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/testdata"
)

func main() {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	examples.SetupLogger()

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg, err := diago.New(ua,
		diago.WithTransport(
			diago.Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  16443,
				Media: diago.TransportMediaConfig{
					SRTP: diago.SRTPConfig{ // This enables SRTP DTLS
						Mode: diago.SecureRTPDTLS,
						DTLS: media.DTLSConfig{
							Certificates:     []tls.Certificate{testdata.ServerCertificate()},
							ServerClientAuth: media.ServerClientAuthRequireCert,
						},
					},
				},
			},
		),
		diago.WithMediaConfig(
			diago.MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))
	if err != nil {
		slog.Error("failed to create diago", "error", err)
		return
	}

	err = dg.Serve(ctx, func(ctx context.Context, d *diago.DialogServerSession) error {
		d.Trying(ctx)
		if err := d.Answer(ctx); err != nil {
			panic(err)
		}

		slog.Info("Starting echo")
		err := d.EchoContext(ctx)
		slog.Info("Echo finished with", "error", err)

		return nil
	})
	if err != nil {
		slog.Error("serve stopped", "error", err)
	}
}
