// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
)

// Agent is a SIP user agent that acts as both UAS (Serve) and UAC (Dial,
// Register) on top of a sipgo UserAgent. Construct it with New and shut it
// down with Close.
type Agent struct {
	ua     *UserAgent
	client *Client
	server *Server

	// ownedUA reports that New created the UserAgent and Close must close it.
	ownedUA    bool
	uaName     string
	uaHostname string

	transports []Transport

	serveHandler atomic.Pointer[CallHandler]

	mediaConf MediaConfig

	// mediaProfiles holds the named media configurations registered with
	// WithMediaProfile. Set up during New and read-only afterwards.
	mediaProfiles map[string]MediaConfig

	// httpClient is the agent-level fallback for MediaConfig.HTTPClient
	// (WithHTTPClient). Nil means the playback default client.
	httpClient *http.Client

	log *slog.Logger

	cache            dialogCachePool
	serverMiddleware func(next RequestHandler) RequestHandler
	// wrapRequest wraps a method handler with the server middleware and the
	// error logging path. Set by New; used by Handle.
	wrapRequest func(f func(req *Request, tx ServerTransaction) error) RequestHandler

	// registerAfter is the timer factory used between REGISTER attempts of
	// Register/RegisterTransaction. It defaults to time.After and exists as a
	// field so tests can exercise retry pacing without wall-clock waits.
	registerAfter func(time.Duration) <-chan time.Time

	// Serve lifecycle state. serveWG tracks the listener goroutines started
	// by serve(); serveCancel cancels them. stopped is terminal: once
	// Close has run, serving is refused.
	serveMu          sync.Mutex
	serveCtx         context.Context
	serveCancel      context.CancelFunc
	serveStartupDone chan struct{}
	serveWG          sync.WaitGroup
	serving          atomic.Bool
	stopped          atomic.Bool
}

// We can extend this WithClientOptions, WithServerOptions

// AgentOption configures New. An option error aborts construction.
type AgentOption func(dg *Agent) error

// Transport describes one SIP signaling listener. See NewTransport and
// Transport.Validate for the accepted network values.
type Transport struct {
	ID string

	// Network must be udp, tcp or ws, or even forcing v4 like udp4, tcp4.
	// TLS transports use tls/wss (with TLSConf set).
	Network string
	network string // network will keep original transport value

	// BindHost sets IP to bind. If specified (not 0.0.0.0) it supplies the
	// media bind IP when the selected profile leaves Network.BindIP nil.
	BindHost string
	// BindPort sets port to bind. Leaving at 0 will use ephemeral port and apply on Contact addr
	BindPort int
	bindIP   net.IP

	ExternalHost string // SIP signaling and media external addr
	ExternalPort int

	// Profile names a MediaConfig registered with WithMediaProfile. Empty
	// selects the agent default (WithMediaConfig). The profile is a complete
	// media configuration for dialogs carried over this transport; the
	// transport-derived bind/advertise addresses fill profile fields left
	// nil (see mediaConfigForTransport).
	Profile string

	mediaBindIP net.IP

	// mediaExternalIP is derived from ExternalHost/BindHost at WithTransport
	// time and applied only when the selected profile leaves
	// Network.ExternalIP unset.
	mediaExternalIP net.IP

	// In case TLS protocol
	TLSConf *tls.Config
	// Avoiding SIPS in contact uri https://datatracker.ietf.org/doc/html/rfc5630#section-3.3
	TLSURINoSIPS bool

	RewriteContact bool

	client *Client
}

// NewTransport builds a Transport for the given network and validates it.
// Accepted networks are udp, tcp, tls, ws and wss, optionally suffixed with
// 4 or 6 to force the IP family (udp4, tcp6, ...). Invalid values fail here
// instead of at serve time.
func NewTransport(network string) (Transport, error) {
	t := Transport{Network: network}
	if err := t.Validate(); err != nil {
		return Transport{}, err
	}
	return t, nil
}

// Validate reports whether the transport can be served by New/serve.
// The network must be udp, tcp, tls, ws or wss with an optional 4/6 suffix;
// tls and wss require TLSConf (and no other network may carry one). tls6 is
// rejected: the underlying SIP stack has no TLS-over-IPv6 listener. The
// remaining suffix forms are translated by tlsListenNetwork at serve time.
func (t Transport) Validate() error {
	// WithTransport trims the 4/6 suffix from Transport and keeps the raw
	// value in network, so validate the original spelling when present.
	raw := strings.TrimSpace(t.Network)
	if t.network != "" {
		raw = t.network
	}
	network := sip.NetworkToLower(raw)
	base := network
	if n := len(network); n > 0 && (network[n-1] == '4' || network[n-1] == '6') {
		base = network[:n-1]
	}
	switch base {
	case "udp", "tcp", "ws":
		if t.TLSConf != nil {
			return fmt.Errorf("network %q cannot carry TLSConf: use tls or wss", raw)
		}
	case "tls", "wss":
		if t.TLSConf == nil {
			return fmt.Errorf("network %q requires TLSConf", raw)
		}
		if base == "tls" && network != "tls" && network != "tls4" {
			// sipgo maps tcp6/ws6 to wss, so there is no TLS-over-IPv6 path.
			return fmt.Errorf("network %q is not supported by the sip stack: use tls, tls4 or wss with an optional 4/6 suffix", raw)
		}
	default:
		return fmt.Errorf("invalid network %q: use udp/tcp/tls/ws/wss with optional 4 or 6 suffix", raw)
	}
	if t.BindPort < 0 || t.BindPort > 65535 {
		return fmt.Errorf("invalid bind port %d", t.BindPort)
	}
	if t.ExternalPort < 0 || t.ExternalPort > 65535 {
		return fmt.Errorf("invalid external port %d", t.ExternalPort)
	}
	return nil
}

// tlsListenNetwork maps a validated tls/wss transport network to the network
// string sipgo.ListenAndServeTLS understands: it applies the IP-family suffix
// itself (tls4 -> tcp4, wss4 -> ws4, wss6 -> ws6). tls6 never reaches serve:
// Validate rejects it.
func tlsListenNetwork(network string) string {
	switch network {
	case "tls4":
		return "tcp4"
	case "wss4":
		return "ws4"
	case "wss6":
		return "ws6"
	}
	return network
}

// WithDefaultTransport explicitly installs the built-in loopback default
// (udp on 127.0.0.1:5060). v2 never adds it implicitly: New fails
// without at least one configured transport.
func WithDefaultTransport() AgentOption {
	return WithTransport(Transport{
		Network:  "udp",
		BindHost: "127.0.0.1",
		BindPort: 5060,
	})
}

// WithTransport adds a signaling transport to the agent. New requires at
// least one; see Transport for the accepted networks and fields.
func WithTransport(t Transport) AgentOption {
	return func(dg *Agent) error {
		if strings.TrimSpace(t.Network) == "" {
			return fmt.Errorf("WithTransport: network is empty")
		}
		t.bindIP = net.ParseIP(t.BindHost)
		t.mediaBindIP = t.bindIP
		if t.bindIP != nil && t.bindIP.IsUnspecified() {
			network := "ip4"
			if t.bindIP.To4() == nil {
				network = "ip6"
			}
			var err error
			t.mediaBindIP, _, err = sip.ResolveInterfacesIP(network, nil)
			if err != nil {
				dg.log.Error("failed to resolve real IP", "error", err)
			}
		}

		// A concrete BindHost/ExternalHost is an explicit signaling choice, so
		// it also pins the media external IP. An unspecified BindHost only
		// provides the client bind address (mediaBindIP) and leaves the media
		// IP to the global MediaConfig.
		externalExplicit := t.ExternalHost != ""
		if t.ExternalHost == "" {
			t.ExternalHost = t.BindHost
			// External host should match media IP
			if t.mediaBindIP != nil {
				t.ExternalHost = t.mediaBindIP.String()
			}
		}
		if t.ExternalPort == 0 {
			t.ExternalPort = t.BindPort
		}

		// Resolve unspecified IP for contact hdr
		extIp := net.ParseIP(t.ExternalHost)
		if t.ExternalHost == "" || (extIp != nil && extIp.IsUnspecified()) {
			// Use the mediaIP
			extIp = t.mediaBindIP
		}

		// Remember the derived media external IP. mediaConfigForTransport
		// applies it only when the selected profile leaves
		// Network.ExternalIP unset; an unspecified BindHost does not imply an
		// external IP.
		if externalExplicit || t.bindIP == nil || !t.bindIP.IsUnspecified() {
			if extIp != nil && !extIp.IsUnspecified() {
				t.mediaExternalIP = extIp
			}
		}

		t.Network = sip.NetworkToLower(strings.TrimSpace(t.Network))
		t.network = t.Network
		t.Network = strings.TrimSuffix(t.Network, "4") // udp4, tcp4
		t.Network = strings.TrimSuffix(t.Network, "6") // udp6, tcp6

		// we want to handle SIP networking better per each transport
		dg.transports = append(dg.transports, t)

		dg.log.Debug("Loaded transport", "t", t)
		return nil
	}
}

