// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"log/slog"
	"net/http"

	"github.com/sjlit/diago/v2/media"
)

// Config is the construction-time configuration of an Agent.
//
// Transports is required; New returns ErrNoTransport when it is empty. The
// remaining zero values select the library defaults.
type Config struct {
	// Transports are the signaling listeners. At least one is required.
	Transports []Transport

	// Media is the default media configuration for dialogs whose transport
	// does not select a profile. An empty Codecs slice keeps the library
	// defaults (PCMU, PCMA, telephone-event).
	Media MediaConfig

	// MediaProfiles are named complete media configurations selected by
	// Transport.Profile. They never inherit fields from Media.
	MediaProfiles map[string]MediaConfig

	// Logger is used by sipgo, the Agent and the media pipeline. Nil uses
	// sip.DefaultLogger.
	Logger *slog.Logger

	// HTTPClient is used by URL playback. Nil uses the playback default client.
	HTTPClient *http.Client

	// UserAgent supplies an externally created sipgo UserAgent. The caller
	// keeps ownership: Agent.Close does not close it. Nil makes New create and
	// own a private UserAgent.
	UserAgent *UserAgent

	// Server supplies an externally created sipgo Server. The caller keeps
	// ownership. It must use the same UserAgent as the agent.
	Server *Server

	// Client supplies a shared sipgo Client. Nil makes each transport use its
	// own client. It must use the same UserAgent as the agent.
	Client *Client

	// UserAgentName and UserAgentHostname customize the private UserAgent
	// created when UserAgent is nil. They are ignored when UserAgent is set.
	UserAgentName     string
	UserAgentHostname string

	// Middleware wraps every request handler registered by the agent.
	Middleware func(next RequestHandler) RequestHandler
}

// DefaultTransport returns the built-in loopback transport (udp on
// 127.0.0.1:5060). Config never adds it implicitly.
func DefaultTransport() Transport {
	return Transport{
		Network:  "udp",
		BindHost: "127.0.0.1",
		BindPort: 5060,
	}
}

// New constructs a b2b user agent that acts as server and client.
//
// The UserAgent is optional: without Config.UserAgent, New creates a private
// one and Close closes it. A UserAgent passed through Config stays
// caller-owned and is not closed by Close, so it can be shared between agents.
//
// New fails when Config.Transports is empty (the v1 implicit 127.0.0.1:5060
// default was removed), when a transport is invalid, when a transport selects
// an unregistered media profile, or when the configuration is otherwise
// invalid.
func New(cfg Config) (*Agent, error) {
	opts := make([]agentOption, 0, 8+len(cfg.Transports)+len(cfg.MediaProfiles))

	if cfg.UserAgent != nil {
		opts = append(opts, withUserAgent(cfg.UserAgent))
	}
	if cfg.Server != nil {
		opts = append(opts, withServer(cfg.Server))
	}
	if cfg.Client != nil {
		opts = append(opts, withClient(cfg.Client))
	}
	if cfg.Logger != nil {
		opts = append(opts, withLogger(cfg.Logger))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, withHTTPClient(cfg.HTTPClient))
	}
	if cfg.UserAgentName != "" {
		opts = append(opts, withUserAgentName(cfg.UserAgentName))
	}
	if cfg.UserAgentHostname != "" {
		opts = append(opts, withUserAgentHostname(cfg.UserAgentHostname))
	}
	if cfg.Middleware != nil {
		opts = append(opts, withServerRequestMiddleware(cfg.Middleware))
	}

	mediaConf := cfg.Media
	if len(mediaConf.Codecs) == 0 {
		mediaConf.Codecs = []media.Codec{
			media.CodecAudioUlaw,
			media.CodecAudioAlaw,
			media.CodecTelephoneEvent8000,
		}
	}
	opts = append(opts, withMediaConfig(mediaConf))

	for name, conf := range cfg.MediaProfiles {
		opts = append(opts, withMediaProfile(name, conf))
	}
	for _, tran := range cfg.Transports {
		opts = append(opts, withTransport(tran))
	}

	return newAgent(opts...)
}
