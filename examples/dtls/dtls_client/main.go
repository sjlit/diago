package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/examples"
	"github.com/sjlit/diago/v2/media"
)

func main() {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	examples.SetupLogger()

	ua, _ := sipgo.NewUA()
	dg, err := diago.NewDiago(ua,
		diago.WithTransport(
			diago.Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  16441,
				Media: diago.TransportMediaConfig{
					SRTP: diago.SRTPConfig{Mode: diago.SecureRTPDTLS}, // USE DTLS
				},
			},
		),
		diago.WithMediaConfig(
			diago.MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))
	if err != nil {
		panic(err)
	}

	d, err := dg.Dial(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 16443}, diago.WithDialogTransport("tcp"))
	if err != nil {
		panic(err)
	}
	defer d.Close()
	defer d.Hangup(d.Context())

	ulaw := make([]byte, 160)

	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	time.Sleep(1 * time.Second)

	go func() {
		audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))
		reader := bytes.NewReader(ulaw)
		for {
			// reader.Reset()
			reader.Seek(0, 0)
			_, err = media.Copy(reader, w)
			if err != nil && !errors.Is(err, io.EOF) {
				slog.Error("Failed to copy", "error", err)
				return
			}

			recv := make([]byte, 160)
			if _, err := r.Read(recv); err != nil {
				slog.Error("Failed to read", "error", err)
				return
			}

			slog.Info("Echo received")
			if !bytes.Equal(ulaw, recv) {
				slog.Error("Send is not received")
			}
			time.Sleep(1 * time.Second)
		}
	}()
	<-ctx.Done()
}