// SecureRTP selects the SRTP keying mode of a media session.
type SecureRTP int

// SecureRTPNone disables SRTP; SecureRTPSDES selects SDES keying;
// SecureRTPDTLS selects DTLS-SRTP.
const (
	SecureRTPNone SecureRTP = iota
	SecureRTPSDES
	SecureRTPDTLS
)

// SRTPAlg is an SRTP protection profile. Values map to the
// srtp.ProtectionProfile constants; zero selects the default profile
// (srtp.ProtectionProfileAes128CmHmacSha1_80) when Mode is not SecureRTPNone.
type SRTPAlg uint16

// RTPNATMode selects how RTP behind NAT is handled.
type RTPNATMode int

// RTPNATDisabled disables RTP source learning; RTPNATLearnSource learns the
// remote source address from incoming packets (symmetric RTP;
// media.RTPNATSymetric).
const (
	RTPNATDisabled RTPNATMode = iota
	RTPNATLearnSource
)

// SRTPConfig groups the SRTP/DTLS settings of a media session.
type SRTPConfig struct {
	// Mode selects the SRTP keying mode. SecureRTPDTLS requires DTLS; when
	// DTLS is non-zero and Mode is left at SecureRTPNone, DTLS is enabled
	// automatically.
	Mode SecureRTP
	// Alg is the SRTP protection profile. Zero picks the default profile.
	// media.DTLSConfig.SRTPProfiles wins for DTLS.
	Alg SRTPAlg
	// DTLS is the DTLS-SRTP configuration.
	DTLS media.DTLSConfig
	// SAVPDisable keeps RTP/AVP in the offered SDP profile even when SRTP is
	// enabled (a=crypto compatibility mode for peers like Asterisk), unless
	// the remote offer already uses RTP/SAVP.
	SAVPDisable bool
}

// NetworkConfig groups the RTP address and port settings.
type NetworkConfig struct {
	// BindIP is the local IP used to bind RTP/RTCP listeners. Nil resolves
	// the interface IP when the media session is created.
	BindIP net.IP
	// ExternalIP is the IP advertised inside SDP (c= line). Nil uses the
	// session bind IP.
	ExternalIP net.IP
	// RTPNAT 0 - disabled, 1 - learn source. Check media.RTPNAT options.
	RTPNAT RTPNATMode
	// RTPPortStart/RTPPortEnd define the local RTP port range. Ports are
	// picked in steps of 2 so the RTCP port (RTP+1) fits. Zero means
	// ephemeral ports with the library's bind retry.
	RTPPortStart int
	RTPPortEnd   int
}

// SDPConfig groups SDP generation settings.
type SDPConfig struct {
	// CodecPreferLocalOrder makes the answerer order common codecs by local
	// preference instead of the offerer order (RFC 3264 default). Zero keeps
	// the offerer order.
	CodecPreferLocalOrder int
	// SessionName overrides the SDP "s=" session-name line. Empty keeps the
	// library default "Sip Go Media".
	SessionName string
}

// MediaConfig configures the media sessions created by Agent.
type MediaConfig struct {
	Codecs  []media.Codec
	SRTP    SRTPConfig
	Network NetworkConfig
	SDP     SDPConfig

	// MusicOnHold is the dialog-level default hold music: Hold() starts
	// looping it automatically once the hold re-INVITE succeeds. Zero value
	// (no segments) disables automatic hold music; Hold then behaves as
	// before. Per-call override: WithMusicOnHold; manual control:
	// Media.StartMusicOnHold/StopMusicOnHold.
	MusicOnHold audio.Tone

	// Logger receives media-pipeline logs: media package internals (RTP/RTCP,
	// negotiation, DTLS debug) and dialog-level components (hold music, DTMF
	// relay, playback). Nil falls back to the Agent logger; when that is also
	// unset the media package default (slog.Default) is used.
	Logger *slog.Logger

	// Debug enables RTP/RTCP packet tracing on the agent's dialogs. When
	// Tracer is nil packets are logged through Logger. Per-transport override
	// is not supported: tracing is agent-wide.
	Debug bool
	// Tracer receives RTP packets when Debug is enabled, replacing logging.
	// Implementations must not block (they run on the media path).
	Tracer media.RTPTracer
	// HTTPClient is used by URL playback (PlayURL/PlayURLContext). Nil uses a
	// default client with a 20s timeout.
	HTTPClient *http.Client
}

// validateMediaConfig checks the parts of a MediaConfig that can be rejected
// before any session is created.
func validateMediaConfig(conf MediaConfig) error {
	if len(conf.Codecs) == 0 {
		return fmt.Errorf("no codecs configured")
	}
	if conf.SRTP.Mode < SecureRTPNone || conf.SRTP.Mode > SecureRTPDTLS {
		return fmt.Errorf("invalid srtp mode %d", conf.SRTP.Mode)
	}
	if conf.Network.RTPPortStart != 0 && conf.Network.RTPPortEnd != 0 && conf.Network.RTPPortEnd < conf.Network.RTPPortStart {
		return fmt.Errorf("RTP port range %d-%d is inverted", conf.Network.RTPPortStart, conf.Network.RTPPortEnd)
	}
	return nil
}

// WithMediaConfig replaces the default media configuration (the profile used
// by transports that do not name one). It is a whole struct replacement, not
// a merge: zero fields keep their documented library defaults (see
// NetworkConfig and SDPConfig). At least one codec is required.
func WithMediaConfig(conf MediaConfig) AgentOption {
	return func(dg *Agent) error {
		if err := validateMediaConfig(conf); err != nil {
			return fmt.Errorf("WithMediaConfig: %w", err)
		}
		dg.mediaConf = conf
		return nil
	}
}

// WithMediaProfile registers a named, complete media configuration. A
// Transport selects one with Transport.Profile (empty selects the
// WithMediaConfig default). Unlike the removed TransportMediaConfig there is
// no field-level inheritance between profiles: the selected profile is used
// as is, and only the transport-derived bind/advertise addresses fill fields
// the profile leaves nil. Registering the same name twice replaces it.
func WithMediaProfile(name string, conf MediaConfig) AgentOption {
	return func(dg *Agent) error {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithMediaProfile: name is empty")
		}
		if err := validateMediaConfig(conf); err != nil {
			return fmt.Errorf("WithMediaProfile %q: %w", name, err)
		}
		if dg.mediaProfiles == nil {
			dg.mediaProfiles = map[string]MediaConfig{}
		}
		dg.mediaProfiles[name] = conf
		return nil
	}
}

