// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/rand"
	"math/big"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run command
// GOMAXPROCS=20 TEST_INTEGRATION=1 go test -bench=BenchmarkIntegrationClientServer -run $^ -benchmem -v . -benchtime=50x
func BenchmarkIntegrationClientServer(t *testing.B) {
	if os.Getenv("TEST_INTEGRATION") == "" {
		t.Skip("Use TEST_INTEGRATION env value to run this test")
		return
	}

	testCases := []struct {
		transport  string
		serverAddr string
		encrypted  bool
	}{
		{transport: "udp", serverAddr: "127.1.1.100:5060"},
		// {transport: "tcp", serverAddr: "127.1.1.100:5060"},
		// {transport: "ws", serverAddr: "127.1.1.100:5061"},
		// {transport: "tls", serverAddr: "127.1.1.100:5062", encrypted: true},
		// {transport: "wss", serverAddr: "127.1.1.100:5063", encrypted: true},
	}

	ctx, shutdown := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	t.Cleanup(func() {
		shutdown()
		wg.Wait()
	})

	tran := Transport{
		Network:  "udp",
		BindHost: "127.1.1.100",
		BindPort: 5060,
	}
	ua, _ := sipgo.NewUA()
	defer ua.Close()
	srv, err := New(WithUserAgent(ua), WithTransport(tran))
	require.NoError(t, err)

	err = srv.ServeBackground(ctx, func(_ context.Context, d *InboundCall) error {
		wg.Add(1)
		defer wg.Done()

		ctx := d.Context()

		err := d.Answer(ctx)
		if err != nil {
			t.Log(err.Error())
			return nil
		}

		pb, err := d.NewPlayback()
		if err != nil {
			t.Log(err.Error())
			return nil
		}
		_, err = pb.PlayFileContext(context.Background(), "./testdata/files/demo-echodone.wav")
		if err != nil {
			t.Log(err.Error())
			return nil
		}

		err = d.Hangup(ctx)
		if err != nil {
			t.Log(err.Error())
		}
		return nil
	})
	require.NoError(t, err)

	var maxInvitesPerSec chan struct{}
	maxInvitesPerSec = make(chan struct{}, 100)
	if v := os.Getenv("MAX_REQUESTS"); v != "" {
		t.Logf("Limiting number of requests: %s req/s", v)
		maxInvites, _ := strconv.Atoi(v)
		maxInvitesPerSec = make(chan struct{}, maxInvites)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(1 * time.Second):
					for i := 0; i < maxInvites; i++ {
						<-maxInvitesPerSec
					}
				}
			}
		}()
	}

	for _, tc := range testCases {
		t.Run(tc.transport, func(t *testing.B) {
			t.ResetTimer()
			t.ReportAllocs()

			t.RunParallel(func(p *testing.PB) {
				// Build UAC
				ua, _ := sipgo.NewUA()
				defer ua.Close()
				phone, err := New(WithUserAgent(ua), WithDefaultTransport())
				require.NoError(t, err)
				id, _ := rand.Int(rand.Reader, big.NewInt(int64(runtime.GOMAXPROCS(0))))
				for p.Next() {
					t.Log("Making call goroutine=", id)
					// If we are running in limit mode
					if maxInvitesPerSec != nil {
						maxInvitesPerSec <- struct{}{}
					}
					dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					dialog, err := phone.DialURI(dialCtx, sip.Uri{Host: tran.BindHost, Port: tran.BindPort, User: "dialer"})
					cancel()

					require.NoError(t, err)

					mediapkts := 0
					outoforder := 0
					audioReader, _ := dialog.AudioReader()
					wg.Add(1)
					go func() {
						defer wg.Done()

						buf := make([]byte, media.RTPBufSize)
						var prevSeq uint16
						reader := dialog.rtpPacketReader
						for {
							_, err := audioReader.Read(buf)
							// _, err := reader.ReadRTP(buf, &p)
							if err != nil {
								return
							}
							p := reader.PacketHeader
							if prevSeq > 0 && p.SequenceNumber != prevSeq+1 {
								outoforder++
							}
							mediapkts++
							prevSeq = p.SequenceNumber
						}
					}()

					clientHangup := false
					select {
					// Audio is 2 sec long
					case <-time.After(3 * time.Second):
						dialog.Hangup(context.TODO())
						clientHangup = true
					case <-dialog.Context().Done():
					}
					dialog.Close()
					assert.Empty(t, outoforder, "Out of order media detected")
					assert.True(t, clientHangup)
					assert.Greater(t, mediapkts, int(2*time.Second/(20*time.Millisecond))-10, "Not enough received packets")
				}
			})

			t.ReportMetric(float64(t.N)/t.Elapsed().Seconds(), "req/s")
		})
	}
}
