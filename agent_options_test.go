// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"log/slog"
	"net/http"
)

// The production constructor takes Config directly. These test-only helpers
// keep the existing white-box tests readable without reintroducing the
// removed public/private option layer into the library build.
type agentOption func(*Config)

func newAgent(opts ...agentOption) (*Agent, error) {
	cfg := Config{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return newAgentFromConfig(cfg)
}

func withDefaultTransport() agentOption {
	return withTransport(DefaultTransport())
}

func withTransport(t Transport) agentOption {
	return func(c *Config) {
		c.Transports = append(c.Transports, t)
	}
}

func withUserAgent(ua *UserAgent) agentOption {
	return func(c *Config) {
		c.UserAgent = ua
	}
}

func withMediaConfig(conf MediaConfig) agentOption {
	return func(c *Config) {
		c.Media = conf
	}
}

func withMediaProfile(name string, conf MediaConfig) agentOption {
	return func(c *Config) {
		if c.MediaProfiles == nil {
			c.MediaProfiles = map[string]MediaConfig{}
		}
		c.MediaProfiles[name] = conf
	}
}

func withHTTPClient(client *http.Client) agentOption {
	return func(c *Config) {
		c.HTTPClient = client
	}
}

func withClient(client *Client) agentOption {
	return func(c *Config) {
		c.Client = client
	}
}

func withUserAgentName(name string) agentOption {
	return func(c *Config) {
		c.UserAgentName = name
	}
}

func withLogger(l *slog.Logger) agentOption {
	return func(c *Config) {
		c.Logger = l
	}
}

func withServerRequestMiddleware(f func(next RequestHandler) RequestHandler) agentOption {
	return func(c *Config) {
		c.Middleware = f
	}
}