// WithHTTPClient sets the HTTP client used by URL playback
// (AudioPlayback.PlayURL/PlayURLContext) on the dialogs of this agent. Nil is
// rejected. The default client has a 20s timeout; set
// MediaConfig.HTTPClient directly when a whole config is provided.
func WithHTTPClient(client *http.Client) AgentOption {
	return func(dg *Agent) error {
		if client == nil {
			return fmt.Errorf("WithHTTPClient: client is nil")
		}
		dg.httpClient = client
		return nil
	}
}

// WithServer allows providing custom server handle. It must use the same UA
// as the agent.
func WithServer(srv *Server) AgentOption {
	return func(dg *Agent) error {
		if srv == nil {
			return fmt.Errorf("WithServer: server is nil")
		}
		dg.server = srv
		return nil
	}
}

// WithClient allows providing custom client handle. It must use the same UA
// as the agent.
func WithClient(client *Client) AgentOption {
	return func(dg *Agent) error {
		if client == nil {
			return fmt.Errorf("WithClient: client is nil")
		}
		dg.client = client
		return nil
	}
}

// WithUserAgent supplies an externally created sipgo UserAgent. The caller
// keeps ownership: Close does not close it, so the same UserAgent can be
// shared between agents. When not set, New creates a private UserAgent and
// Close closes it.
func WithUserAgent(ua *UserAgent) AgentOption {
	return func(dg *Agent) error {
		if ua == nil {
			return fmt.Errorf("WithUserAgent: user agent is nil")
		}
		dg.ua = ua
		return nil
	}
}

// WithUserAgentName sets the user name of the private UserAgent created by
// New (default "diago"). It is ignored when WithUserAgent supplies one.
func WithUserAgentName(name string) AgentOption {
	return func(dg *Agent) error {
		if name == "" {
			return fmt.Errorf("WithUserAgentName: name is empty")
		}
		dg.uaName = name
		return nil
	}
}

// WithUserAgentHostname sets the hostname presented in From/Contact of the
// private UserAgent created by New (default sipgo's "localhost"). It is
// ignored when WithUserAgent supplies one.
func WithUserAgentHostname(hostname string) AgentOption {
	return func(dg *Agent) error {
		if hostname == "" {
			return fmt.Errorf("WithUserAgentHostname: hostname is empty")
		}
		dg.uaHostname = hostname
		return nil
	}
}

// WithLogger sets the logger for sipgo, the Agent engine, and the media
// pipeline logs (hold music, DTMF relay, playback and media internals) unless
// MediaConfig.Logger explicitly overrides it. Nil is rejected (the default is
// sip.DefaultLogger()).
func WithLogger(l *slog.Logger) AgentOption {
	return func(dg *Agent) error {
		if l == nil {
			return fmt.Errorf("WithLogger: logger is nil")
		}
		dg.log = l
		return nil
	}
}

// WithServerRequestMiddleware wraps every request handler registered by the
// agent.
func WithServerRequestMiddleware(f func(next RequestHandler) RequestHandler) AgentOption {
	return func(dg *Agent) error {
		if f == nil {
			return fmt.Errorf("WithServerRequestMiddleware: middleware is nil")
		}
		dg.serverMiddleware = f
		return nil
	}
}

