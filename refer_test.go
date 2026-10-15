// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
)

// referEnv is the setup shared by the client-side and server-side REFER
// integration tests: a dialer that calls the transferor, the transferor that
// answers and issues the REFER, and the UAS that receives the call the REFER
// creates. Every agent binds an ephemeral port, so the tests can run next to
// each other.
//
// The two tests differ only in how they drive the transferor: the server-side
// one answers from the test goroutine and calls InboundCall.Refer itself, so
// its handler just publishes the dialog (see newReferEnv's onTransferor); the
// client-side one lets the handler answer with WithOnRefer and then calls
// OutboundCall.Refer from the test. The UAS and the dialer are identical in
// both.
type referEnv struct {
	// dialer places the call that starts the transfer.
	dialer *Agent
	// transferorPort is where dialer's call lands.
	transferorPort int
	// uasPort is the REFER target: this UAS answers the transferred call.
	uasPort int
}

// uasTarget returns the URI of the REFER target (the UAS).
func (e *referEnv) uasTarget(user string) sip.Uri {
	return sip.Uri{User: user, Host: "127.0.0.1", Port: e.uasPort}
}

// newReferEnv starts the three agents and returns their addresses. onTransferor
// is the serve handler of the transferor agent, which is the only part that
// differs between the two REFER tests.
func newReferEnv(t *testing.T, ctx context.Context, onTransferor CallHandler) *referEnv {
	t.Helper()
	env := &referEnv{}

	// UAS that accepts REFER. The "busy" and "noanswer" users make the
	// transfer fail so the NOTIFY outcome can be asserted.
	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { ua.Close() })
	_, env.uasPort = servingAgent(t, ctx, func(ctx context.Context, d *InboundCall) error {
		t.Log("Call INVITE due to REFER received")
		switch d.ToUser() {
		case "busy":
			d.Respond(sip.StatusBusyHere, "Busy Here", nil)
			return nil
		case "noanswer":
			d.Ringing(ctx)
			return nil
		default:
			d.Answer(ctx)
		}

		<-d.Context().Done()
		return nil
	}, WithUserAgent(ua), WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))

	uaTransferor, _ := sipgo.NewUA(sipgo.WithUserAgent("transferor"))
	t.Cleanup(func() { uaTransferor.Close() })
	_, env.transferorPort = servingAgent(t, ctx, onTransferor,
		WithUserAgent(uaTransferor),
		WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
	)

	// The dialer listens for re-INVITEs of its own call, so it needs a
	// listener too even though it initiates the dialog.
	uaDialer, _ := sipgo.NewUA()
	t.Cleanup(func() { uaDialer.Close() })
	env.dialer, _ = servingAgent(t, ctx, nil,
		WithUserAgent(uaDialer),
		WithTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0, ID: "udp"}),
	)

	return env
}

// referNotifier collects the NOTIFY status codes a REFER reports. The callback
// runs on a library goroutine: the channel is buffered and a further send is
// dropped rather than blocking the library, and expect reads with a timeout so
// a missing NOTIFY fails instead of hanging the test.
type referNotifier struct {
	states chan int
}

func newReferNotifier() *referNotifier {
	return &referNotifier{states: make(chan int, 8)}
}

func (n *referNotifier) option() ReferOption {
	return WithOnReferNotify(func(statusCode int) {
		select {
		case n.states <- statusCode:
		default:
		}
	})
}

// expect reads one NOTIFY code per wanted code and compares them in order.
func (n *referNotifier) expect(t *testing.T, codes ...int) {
	t.Helper()
	for _, want := range codes {
		got := recvWithin(t, n.states, 5*time.Second)
		assert.Equal(t, want, got, "unexpected REFER NOTIFY status")
	}
}
