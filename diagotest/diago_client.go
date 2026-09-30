package diagotest

import (
	"fmt"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2"
)

func NewClient(ua *sipgo.UserAgent, onRequest func(req *sip.Request) *sip.Response) (*diago.Agent, error) {
	// Create client transaction request
	cTxReq := &clientTxRequester{
		onRequest: onRequest,
	}

	client, err := sipgo.NewClient(ua)
	if err != nil {
		return nil, fmt.Errorf("create sipgo client: %w", err)
	}
	client.TxRequester = cTxReq
	return diago.New(diago.WithUserAgent(ua), diago.WithClient(client), diago.WithDefaultTransport())
}