// New constructs a b2b user agent that will act as server and client.
//
// The UserAgent is optional: without WithUserAgent, New creates a private one
// (name "diago", WithUserAgentName/WithUserAgentHostname to customize) and
// Close closes it. A UserAgent passed with WithUserAgent stays caller-owned
// and is not closed by Close, so it can be shared between agents.
//
// New fails when no transport is configured (the v1 implicit 127.0.0.1:5060
// default was removed), when a configured transport is invalid or when an
// option rejects its configuration; use WithTransport or WithDefaultTransport.
func New(opts ...AgentOption) (*Agent, error) {
	dg := &Agent{
		log:        sip.DefaultLogger(),
		transports: []Transport{},
		mediaConf: MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
		},
		mediaProfiles: map[string]MediaConfig{},

		cache: dialogCachePool{
			client: &dialogCacheMap[*OutboundCall]{sync.Map{}},
			server: &dialogCacheMap[*InboundCall]{sync.Map{}},
		},

		registerAfter: time.After,
	}
	// Refuse calls until Serve installs a real handler.
	handler := CallHandler(defaultCallHandler)
	dg.serveHandler.Store(&handler)

	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(dg); err != nil {
			return nil, fmt.Errorf("diago: option: %w", err)
		}
	}

	// Media logs follow the Agent logger unless a MediaConfig explicitly
	// carries its own.
	if dg.mediaConf.Logger == nil {
		dg.mediaConf.Logger = dg.log
	}
	// WithHTTPClient provides the agent-level default for the playback client
	// unless the MediaConfig already carries one.
	if dg.mediaConf.HTTPClient == nil {
		dg.mediaConf.HTTPClient = dg.httpClient
	}

	if len(dg.transports) == 0 {
		return nil, fmt.Errorf("diago: no transport configured: use WithTransport or WithDefaultTransport")
	}
	for i := range dg.transports {
		if err := dg.transports[i].Validate(); err != nil {
			return nil, fmt.Errorf("diago: transport %d: %w", i, err)
		}
		if p := dg.transports[i].Profile; p != "" {
			if _, ok := dg.mediaProfiles[p]; !ok {
				return nil, fmt.Errorf("diago: transport %d: media profile %q is not registered: use WithMediaProfile", i, p)
			}
		}
	}

	// Resolve the UserAgent: an explicit WithUserAgent wins; otherwise create
	// a private one owned by this agent (Close closes it).
	if dg.ua == nil {
		uaName := dg.uaName
		if uaName == "" {
			uaName = "diago"
		}
		uaOpts := []sipgo.UserAgentOption{sipgo.WithUserAgent(uaName)}
		if dg.uaHostname != "" {
			uaOpts = append(uaOpts, sipgo.WithUserAgentHostname(dg.uaHostname))
		}
		ua, err := sipgo.NewUA(uaOpts...)
		if err != nil {
			return nil, fmt.Errorf("diago: failed to create UserAgent: %w", err)
		}
		dg.ua = ua
		dg.ownedUA = true
	}

	// Per-transport clients need the resolved UserAgent.
	for i := range dg.transports {
		dg.transports[i].client = dg.createClient(dg.transports[i])
	}

	if dg.server == nil {
		srv, err := sipgo.NewServer(dg.ua, sipgo.WithServerLogger(dg.log))
		if err != nil {
			return nil, fmt.Errorf("diago: failed to create default sip server: %w", err)
		}
		dg.server = srv
	}
	server := dg.server

	errHandler := func(f func(req *Request, tx ServerTransaction) error) RequestHandler {
		handler := func(req *Request, tx ServerTransaction) {
			if err := f(req, tx); err != nil {
				dg.log.Warn("Failed to handle request", "error", err, "req.method", req.Method.String(), "req.from", req.From().String(), "req.to", req.To().Value())
				return
			}
			// Termination gracefull will be done by sipgo now
		}

		// Return handler wrapped with middleware if user supprots
		if dg.serverMiddleware != nil {
			return func(req *Request, tx ServerTransaction) {
				dg.serverMiddleware(handler)(req, tx)
			}
		}
		return handler
	}
	dg.wrapRequest = errHandler

	handleNoDialog := func(req *Request, tx ServerTransaction, err error) error {
		var response *Response
		if errors.Is(err, sipgo.ErrDialogDoesNotExists) {
			response = sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		} else {
			response = sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil)
		}
		return errors.Join(err, tx.Respond(response))
	}

	server.OnInvite(errHandler(func(req *Request, tx ServerTransaction) error {
		// What if multiple server transports?
		id, err := sip.DialogIDFromRequestUAS(req)
		if err == nil {
			return dg.handleReInvite(req, tx, id)
		}

		tran, _ := dg.getTransport(req.Transport())

		// Proceed as new call
		dialogUA := sipgo.DialogUA{
			Client:         dg.getClient(tran),
			RewriteContact: tran.RewriteContact,
		}
		dg.contactHDRFromTransport(tran, &dialogUA.ContactHDR)

		dialog, err := dialogUA.ReadInvite(req, tx)
		if err != nil {
			return fmt.Errorf("handling new invite failed: %w", err)
		}

		// Server-side digest auth is opt-in: call InboundCall.Authorize
		// (DigestAuthServer.AuthorizeDialog) as the first action of your serve
		// handler to challenge/validate the INVITE (RFC 2617).
		mediaConf := dg.mediaConfigForTransport(tran)
		dWrap := &InboundCall{
			DialogServerSession: dialog,
			callMedia:           Media{log: mediaConf.Logger, httpClient: mediaConf.HTTPClient},
			// TODO we may actually just build media session with this conf here
			mediaConf: mediaConf,
		}

		if err := dg.cache.server.store(dWrap.Context(), dWrap.DialogServerSession.ID, dWrap); err != nil {
			return fmt.Errorf("failed to store server dialog: %w", err)
		}

		hctx, hcancel := dg.callHandlerContext(dialog.Context())
		defer func() {
			// The dialog (not the handler) owns the lifetime: when it ends —
			// peer BYE/CANCEL, a decline written below, Close or serve
			// context cancellation — the cache entry is evicted, the media is
			// closed and the handler context is canceled. An answered call
			// therefore stays alive after the handler returns.
			<-hctx.Done()
			hcancel()

			if dWrap.LoadState() != sip.DialogStateEnded {
				// The serve context ended while the dialog was still up
				// (plain Serve ctx cancellation, not Close). Tear the call
				// down so media and transaction state do not leak.
				tctx, tcancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := dWrap.Hangup(tctx); err != nil {
					dg.log.Warn("Hangup on dialog teardown failed", "error", err, "dialog", dWrap.ID())
				}
				tcancel()
			}

			// TODO: have better context
			if err := dg.cache.server.remove(context.Background(), dWrap.DialogServerSession.ID); err != nil {
				dg.log.Error("Failed to delete server dialog", "error", err)
			}
			closeAndLog(dWrap.logger(), dWrap, "closing dialog server returned error")
		}()

		herr := dg.handler()(hctx, dWrap)

		// Check is dialog closed
		dialogCtx := dialog.Context()
		// Teardown context for the response we may still owe the peer.
		ctx, cancel := context.WithTimeout(dialogCtx, 10*time.Second)
		defer cancel()

		switch state := dWrap.LoadState(); {
		case state == sip.DialogStateEnded:
			// The dialog already ended (CANCEL/BYE) while the handler ran.
			return nil
		case herr != nil:
			if err := dWrap.respondHandlerError(ctx, herr); err != nil {
				return fmt.Errorf("handler error handling failed: %w", err)
			}
		case state < sip.DialogStateEstablished:
			// The handler returned without answering or otherwise sending a
			// final response: decline the still-pending INVITE.
			if err := dWrap.Hangup(ctx); err != nil {
				if errors.Is(ctx.Err(), context.Canceled) {
					// Already hangup
					return nil
				}

				return fmt.Errorf("hanguping call failed: %w", err)
			}
		default:
			// The call was answered: it stays alive until the peer, the
			// framework or the owner ends it. The deferred watcher above
			// performs the cleanup when that happens.
		}
		return nil
	}))

	server.OnCancel(errHandler(func(req *Request, tx ServerTransaction) error {
		// INVITE transaction should be terminated by transaction layer and 200 response will be sent
		// In case of stateless proxy this we would need to forward
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
	}))

	server.OnAck(errHandler(func(req *Request, tx ServerTransaction) error {
		// d, err := dg.cache.matchDialogServer(req)
		// if err != nil {
		// 	// Normally ACK will be received if some out of dialog request is received or we responded negatively
		// 	// tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, err.Error(), nil))
		// 	return err
		// }

		sd, cd, err := dg.cache.matchDialog(req)
		if err != nil {
			// We should not do responses for ACK
			return err
			// return handleNoDialog(req, tx, err)
		}

		if cd != nil {
			// THis is from REINVITE
			return cd.handleReInviteACK(req, tx)
		}

		// Race defense: notify the per-dialog ACK channel BEFORE calling
		// readAck so any pending respondSignal waiter can treat a sipgo
		// WaitForAck loop exit as success. Only the ACK matching the INVITE
		// CSeq counts; a stray ACK must not close the channel.
		if sd.matchesInviteAck(req) {
			sd.notifyAck()
		}
		return sd.readAck(req, tx)
	}))

	server.OnBye(errHandler(func(req *Request, tx ServerTransaction) error {
		sd, cd, err := dg.cache.matchDialog(req)
		if err != nil {
			return handleNoDialog(req, tx, err)
		}

		// Respond to BYE
		// Terminate our media processing
		// As user may stuck in playing or reading media, this unblocks that goroutine
		if cd != nil {
			defer closeAndLog(cd.logger(), &cd.callMedia, "failed to close client media")

			return cd.ReadBye(req, tx)
		}

		defer closeAndLog(sd.logger(), &sd.callMedia, "failed to close server media")
		return sd.ReadBye(req, tx)
	}))

	server.OnInfo(errHandler(func(req *Request, tx ServerTransaction) error {
		// Handle DTMF out of band
		ct := req.ContentType()
		if ct == nil || ct.Value() != "application/dtmf-relay" {
			return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		}

		sd, cd, err := dg.cache.matchDialog(req)
		if err != nil {
			return handleNoDialog(req, tx, err)
		}

		if cd != nil {
			return cd.readSIPInfoDTMF(req, tx)

		}
		return sd.readSIPInfoDTMF(req, tx)

		// 		INFO sips:sipgo@127.0.0.1:5443 SIP/2.0
		// Via: SIP/2.0/WSS df7jal23ls0d.invalid;branch=z9hG4bKhzJuRuWp4pLmTAbrIg7MUGofWdV1u577;rport
		// From: "IVR Webrtc"<sips:ivr.699c4b45-c800-4891-8133-fded5b26f942.579938@localhost:6060>;tag=foSxtEhHq9QOSeSdgJCC
		// To: <sip:playback@localhost>;tag=f814097f-467a-46ad-be0a-76c2a1225378
		// Contact: "IVR Webrtc"<sips:ivr.699c4b45-c800-4891-8133-fded5b26f942.579938@df7jal23ls0d.invalid;rtcweb-breaker=no;click2call=no;transport=wss>;+g.oma.sip-im;language="en,fr"
		// Call-ID: 047c3631-e85a-27d2-8f69-4de6e0391253
		// CSeq: 29586 INFO
		// Content-Type: application/dtmf-relay
		// Content-Length: 22
		// Max-Forwards: 70
		// User-Agent: IM-client/OMA1.0 sipML5-v1.2016.03.04

		// Signal=8
		// Duration=120

	}))

	// TODO deal with OPTIONS more correctly
	// For now leave it for keep alive
	dg.server.OnOptions(errHandler(func(req *Request, tx ServerTransaction) error {
		methods := dg.server.RegisteredMethods()
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		res.AppendHeader(sip.NewHeader("Allow", strings.Join(methods, ", ")))
		res.AppendHeader(sip.NewHeader("Accept", "application/sdp"))
		// res.AppendHeader(sip.NewHeader("Supported", "replaces, 100rel"))
		return tx.Respond(res)
	}))

	dg.server.OnRefer(errHandler(func(req *Request, tx ServerTransaction) error {
		sd, cd, err := dg.cache.matchDialog(req)
		if err != nil {
			return handleNoDialog(req, tx, err)
		}
		if cd != nil {
			cd.handleRefer(dg, req, tx)
			return nil

		}
		// TODO server
		sd.handleRefer(dg, req, tx)
		return nil
	}))

	dg.server.OnNotify(errHandler(func(req *Request, tx ServerTransaction) error {
		// THIS should match now subscribtion instead dialog
		sd, cd, err := dg.cache.matchDialog(req)
		if err != nil {
			return handleNoDialog(req, tx, err)
		}

		if cd != nil {
			cd.handleReferNotify(req, tx)
			return nil
		}
		sd.handleReferNotify(req, tx)
		return nil
	}))
	// server.OnRefer(func(req *Request, tx ServerTransaction) {
	// 	d, err := matchDialogServer(req)
	// 	if err != nil {
	// 		// Security? When to answer this?
	// 		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, err.Error(), nil))
	// 		return
	// 	}
	// })

	return dg, nil
}

