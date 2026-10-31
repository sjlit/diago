// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/require"
)

func TestIntegrationPlaybackURL(t *testing.T) {
	skipShort(t)
	// Create transaction users, as many as needed.
	ua, _ := sipgo.NewUA(
		sipgo.WithUserAgent("inbound"),
	)
	defer ua.Close()
	tu, err := newAgent(withUserAgent(ua), withTransport(
		Transport{
			Network:  "udp",
			BindHost: "127.0.0.1",
			BindPort: 0,
		},
	))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// media.RTPDebug = true

	urlStr := testStartAudioStreamServer(t)

	var errServer error
	wg := sync.WaitGroup{}
	wg.Add(1)
	var handled atomic.Bool
	err = tu.ServeBackground(ctx, func(ctx context.Context, in *InboundCall) error {
		// Stray retransmissions from other tests used to reach this fixed
		// port; only the first dialog is the one under test.
		if !handled.CompareAndSwap(false, true) {
			return ErrBusy
		}
		defer wg.Done()
		in.Trying(ctx)
		in.Ringing(ctx)
		in.Answer(ctx)
		t.Log("Playing url ", urlStr)
		pb, _ := in.NewPlayback()
		if _, err := pb.PlayURLContext(context.Background(), urlStr); err != nil {
			errServer = errors.Join(errServer, err)
		}

		t.Log("Done playing", urlStr)
		in.Close(in.Context())
		return nil
	})
	require.NoError(t, err)
	srvPort := tu.transports[0].BindPort
	require.NotZero(t, srvPort)

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()
		phone, err := newAgent(withUserAgent(ua), withTransport(Transport{
			Network:  "udp",
			BindHost: "127.0.0.1",
			BindPort: 0,
		}))
		require.NoError(t, err)
		// Just to have handled BYE
		err = phone.ServeBackground(context.TODO(), func(ctx context.Context, d *InboundCall) error {
			return nil
		})
		require.NoError(t, err)

		dialog, err := phone.DialURI(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: srvPort})
		require.NoError(t, err)
		defer dialog.release()

		rtpReader := dialog.rtpPacketReader

		go func() {
			defer dialog.release()
			// Playback duration, not a wait: hold the call open so the
			// HTTP-backed stream keeps being fetched and copied.
			time.Sleep(10 * time.Second)
			dialog.Close(ctx)
		}()
		b := bytes.NewBuffer([]byte{})
		written, err := media.CopyWithBuf(rtpReader, b, make([]byte, media.RTPBufSize))
		// bnf, err := io.ReadAll(rtpReader)
		require.ErrorIs(t, err, io.EOF)
		require.Greater(t, written, int64(10000))
		require.Greater(t, b.Len(), 10000)
	}

	t.Log("Waiting server goroutine to exit")
	wg.Wait()
	require.NoError(t, errServer)
}

func testStartAudioStreamServer(t *testing.T) string {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(200)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		fh, err := os.Open("testdata/files/demo-echodone.wav")
		if err != nil {
			return
		}

		// Get file info
		fileInfo, err := fh.Stat()
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Add("content-type", "audio/wav")
		t.Logf("Serving file %q", fh.Name())
		http.ServeContent(w, req, "audio/wav", fileInfo.ModTime(), fh)
	})

	srv := http.Server{
		Addr:    "127.0.0.1:18080",
		Handler: mux,
	}

	l, err := net.Listen("tcp", srv.Addr)
	require.NoError(t, err)
	go srv.Serve(l)

	t.Cleanup(func() {
		srv.Shutdown(context.TODO())
	})
	return "http://" + srv.Addr + "/"
}
