// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/siptest"
)

func TestParseDTMFRelay(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		dtmf    rune
		dur     time.Duration
		wantErr bool
	}{
		{name: "lf", body: "Signal=1\nDuration=160", dtmf: '1', dur: 160 * time.Millisecond},
		{name: "crlf", body: "Signal=8\r\nDuration=120\r\n", dtmf: '8', dur: 120 * time.Millisecond},
		{name: "case-insensitive", body: "signal=*\nduration=100", dtmf: '*', dur: 100 * time.Millisecond},
		{name: "hash", body: "Signal=#\nDuration=0", dtmf: '#', dur: 0},
		{name: "letter-lower", body: "Signal=a\nDuration=100", dtmf: 'a', dur: 100 * time.Millisecond},
		{name: "letter-upper", body: "Signal=D\nDuration=100", dtmf: 'd', dur: 100 * time.Millisecond},
		{name: "no-duration", body: "Signal=5", dtmf: '5', dur: 0},
		{name: "extra-fields", body: "Signal=5\nDuration=100\r\nFoo=bar", dtmf: '5', dur: 100 * time.Millisecond},
		{name: "empty", body: "", wantErr: true},
		{name: "no-signal", body: "Duration=160", wantErr: true},
		{name: "invalid-signal", body: "Signal=x\nDuration=160", wantErr: true},
		{name: "multi-char-signal", body: "Signal=12\nDuration=160", wantErr: true},
		{name: "invalid-duration", body: "Signal=1\nDuration=abc", wantErr: true},
		{name: "negative-duration", body: "Signal=1\nDuration=-5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dtmf, dur, err := parseDTMFRelay([]byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got dtmf=%q dur=%v", dtmf, dur)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dtmf != tt.dtmf {
				t.Fatalf("expected dtmf=%q, got %q", tt.dtmf, dtmf)
			}
			if dur != tt.dur {
				t.Fatalf("expected dur=%v, got %v", tt.dur, dur)
			}
		})
	}
}

func newInfoDTMFRequest(t *testing.T, body string) (*sip.Request, *siptest.ServerTxRecorder) {
	t.Helper()
	recipient := sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: 5060, User: "test"}
	req, err := diagotestSafeNewRequest(sip.INFO, recipient)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBody([]byte(body))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/dtmf-relay"))
	tx := siptest.NewServerTxRecorder(req)
	return req, tx
}

func diagotestSafeNewRequest(method sip.RequestMethod, recipient sip.Uri) (*sip.Request, error) {
	req := sip.NewRequest(method, recipient)
	ua, err := sipgo.NewUA()
	if err != nil {
		return nil, err
	}
	cli, err := sipgo.NewClient(ua, sipgo.WithClientAddr("127.0.0.1:11111"))
	if err != nil {
		return nil, err
	}
	return req, sipgo.ClientRequestBuild(cli, req)
}

func TestReadSIPInfoDTMFDeliversToReader(t *testing.T) {
	var got atomic.Int32
	media := DialogMedia{}
	media.registerDTMFReader(&DTMFReader{
		onDTMF: func(dtmf rune) error {
			if dtmf != '5' {
				t.Errorf("expected '5', got %q", dtmf)
			}
			got.Add(1)
			return nil
		},
	})

	req, tx := newInfoDTMFRequest(t, "Signal=5\r\nDuration=160\r\n")
	if err := readSIPInfoDTMF(&media, req, tx); err != nil {
		t.Fatalf("readSIPInfoDTMF failed: %v", err)
	}

	res := tx.Result()
	if len(res) != 1 || res[0].StatusCode != sip.StatusOK {
		t.Fatalf("expected single 200 OK response, got %+v", res)
	}
	if got.Load() != 1 {
		t.Fatal("DTMF callback was not invoked")
	}
}

func TestReadSIPInfoDTMFBadBodyResponds400(t *testing.T) {
	media := DialogMedia{}
	req, tx := newInfoDTMFRequest(t, "garbage body")

	if err := readSIPInfoDTMF(&media, req, tx); err != nil {
		t.Fatalf("readSIPInfoDTMF failed: %v", err)
	}

	res := tx.Result()
	if len(res) != 1 || res[0].StatusCode != sip.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %+v", res)
	}
}

func TestReadSIPInfoDTMFNoReaderStill200(t *testing.T) {
	media := DialogMedia{}
	req, tx := newInfoDTMFRequest(t, "Signal=1\nDuration=160\n")

	if err := readSIPInfoDTMF(&media, req, tx); err != nil {
		t.Fatalf("readSIPInfoDTMF failed: %v", err)
	}

	res := tx.Result()
	if len(res) != 1 || res[0].StatusCode != sip.StatusOK {
		t.Fatalf("expected 200 OK even without registered reader, got %+v", res)
	}
}