func (dg *Agent) handleReInvite(req *Request, tx ServerTransaction, id string) error {
	// Cache lookups are in-memory and context-free today; Background only
	// satisfies the interface.
	ctx := context.Background()
	// No Error means we have ID
	s, err := dg.cache.server.load(ctx, id)
	if err != nil {
		id, err := sip.DialogIDFromRequestUAC(req)
		if err != nil {
			dg.log.Info("Reinvite failed to read request dialog ID", "error", err)
			return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))

		}
		// No Error means we have ID
		c, err := dg.cache.client.load(ctx, id)
		if err != nil {
			return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		}

		return c.handleReInvite(req, tx)
	}

	return s.handleReInvite(req, tx)
}

// MethodHandler handles one out-of-dialog SIP request method registered with
// Agent.Handle. Returning an error is logged; no SIP response is generated by
// the framework (the handler is expected to respond itself).
type MethodHandler func(req *Request, tx ServerTransaction) error

// Handle registers a handler for a SIP method diago does not own (MESSAGE,
// SUBSCRIBE, PUBLISH, ...). The handler is wrapped by the middleware given to
// WithServerRequestMiddleware.
//
// Handle must be called before Serve/ServeBackground: the sipgo method to
// handler map is not safe to mutate while the server is dispatching, so
// registering while serving returns an error.
func (dg *Agent) Handle(method RequestMethod, handler MethodHandler) error {
	if handler == nil {
		return fmt.Errorf("diago: Handle: handler is nil")
	}
	// Methods owned by the agent carry dialog and media handling; replacing
	// them would silently bypass it.
	switch method {
	case sip.INVITE, sip.ACK, sip.CANCEL, sip.BYE, sip.INFO, sip.OPTIONS, sip.REFER, sip.NOTIFY:
		return fmt.Errorf("diago: Handle: method %s is managed by the agent", method)
	}
	if dg.serving.Load() || dg.stopped.Load() {
		return fmt.Errorf("diago: Handle must be called before Serve")
	}
	dg.server.OnRequest(method, dg.wrapRequest(handler))
	return nil
}

// UA returns the sipgo UserAgent backing this agent. It is never nil after
// New; it is caller-owned when WithUserAgent was used, otherwise the agent
// closes it in Close.
func (dg *Agent) UA() *UserAgent {
	return dg.ua
}

// Server returns the sipgo SIP server used for incoming requests. It is the
// escape hatch for sipgo features diago does not expose; prefer Handle for
// registering additional methods and WithServer for a fully custom server.
func (dg *Agent) Server() *Server {
	return dg.server
}

// Client returns the shared sipgo client provided with WithClient, or nil
// when each transport uses its own client.
func (dg *Agent) Client() *Client {
	return dg.client
}

// Serve starts 'Server' handle for SIP.
// Should be called for UAS but can be skipped for UAC behavior
//
// Handler contract: the CallHandler decides how the call is answered.
// Returning before a final response was sent declines the pending INVITE
// (nil -> 480 decline, non-nil -> mapped response, see CallHandler).
// Returning after answering keeps the call alive: the dialog is owned by the
// framework until the peer ends it, Close runs or the handler hangs it up.
// Teardown of an answered call is always a BYE; the media and cache entry are
// released when the dialog ends.
//
// Listener contract: Serve returns the first listener error joined with the
// errors of the remaining listeners, and it only returns after every listener
// has stopped — a non-nil return means nothing is serving. This also applies
// to ServeBackground, which reports the same error while Serve is tearing the
// remaining listeners down.
func (dg *Agent) Serve(ctx context.Context, h CallHandler) error {
	return dg.serve(ctx, h, func() {})
}

// handler returns the installed CallHandler. Serve swaps it atomically, so
// request goroutines never race with a serve (re)start.
func (dg *Agent) handler() CallHandler {
	if p := dg.serveHandler.Load(); p != nil {
		return *p
	}
	return defaultCallHandler
}

func (dg *Agent) serve(ctx context.Context, h CallHandler, readyCh func()) error {
	if dg.stopped.Load() {
		return fmt.Errorf("diago is shut down and cannot serve again")
	}
	if !dg.serving.CompareAndSwap(false, true) {
		return fmt.Errorf("diago is already serving")
	}
	// Reset serving on every exit path from here on, including the
	// concurrent-Close refusal below.
	defer dg.serving.Store(false)

	if h == nil {
		h = defaultCallHandler
	}
	handler := h
	dg.serveHandler.Store(&handler)

	// Derived ctx so Close can stop the listeners even when Serve was
	// called with a caller-owned context.
	serveCtx, cancel := context.WithCancel(ctx)

	// A listener is "settled" once it either reported ready (its socket
	// exists) or exited. Cancelling before that can leak the listener inside
	// sipgo: its ctx-watcher goroutine only closes the socket when it already
	// has one, so a cancel during the startup window is lost.
	var pending atomic.Int32
	pending.Store(int32(len(dg.transports)))
	startupDone := make(chan struct{})
	markSettled := func() {
		if pending.Add(-1) == 0 {
			close(startupDone)
		}
	}

	dg.serveMu.Lock()
	if dg.stopped.Load() {
		// Close ran concurrently with this serve startup: refuse instead
		// of leaking listeners it could no longer stop.
		dg.serveMu.Unlock()
		cancel()
		return fmt.Errorf("diago is shut down and cannot serve again")
	}
	dg.serveCtx = serveCtx
	dg.serveCancel = cancel
	dg.serveStartupDone = startupDone
	dg.serveMu.Unlock()

	// Listeners get a diago-private context. The cancel signals of serveCtx
	// (caller ctx and Close) are forwarded only after every listener
	// settled: a cancel delivered during sipgo's startup window is dropped by
	// its ctx-watcher (no socket to close yet) and would leak the listener.
	listenerCtx, listenerCancel := context.WithCancel(context.Background())
	defer listenerCancel()
	_ = context.AfterFunc(serveCtx, func() {
		<-startupDone
		listenerCancel()
	})

	server := dg.server
	errCh := make(chan error, len(dg.transports))

	for i, tran := range dg.transports {
		hostport := net.JoinHostPort(tran.BindHost, strconv.Itoa(tran.BindPort))

		var settled sync.Once
		settle := func() { settled.Do(markSettled) }

		dg.serveWG.Add(1)
		go func(i int, tran Transport) {
			defer dg.serveWG.Done()
			defer settle()
			// Update transport
			//nolint:staticcheck // sipgo exposes ListenReadyCtxKey as an untyped string key.
			ctx := context.WithValue(listenerCtx, sipgo.ListenReadyCtxKey, sipgo.ListenReadyFuncCtxValue(func(network, addr string) {
				// This now fixes port for ephemeral binding
				// Alternative to use is tp.GetListenPort but it squashes networks
				_, port, _ := sip.ParseAddr(addr)
				if tran.BindPort == 0 {
					tran.BindPort = port
					tran.ExternalPort = port
					tran.client = dg.createClient(tran)
					dg.transports[i] = tran
				}
				settle()
				readyCh()

				dg.log.Info("Listening on transport", "addr", addr, "protocol", tran.network)
			}))

			if tran.Network == "tls" || tran.Network == "wss" {
				errCh <- server.ListenAndServeTLS(ctx, tlsListenNetwork(tran.network), hostport, tran.TLSConf)
				return
			}
			errCh <- server.ListenAndServe(ctx, tran.network, hostport)
		}(i, tran)
	}

	// On any listener failure, stop the remaining listeners and wait for them
	// so Serve returning an error means nothing is serving anymore. The wait
	// for startupDone first avoids the sipgo startup race described above.
	firstErr := <-errCh
	<-startupDone
	cancel()

	errs := make([]error, 0, len(dg.transports))
	errs = append(errs, firstErr)
	for i := 1; i < len(dg.transports); i++ {
		errs = append(errs, <-errCh)
	}
	dg.serveWG.Wait()
	return errors.Join(errs...)
}

