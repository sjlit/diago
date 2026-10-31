// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago_test

import (
	"context"
	"log"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2"
)

// ExampleNew creates an agent with a private UserAgent and a UDP transport.
// Close stops the listeners and closes the private UserAgent.
func ExampleNew() {
	agent, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close(context.Background())

	_ = agent
}

// ExampleAgent_Serve accepts incoming calls: ring, answer and keep the call
// alive until either side ends it.
func ExampleAgent_Serve() {
	agent, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close(context.Background())

	_ = agent.Serve(context.Background(), func(ctx context.Context, call *diago.InboundCall) error {
		if err := call.Ringing(ctx); err != nil {
			return err
		}
		if err := call.Answer(ctx); err != nil {
			return err
		}
		// Returning after Answer keeps the call up; this handler blocks until
		// the peer hangs up.
		<-call.Context().Done()
		return nil
	})
}

// ExampleAgent_Dial places an outbound call. Dial performs Invite and the
// final ACK; Hangup ends the signaling and Close releases local resources.
func ExampleAgent_Dial() {
	agent, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close(context.Background())

	call, err := agent.Dial(context.Background(), "sip:bob@example.com")
	if err != nil {
		log.Fatal(err)
	}
	defer call.Close()

	if err := call.Hangup(context.Background()); err != nil {
		log.Fatal(err)
	}
}

// ExampleAgent_Register registers a contact and keeps the registration
// refreshed until Close. WaitReady reports the first successful REGISTER.
func ExampleAgent_Register() {
	agent, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close(context.Background())

	ctx := context.Background()
	reg, err := agent.Register(ctx, "sip:alice@example.com",
		diago.WithAuthCredentials("alice", "secret"),
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := reg.WaitReady(ctx); err != nil {
		log.Fatal(err)
	}
	defer reg.Close(ctx)
}

// ExampleAgent_Handle registers a handler for a SIP method diago does not
// cover (MESSAGE here). Register before Serve.
func ExampleAgent_Handle() {
	agent, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close(context.Background())

	err = agent.Handle(sip.MESSAGE, func(req *diago.Request, tx diago.ServerTransaction) error {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		return tx.Respond(res)
	})
	if err != nil {
		log.Fatal(err)
	}
}
