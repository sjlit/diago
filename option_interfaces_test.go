// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/assert"
)

// TestOptionInterfaceMatrix pins which per-operation option interfaces each
// With* constructor implements. API calls are checked at compile time; this
// test guards the matrix itself, so accidentally widening a constructor's
// marker set (which would make an API accept an option it does not consume)
// fails here.
func TestOptionInterfaceMatrix(t *testing.T) {
	families := map[string]func(SignalOption) bool{
		"response":        func(o SignalOption) bool { _, ok := o.(ResponseOption); return ok },
		"progressMedia":   func(o SignalOption) bool { _, ok := o.(ProgressMediaOption); return ok },
		"answer":          func(o SignalOption) bool { _, ok := o.(AnswerOption); return ok },
		"request":         func(o SignalOption) bool { _, ok := o.(RequestOption); return ok },
		"hold":            func(o SignalOption) bool { _, ok := o.(HoldOption); return ok },
		"ack":             func(o SignalOption) bool { _, ok := o.(AckOption); return ok },
		"refer":           func(o SignalOption) bool { _, ok := o.(ReferOption); return ok },
		"invite":          func(o SignalOption) bool { _, ok := o.(InviteOption); return ok },
		"dial":            func(o SignalOption) bool { _, ok := o.(DialOption); return ok },
		"newCall":         func(o SignalOption) bool { _, ok := o.(NewCallOption); return ok },
		"waitAnswer":      func(o SignalOption) bool { _, ok := o.(WaitAnswerOption); return ok },
		"registerAttempt": func(o SignalOption) bool { _, ok := o.(RegisterAttemptOption); return ok },
		"register":        func(o SignalOption) bool { _, ok := o.(RegisterOption); return ok },
	}

	options := []struct {
		name     string
		opt      SignalOption
		families []string
	}{
		{"WithHeaders", WithHeaders(sip.NewHeader("X", "1")), []string{"response", "progressMedia", "answer", "request", "hold", "ack", "refer", "invite", "dial", "registerAttempt", "register"}},
		{"WithContact", WithContact(&sip.ContactHeader{Address: sip.Uri{Host: "127.0.0.1"}}), []string{"response", "progressMedia", "answer", "request", "hold", "ack", "refer", "invite", "dial", "registerAttempt", "register", "newCall"}},
		{"WithBody", WithBody([]byte("v=0")), []string{"progressMedia", "answer", "request", "hold", "invite", "dial"}},
		{"WithCodecs", WithCodecs(media.CodecAudioAlaw), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithRTPNAT", WithRTPNAT(1), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithMediaBindIP", WithMediaBindIP(net.ParseIP("127.0.0.1")), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithMediaExternalIP", WithMediaExternalIP(net.ParseIP("127.0.0.1")), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithMediaDTLS", WithMediaDTLS(media.DTLSConfig{ServerName: "sip.example"}), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithSDPSessionName", WithSDPSessionName("Name"), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithMediaSession", WithMediaSession(&media.MediaSession{}), []string{"progressMedia", "answer", "invite", "dial"}},
		{"WithMusicOnHold", WithMusicOnHold(audio.Tone{}), []string{"progressMedia", "answer", "hold", "invite", "dial"}},
		{"WithOnResponse", WithOnResponse(func(res *sip.Response) error { return nil }), []string{"invite", "dial", "waitAnswer"}},
		{"WithOnMediaUpdate", WithOnMediaUpdate(func(d *Media) {}), []string{"answer", "invite", "dial"}},
		{"WithOnRefer", WithOnRefer(func(d *OutboundCall) error { return nil }), []string{"answer", "invite", "dial"}},
		{"WithOnReferNotify", WithOnReferNotify(func(statusCode int) {}), []string{"refer"}},
		{"WithRequestMutator", WithRequestMutator(func(req *sip.Request) error { return nil }), []string{"request", "hold", "ack", "refer", "invite", "dial", "registerAttempt", "register"}},
		{"WithResponseMutator", WithResponseMutator(func(res *sip.Response) error { return nil }), []string{"response", "progressMedia", "answer"}},
		{"WithDialogTransport", WithDialogTransport("udp"), []string{"dial", "newCall"}},
		{"WithDialogTransportID", WithDialogTransportID("tr-1"), []string{"dial", "newCall"}},
		{"WithOriginator", WithOriginator(newBridgeTestDialog("o")), []string{"invite", "dial"}},
		{"WithAuthCredentials", WithAuthCredentials("u", "p"), []string{"invite", "dial", "waitAnswer", "registerAttempt", "register"}},
		{"WithEarlyMediaDetect", WithEarlyMediaDetect(), []string{"invite"}},
		{"WithRegisterExpiry", WithRegisterExpiry(time.Second), []string{"register"}},
		{"WithRegisterRetryInterval", WithRegisterRetryInterval(time.Second), []string{"register"}},
		{"WithRegisterAllowHeaders", WithRegisterAllowHeaders("INVITE"), []string{"register"}},
		{"WithRegisterProxyHost", WithRegisterProxyHost("127.0.0.1:5060"), []string{"register"}},
		{"WithOnRegistered", WithOnRegistered(func() {}), []string{"register"}},
		{"WithRegistrationDetached", WithRegistrationDetached(), []string{"register"}},
	}

	for _, tc := range options {
		t.Run(tc.name, func(t *testing.T) {
			want := make(map[string]bool, len(tc.families))
			for _, f := range tc.families {
				want[f] = true
			}
			for name, check := range families {
				assert.Equal(t, want[name], check(tc.opt),
					"option %s: interface %s must be %v", tc.name, name, want[name])
			}
		})
	}
}