// ServeBackground starts serving in background, but waits server listener to be started before returning
// Checkout more info on Serve()
func (dg *Agent) ServeBackground(ctx context.Context, h CallHandler) error {
	readyCh := make(chan struct{}, len(dg.transports))
	ready := func() {
		readyCh <- struct{}{}
	}
	chErr := make(chan error, 1)

	go func() {
		chErr <- dg.serve(ctx, h, ready)
	}()

	for range dg.transports {
		select {
		case err := <-chErr:
			return err
		case <-readyCh:
			dg.log.Info("Network ready")
		}
	}
	return nil
}

// shutdownDialogHangupTimeout bounds the per-dialog Hangup during Close.
// context.WithTimeout takes the earlier deadline when the passed ctx already
// has one.
const shutdownDialogHangupTimeout = 3 * time.Second

// Close gracefully stops the Agent instance:
//
//  1. It hangs up and closes every dialog tracked in the dialog caches
//     (server dialogs are tracked from the incoming INVITE, client dialogs
//     once confirmed). Media sockets close with the dialogs and the cache
//     entries are evicted.
//  2. It stops the SIP listeners started by Serve/ServeBackground and waits
//     for them to exit.
//  3. It closes the private UserAgent created by New. A UserAgent passed with
//     WithUserAgent is caller-owned and is NOT closed, so it can be shared
//     between agents; call ua.Close() yourself when done.
//
// Ownership boundaries:
//   - Outgoing dialogs that never reached an answer are not in the cache;
//     cancel their Invite contexts instead (which sends CANCEL).
//   - Registration handles are caller-owned: call Registration.Close (which
//     unregisters the contact and stops the loop) or cancel their context
//     around Close.
//
// In-flight serve handlers keep running concurrently with phase 1; their own
// Hangup/Close on an already-torn-down dialog is a tolerated no-op path.
// Close is idempotent. After Close, Serve/ServeBackground return an
// error.
func (dg *Agent) Close(ctx context.Context) error {
	dg.stopped.Store(true)

	var errs []error
	// Phase 1: tear down dialogs while the signaling stack is still alive so
	// the BYE/decline messages can actually be sent.
	errs = append(errs, dg.shutdownDialogs(ctx)...)

	// Phase 2: stop the listeners and wait for them to exit.
	dg.serveMu.Lock()
	cancel := dg.serveCancel
	startupDone := dg.serveStartupDone
	dg.serveMu.Unlock()

	// Wait for every listener to settle (socket created or listener out of
	// its startup window) before cancelling: cancelling earlier can leak a
	// listener inside sipgo (see serve).
	if startupDone != nil {
		select {
		case <-startupDone:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("shutdown timed out waiting for listeners to start: %w", ctx.Err()))
		}
	}
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		dg.serveWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		// serve()'s own defer clears this too, but do it here so callers
		// observe a consistent state right after Close returns.
		dg.serving.Store(false)
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("shutdown timed out waiting for listeners: %w", ctx.Err()))
	}

	// Phase 3: close the private UserAgent. A caller-owned one (WithUserAgent)
	// is intentionally left open.
	if dg.ownedUA && dg.ua != nil {
		if err := dg.ua.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close user agent: %w", err))
		}
	}

	return errors.Join(errs...)
}

func (dg *Agent) shutdownDialogs(ctx context.Context) []error {
	var errs []error
	if err := dg.cache.server.rangeDialogs(ctx, func(id string, d *InboundCall) bool {
		errs = append(errs, shutdownDialog(ctx, id, d)...)
		return true
	}); err != nil {
		errs = append(errs, err)
	}
	if err := dg.cache.client.rangeDialogs(ctx, func(id string, d *OutboundCall) bool {
		errs = append(errs, shutdownDialog(ctx, id, d)...)
		return true
	}); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func shutdownDialog(ctx context.Context, id string, d Call) []error {
	var errs []error
	hctx, cancel := context.WithTimeout(ctx, shutdownDialogHangupTimeout)
	defer cancel()
	if err := d.Hangup(hctx); err != nil {
		errs = append(errs, fmt.Errorf("hangup dialog %q: %w", id, err))
	}
	if err := d.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close dialog %q: %w", id, err))
	}
	return errs
}

// Dial makes outgoing call leg and waits for answer.
// target is a SIP URI string ("sip:bob@example.com",
// "sip:alice@host:5060;transport=tcp"). Use DialURI when you already parsed
// a URI.
//
// It is a helper that calls NewCall and Invite for you.
// Options allow full call customization, checkout SignalOption.
// Each SignalOption func is executed exactly once for the whole call setup.
// The final ACK is sent without options; call Ack directly if you need ACK headers.
// Scope: everything OutboundCall.Invite accepts, plus
// WithDialogTransport and WithDialogTransportID.
//
// For better control more details use above functions instead.
// If you want to bridge call then use helper DialBridge
func (dg *Agent) Dial(ctx context.Context, target string, opts ...DialOption) (*OutboundCall, error) {
	uri, err := parseTargetURI(target)
	if err != nil {
		return nil, err
	}
	return dg.DialURI(ctx, uri, opts...)
}

// DialURI is Dial for an already parsed SIP URI. The URI is copied, so the
// caller's value is never mutated (unlike v2, the "transport" URI parameter
// is consumed from the copy only).
func (dg *Agent) DialURI(ctx context.Context, recipient URI, opts ...DialOption) (d *OutboundCall, err error) {
	params, err := newSignalParams(opts...)
	if err != nil {
		return nil, err
	}
	d, err = dg.newDialogWithParams(recipient, params)
	if err != nil {
		return nil, err
	}

	if err := d.inviteWithParams(ctx, params); err != nil {
		closeErr := d.Close()
		return nil, errors.Join(err, closeErr)
	}

	if err := d.Ack(ctx); err != nil {
		closeErr := d.Close()
		return nil, errors.Join(err, closeErr)
	}
	return d, nil
}

// parseTargetURI parses a dial target string into a private URI copy.
// Failing parse or an empty target returns ErrInvalidTarget.
func parseTargetURI(target string) (URI, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return URI{}, fmt.Errorf("empty SIP target: %w", ErrInvalidTarget)
	}
	var uri URI
	if err := sip.ParseUri(target, &uri); err != nil {
		return URI{}, fmt.Errorf("%w: %q: %w", ErrInvalidTarget, target, err)
	}
	if strings.TrimSpace(uri.Host) == "" {
		return URI{}, fmt.Errorf("%w: %q has no host", ErrInvalidTarget, target)
	}
	return cloneURI(uri), nil
}

// cloneURI copies the URI and its parameter slices so library code can
// consume them (for example the "transport" parameter) without mutating the
// caller's value. URI is passed by value but its params are slices.
func cloneURI(uri URI) URI {
	uri.UriParams = uri.UriParams.Clone()
	uri.Headers = uri.Headers.Clone()
	return uri
}

// DialBridge makes outgoing call leg and does bridging.
// target is a SIP URI string; use DialBridgeURI for a parsed URI.
// Outgoing session will be added into bridge on answer
// If bridge is a *Bridge with Originator (first participant) it will be used
// for creating outgoing call leg as in B2BUA.
// When bridge is provided then this call will be bridged with any participant
// already present in bridge.
// Scope: same as Dial; for a *Bridge, Originator defaults to the bridge
// originator when not set. Options execute exactly once.
func (dg *Agent) DialBridge(ctx context.Context, target string, bridge Bridger, opts ...DialOption) (*OutboundCall, error) {
	uri, err := parseTargetURI(target)
	if err != nil {
		return nil, err
	}
	return dg.DialBridgeURI(ctx, uri, bridge, opts...)
}

// DialBridgeURI is DialBridge for an already parsed SIP URI.
func (dg *Agent) DialBridgeURI(ctx context.Context, recipient URI, bridge Bridger, opts ...DialOption) (d *OutboundCall, err error) {
	if isNilBridger(bridge) {
		return nil, fmt.Errorf("dial bridge: bridge is nil")
	}
	params, err := newSignalParams(opts...)
	if err != nil {
		return nil, err
	}
	d, err = dg.newDialogWithParams(recipient, params)
	if err != nil {
		return nil, err
	}

	// Keep things compatible: bridge originator is default when not provided.
	// Params are computed once above, so user-supplied SignalOption funcs
	// run a single time across dialog creation and Invite.
	if params == nil || params.Dialog.Originator == nil {
		if b, ok := bridge.(*Bridge); ok {
			if orig := b.originator(); orig != nil {
				params.Dialog.Originator = orig
			}
		}
	}

	if err := d.inviteWithParams(ctx, params); err != nil {
		return nil, errors.Join(err, d.Hangup(d.Context()), d.Close())
	}

	// Do bridging now
	if err := bridge.Add(d); err != nil {
		return nil, errors.Join(err, d.Close())
	}

	if err := d.Ack(ctx); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return d, nil
}

// isNilBridger reports whether bridge is nil, including a typed-nil pointer
// stored in the interface (ex. var b *Bridge; DialBridge(ctx, uri, b)).
// Without this check the type assertion succeeds and the originator access
// dereferences a nil receiver.
func isNilBridger(bridge Bridger) bool {
	if bridge == nil {
		return true
	}
	v := reflect.ValueOf(bridge)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// NewCall creates a new outbound call for manual control: call Invite(...)
// and then Ack(...) yourself. For the common case use Dial, which performs
// Invite and the final ACK for you.
// Options allow selecting transport (WithDialogTransport, WithDialogTransportID)
// and overriding Contact.
// Scope: WithDialogTransport, WithDialogTransportID, WithContact.
// Pass the remaining options to the later Invite call; this method rejects them.
func (dg *Agent) NewCall(recipient URI, opts ...NewCallOption) (d *OutboundCall, err error) {
	params, err := newSignalParams(opts...)
	if err != nil {
		return nil, err
	}
	return dg.newDialogWithParams(recipient, params)
}

// newDialogWithParams creates a client dialog session from pre-computed params.
// It exists so helpers (Dial, DialBridge) can execute user-supplied
// SignalOption funcs a single time across dialog creation and Invite.
func (dg *Agent) newDialogWithParams(recipient URI, params *signalParams) (d *OutboundCall, err error) {
	// Never mutate the caller's URI params (they share the backing slice).
	recipient = cloneURI(recipient)
	transport := ""
	transportID := ""
	var contact *ContactHeader
	if params != nil {
		transport = params.Dialog.Transport
		transportID = params.Dialog.TransportID
		contact = params.Msg.Contact
	}

	if transport == "" && recipient.UriParams != nil {
		if t, ok := recipient.UriParams.Get("transport"); t != "" && ok {
			transport = t
			recipient.UriParams.Remove("transport")
		}

	}
	tran, exists := dg.findTransport(transport, transportID)
	if !exists {
		if transport == "" {
			return nil, fmt.Errorf("transport %q does not exist: no default UDP transport configured", "udp")
		}
		return nil, fmt.Errorf("transport %q does not exist", transport)
	}

	return dg.newSIPDialog(recipient, tran, contact)
}

// mediaConfigForTransport selects the media config for dialogs carried over
// the transport: the profile named by Transport.Profile, or the default
// (WithMediaConfig) when no profile is selected. The selected profile is used
// as is; only the transport-derived bind/advertise addresses fill fields the
// profile leaves nil (a concrete BindHost supplies the media bind IP, an
// explicit ExternalHost supplies the advertised IP). A configured DTLS
// profile implies DTLS-SRTP unless an explicit SRTP mode was chosen.
func (dg *Agent) mediaConfigForTransport(tran *Transport) MediaConfig {
	conf := dg.mediaConf
	if tran.Profile != "" {
		if p, ok := dg.mediaProfiles[tran.Profile]; ok {
			conf = p
		} else {
			// New validates transport profiles; this is a defensive fallback
			// for Transport values built by hand/tests.
			dg.log.Warn("Media profile not registered, using default", "profile", tran.Profile)
		}
	}

	if conf.Network.BindIP == nil {
		if bindIP := tran.bindIP; bindIP != nil && !bindIP.IsUnspecified() {
			conf.Network.BindIP = bindIP
		} else if bindIP := net.ParseIP(tran.BindHost); bindIP != nil && !bindIP.IsUnspecified() {
			// A Transport built as a literal (not through WithTransport) still
			// pins a concrete BindHost.
			conf.Network.BindIP = bindIP
		}
	}
	if conf.Network.ExternalIP == nil && tran.mediaExternalIP != nil {
		conf.Network.ExternalIP = tran.mediaExternalIP
	}

	if conf.SRTP.Mode == SecureRTPNone && !dtlsConfIsZero(conf.SRTP.DTLS) {
		conf.SRTP.Mode = SecureRTPDTLS
	}
	return conf
}

// dtlsConfIsZero reports whether conf carries no DTLS configuration. It is a
// field-by-field check because media.DTLSConfig contains a func field and is
// therefore not comparable with ==.
func dtlsConfIsZero(conf media.DTLSConfig) bool {
	return len(conf.Certificates) == 0 &&
		conf.ServerName == "" &&
		conf.ServerClientAuth == 0 &&
		len(conf.SRTPProfiles) == 0 &&
		conf.SDPSetupRole == nil &&
		len(conf.EllipticCurves) == 0
}

func (dg *Agent) newSIPDialog(recipient URI, tran *Transport, contact *ContactHeader) (d *OutboundCall, err error) {
	transport := tran.Network

	// TODO: remove this alloc of UA each time
	client := dg.getClient(tran)
	dialogUA := sipgo.DialogUA{
		Client:         client,
		RewriteContact: tran.RewriteContact,
	}
	dg.contactHDRFromTransport(tran, &dialogUA.ContactHDR)
	if contact != nil {
		// Per dialog contact override
		dialogUA.ContactHDR = *contact.Clone()
	}

	inviteReq := sip.NewRequest(sip.INVITE, recipient)
	inviteReq.SetTransport(sip.NetworkToUpper(transport))

	d = &OutboundCall{
		DialogClientSession: &sipgo.DialogClientSession{
			UA: &dialogUA,
			Dialog: Dialog{
				InviteRequest: inviteReq,
			},
		},
	}
	d.Init()

	// Per-dialog media config: global media config with transport values
	// overlaid, so it carries all fields (codecs, RTPNAT, SRTP alg, port
	// range, codec order).
	d.mediaConfig = dg.mediaConfigForTransport(tran)
	d.callMedia.log = d.mediaConfig.Logger
	d.callMedia.httpClient = d.mediaConfig.HTTPClient

	// This should be run on ACK
	d.OnState(func(s sip.DialogState) {
		if s != sip.DialogStateConfirmed {
			return
		}

		// Now dialog is established and can be add into store
		if err := dg.cache.client.store(context.Background(), d.DialogClientSession.ID, d); err != nil {
			dg.log.Error("Failed to store in dialog cache", "error", err)
		}
	})

	d.OnClose(func() error {
		return dg.cache.client.remove(context.Background(), d.DialogClientSession.ID)
	})
	return d, nil
}

func (dg *Agent) contactHDRFromTransport(tran *Transport, contact *ContactHeader) {
	// Find contact hdr matching transport
	scheme := "sip"
	if tran.TLSConf != nil && !tran.TLSURINoSIPS {
		scheme = "sips"
	}

	contact.DisplayName = "" //TODO
	contact.Address = URI{
		Scheme:    scheme,
		User:      dg.ua.Name(),
		Host:      tran.ExternalHost,
		Port:      tran.ExternalPort,
		UriParams: sip.NewParams(),
		Headers:   sip.NewParams(),
	}
	// Transport should be reflected in contact.
	// Set this only if we are using same as bind host, as external network could be different
	// Example TLS termination. External is TLS and this is TCP
	if tran.ExternalHost == tran.BindHost {
		contact.Address.UriParams.Add("transport", tran.Network)
	}
}

func (dg *Agent) getClient(tran *Transport) *Client {
	if dg.client != nil {
		// Use global one if exists
		return dg.client
	}

	return tran.client
}

func (dg *Agent) getTransport(transport string) (*Transport, bool) {
	if transport == "" {
		return &dg.transports[0], true
	}
	for _, t := range dg.transports {
		if sip.NetworkToLower(transport) == t.Network {
			return &t, true
		}
	}
	return nil, false
}

func (dg *Agent) findTransport(transport string, id string) (*Transport, bool) {
	if id != "" {
		for _, t := range dg.transports {
			if id == t.ID {
				return &t, true
			}
		}
		return nil, false
	}

	if transport != "" {
		return dg.getTransport(transport)
	}

	return dg.getTransport("udp")
}

// Register starts a REGISTER transaction that keeps the registration ongoing
// in the background and returns a handle to it. target is a SIP URI string
// ("sip:alice@example.com"); use RegisterURI for a parsed URI. The initial
// REGISTER is sent asynchronously; use Registration.WaitReady to wait for the
// first success. For granular one-shot control use RegisterTransaction.
//
// The returned handle owns the loop: Registration.Close stops it and
// unregisters the contact; Registration.Wait reports the terminal error.
// Cancel the passed ctx to stop the loop without unregistering explicitly
// (Close is still recommended to wait for the exit).
//
// Retry semantics: a terminal REGISTER response (401/407, other 4xx, 3xx, 6xx)
// ends the loop and is reported by Err/Wait. 423 Interval Too Brief is retried
// once with the server's Min-Expires (RFC 3261 §10.2.8); 408/491 and 5xx
// (except 501/505) are retried honoring Retry-After. Failures without a final
// response (transport/DNS errors, unusable digest challenges) are retried
// indefinitely with a 5s floor: the only way to stop the loop is canceling ctx
// or Close, so callers that need an upper bound must pass a
// bounded/cancelable ctx.
//
// Options configure the transaction (WithRegisterExpiry,
// WithRegisterRetryInterval, WithRegisterProxyHost, WithRegisterAllowHeaders,
// WithOnRegistered) and every REGISTER attempt (WithAuthCredentials,
// WithContact, WithHeaders, WithRequestMutator).
// Scope: WithHeaders, WithContact, WithRequestMutator, WithAuthCredentials,
// WithRegisterExpiry, WithRegisterRetryInterval, WithRegisterAllowHeaders,
// WithRegisterProxyHost, WithOnRegistered.
func (dg *Agent) Register(ctx context.Context, target string, opts ...RegisterOption) (*Registration, error) {
	uri, err := parseTargetURI(target)
	if err != nil {
		return nil, err
	}
	return dg.RegisterURI(ctx, uri, opts...)
}

// RegisterURI is Register for an already parsed SIP URI.
func (dg *Agent) RegisterURI(ctx context.Context, recipient URI, opts ...RegisterOption) (*Registration, error) {
	t, err := dg.RegisterTransaction(ctx, recipient, opts...)
	if err != nil {
		return nil, err
	}
	return newRegistration(ctx, dg.log, t), nil
}

// RegisterTransaction creates the REGISTER transaction object used for
// Register and Unregister requests.
// Options configure the transaction (see Register) and every REGISTER
// attempt. Scope: WithHeaders, WithContact, WithRequestMutator,
// WithAuthCredentials, WithRegisterExpiry, WithRegisterRetryInterval,
// WithRegisterAllowHeaders, WithRegisterProxyHost, WithOnRegistered.
func (dg *Agent) RegisterTransaction(ctx context.Context, recipient URI, opts ...RegisterOption) (*RegisterTransaction, error) {
	params, err := newSignalParams(opts...)
	if err != nil {
		return nil, err
	}
	// Make our client reuse address
	transport, exists := recipient.UriParams.Get("transport")
	if !exists {
		transport = "udp"
	}
	tran, exists := dg.getTransport(transport)
	if !exists {
		return nil, fmt.Errorf("transport=%q does not exist", transport)
	}

	contactHDR := ContactHeader{}
	dg.contactHDRFromTransport(tran, &contactHDR)

	// client, err := sipgo.NewClient(dg.ua,
	// 	sipgo.WithClientHostname(contactHDR.Address.Host),
	// 	// sipgo.WithClientPort(lport),
	// 	sipgo.WithClientNAT(), // add rport support
	// )
	// if err != nil {
	// 	return nil, err
	// }
	client := dg.getClient(tran)
	after := dg.registerAfter
	if after == nil {
		after = time.After
	}
	return newRegisterTransaction(client, recipient, contactHDR, dg.log, params, after), nil
}

func (dg *Agent) createClient(tran Transport) (client *Client) {
	ua := dg.ua
	// When transport is not binding to specific IP

	opts := []sipgo.ClientOption{
		sipgo.WithClientNAT(),
		sipgo.WithClientLogger(dg.log),
	}

	// UDP is good to be reused
	// 1. Prefer resolved IP when sending out, as sending as unspecifed makes issue in receiving responses
	// 2. Use bind port if IP is specified. ISSUE: if ip is unspecified and SIP starts listen on 0.0.0.0:5060, SIPGO will try to use 192.168.1.1:5060 and therefore fail
	if tran.Network == "udp" {
		resolvedIP := tran.bindIP
		bindPort := tran.BindPort
		if resolvedIP != nil {
			if resolvedIP.IsUnspecified() && tran.mediaBindIP != nil {
				resolvedIP = tran.mediaBindIP
				bindPort = 0
			}
		}

		hostname := ""
		if resolvedIP != nil && !resolvedIP.IsUnspecified() {
			hostname = resolvedIP.String()
		}

		if hostname != "" {
			opts = append(opts, sipgo.WithClientConnectionAddr(net.JoinHostPort(hostname, strconv.Itoa(bindPort))))
		}
	}

	// Are we behind public IP
	if tran.ExternalHost != tran.BindHost {
		opts = append(opts, sipgo.WithClientHostname(tran.ExternalHost))
		opts = append(opts, sipgo.WithClientPort(tran.ExternalPort))
	}

	cli, err := sipgo.NewClient(ua, opts...)
	if err != nil {
		dg.log.Error("Failed to create transport client", "error", err)
		cli, _ = sipgo.NewClient(ua) // Make some defaut
	}
	return cli
}

// FindCall resolves an in-dialog SIP request to the dialog tracked by
// this engine. It mirrors internal routing order: server (UAS) role via
// DialogIDFromRequestUAS first, then client (UAC) via DialogIDFromRequestUAC.
// ok is false for out-of-dialog, missing headers, or unknown dialogs. When ok,
// the returned Call is the live *InboundCall or
// *OutboundCall (type-assert if role matters).
func (dg *Agent) FindCall(req *Request) (Call, bool) {
	if dg == nil || req == nil {
		return nil, false
	}
	sd, cd, err := dg.cache.matchDialog(req)
	if err != nil {
		return nil, false
	}
	if sd != nil {
		return sd, true
	}
	if cd != nil {
		return cd, true
	}
	return nil, false
}
