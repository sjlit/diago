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

type Diago struct {
	ua         *sipgo.UserAgent
	client     *sipgo.Client
	server     *sipgo.Server
	transports []Transport

	serveHandler DialogHandler

	mediaConf MediaConfig

	log *slog.Logger

	cache            dialogCachePool
	serverMiddleware func(next sipgo.RequestHandler) sipgo.RequestHandler

	// registerAfter is the timer factory used between REGISTER attempts of
	// Register/RegisterTransaction. It defaults to time.After and exists as a
	// field so tests can exercise retry pacing without wall-clock waits.
	registerAfter func(time.Duration) <-chan time.Time

	// Serve lifecycle state. serveWG tracks the listener goroutines started
	// by serve(); serveCancel cancels them. stopped is terminal: once
	// Shutdown has run, serving is refused.
	serveMu          sync.Mutex
	serveCtx         context.Context
	serveCancel      context.CancelFunc
	serveStartupDone chan struct{}
	serveWG          sync.WaitGroup
	serving          atomic.Bool
	stopped          atomic.Bool
}

// We can extend this WithClientOptions, WithServerOptions

type DiagoOption func(dg *Diago)

type Transport struct {
	ID string

	// Transport must be udp,tcp or ws, or even forcing v4 like udp4, tcp4
	Transport string
	network   string // network will keep original transport value

	// BindHost sets IP to bind. If specified (not 0.0.0.0) it will be used same for media IP unless Media.Network.BindIP is set.
	BindHost string
	// BindPort sets port to bind. Leaving at 0 will use empheral port and apply on Contact addr
	BindPort int
	bindIP   net.IP

	ExternalHost string // SIP signaling and media external addr
	ExternalPort int

	// Media overrides the global MediaConfig for dialogs carried over this
	// transport. Zero fields inherit the global value; a field set here wins.
	Media TransportMediaConfig

	mediaBindIP net.IP

	// In case TLS protocol
	TLSConf *tls.Config
	// Avoiding SIPS in contact uri https://datatracker.ietf.org/doc/html/rfc5630#section-3.3
	TLSURINoSIPS bool

	RewriteContact bool

	client *sipgo.Client
}

// NewTransport builds a Transport for the given network and validates it.
// Accepted networks are udp, tcp, tls, ws and wss, optionally suffixed with
// 4 or 6 to force the IP family (udp4, tcp6, ...). Invalid values fail here
// instead of at serve time.
func NewTransport(network string) (Transport, error) {
	t := Transport{Transport: network}
	if err := t.Validate(); err != nil {
		return Transport{}, err
	}
	return t, nil
}

// Validate reports whether the transport can be served by NewDiago/serve.
// The network must be udp, tcp, tls, ws or wss with an optional 4/6 suffix;
// tls and wss require TLSConf (and no other network may carry one). tls6 is
// rejected: the underlying SIP stack has no TLS-over-IPv6 listener. The
// remaining suffix forms are translated by tlsListenNetwork at serve time.
func (t Transport) Validate() error {
	// WithTransport trims the 4/6 suffix from Transport and keeps the raw
	// value in network, so validate the original spelling when present.
	raw := strings.TrimSpace(t.Transport)
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
			return fmt.Errorf("network %q is not supported by the SIP stack: use tls, tls4 or wss with an optional 4/6 suffix", raw)
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
// (udp on 127.0.0.1:5060). v2 never adds it implicitly: NewDiago fails
// without at least one configured transport.
func WithDefaultTransport() DiagoOption {
	return WithTransport(Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  5060,
	})
}

func WithTransport(t Transport) DiagoOption {
	return func(dg *Diago) {
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

		if t.Media.Network.ExternalIP == nil && (externalExplicit || t.bindIP == nil || !t.bindIP.IsUnspecified()) {
			// try to use external host as external media IP
			if extIp != nil && !extIp.IsUnspecified() {
				t.Media.Network.ExternalIP = extIp
			}
		}

		t.Transport = sip.NetworkToLower(strings.TrimSpace(t.Transport))
		t.network = t.Transport
		t.Transport = strings.TrimSuffix(t.Transport, "4") // udp4, tcp4
		t.Transport = strings.TrimSuffix(t.Transport, "6") // udp6, tcp6

		// we want to handle SIP networking better per each transport
		t.client = dg.createClient(t)
		dg.transports = append(dg.transports, t)

		dg.log.Debug("Loaded transport", "t", t)
	}
}

// SecureRTP selects the SRTP keying mode of a media session.
type SecureRTP int

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

const (
	RTPNATDisabled RTPNATMode = iota
	// RTPNATLearnSource learns the remote source address from incoming
	// packets (symmetric RTP; media.RTPNATSymetric).
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

// MediaConfig configures the media sessions created by Diago.
type MediaConfig struct {
	Codecs  []media.Codec
	SRTP    SRTPConfig
	Network NetworkConfig
	SDP     SDPConfig

	// MusicOnHold is the dialog-level default hold music: Hold() starts
	// looping it automatically once the hold re-INVITE succeeds. Zero value
	// (no segments) disables automatic hold music; Hold then behaves as
	// before. Per-call override: WithMusicOnHold; manual control:
	// DialogMedia.PlayMusicOnHold/StopMusicOnHold.
	MusicOnHold audio.Tone

	// Logger receives media-pipeline logs: media package internals (RTP/RTCP,
	// negotiation, DTLS debug) and dialog-level components (hold music, DTMF
	// relay, playback). Nil falls back to the Diago logger; when that is also
	// unset the media package default (slog.Default) is used.
	Logger *slog.Logger
}

// TransportMediaConfig overrides the transport-scoped parts of MediaConfig
// (SRTP and Network) for dialogs carried over a specific Transport. Zero
// fields inherit the global MediaConfig; the precedence for a field is:
// explicit Transport value, then a value derived from BindHost/ExternalHost,
// then MediaConfig. Codecs, SDP, MusicOnHold and Logger are global-only.
type TransportMediaConfig struct {
	SRTP    SRTPConfig
	Network NetworkConfig
}

// WithMediaConfig replaces the global media configuration. It is a whole
// struct replacement, not a merge: zero fields keep their documented library
// defaults (see NetworkConfig and SDPConfig). Codecs left nil would fail
// session creation, so start from the defaults you need.
func WithMediaConfig(conf MediaConfig) DiagoOption {
	return func(dg *Diago) {
		dg.mediaConf = conf
	}
}

// WithServer allows providing custom server handle. Consider still it needs to use same UA as diago
func WithServer(srv *sipgo.Server) DiagoOption {
	return func(dg *Diago) {
		dg.server = srv
	}
}

// WithClient allows providing custom client handle. Consider still it needs to use same UA as diago
func WithClient(client *sipgo.Client) DiagoOption {
	return func(dg *Diago) {
		dg.client = client
	}
}

// WithLogger sets the logger for sipgo, the Diago engine, and the media
// pipeline logs (hold music, DTMF relay, playback and media internals) unless
// MediaConfig.Logger explicitly overrides it.
func WithLogger(l *slog.Logger) DiagoOption {
	return func(dg *Diago) {
		dg.log = l
	}
}

func WithServerRequestMiddleware(f func(next sipgo.RequestHandler) sipgo.RequestHandler) DiagoOption {
	return func(dg *Diago) {
		dg.serverMiddleware = f
	}
}

// NewDiago constructs a b2b user agent that will act as server and client.
// It fails when no transport is configured (the v1 implicit 127.0.0.1:5060
// default was removed) or when a configured transport is invalid; use
// WithTransport or WithDefaultTransport.
func NewDiago(ua *sipgo.UserAgent, opts ...DiagoOption) (*Diago, error) {
	dg := &Diago{
		ua:  ua,
		log: sip.DefaultLogger(),
		// Refuse calls until Serve installs a real handler.
		serveHandler: defaultDialogHandler,
		transports:   []Transport{},
		mediaConf: MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
		},

		cache: dialogCachePool{
			client: &dialogCacheMap[*DialogClientSession]{sync.Map{}},
			server: &dialogCacheMap[*DialogServerSession]{sync.Map{}},
		},

		registerAfter: time.After,
	}

	for _, o := range opts {
		o(dg)
	}

	// Media logs follow the Diago logger unless a MediaConfig explicitly
	// carries its own.
	if dg.mediaConf.Logger == nil {
		dg.mediaConf.Logger = dg.log
	}

	if len(dg.transports) == 0 {
		return nil, fmt.Errorf("diago: no transport configured: use WithTransport or WithDefaultTransport")
	}
	for i := range dg.transports {
		if err := dg.transports[i].Validate(); err != nil {
			return nil, fmt.Errorf("diago: transport %d: %w", i, err)
		}
	}

	if dg.server == nil {
		srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(dg.log))
		if err != nil {
			return nil, fmt.Errorf("diago: failed to create default sip server: %w", err)
		}
		dg.server = srv
	}
	server := dg.server

	errHandler := func(f func(req *sip.Request, tx sip.ServerTransaction) error) sipgo.RequestHandler {
		handler := func(req *sip.Request, tx sip.ServerTransaction) {
			if err := f(req, tx); err != nil {
				dg.log.Warn("Failed to handle request", "error", err, "req.method", req.Method.String(), "req.from", req.From().String(), "req.to", req.To().Value())
				return
			}
			// Termination gracefull will be done by sipgo now
		}

		// Return handler wrapped with middleware if user supprots
		if dg.serverMiddleware != nil {
			return func(req *sip.Request, tx sip.ServerTransaction) {
				dg.serverMiddleware(handler)(req, tx)
			}
		}
		return handler
	}

	handleNoDialog := func(req *sip.Request, tx sip.ServerTransaction, err error) error {
		var response *sip.Response
		if errors.Is(err, sipgo.ErrDialogDoesNotExists) {
			response = sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		} else {
			response = sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil)
		}
		return errors.Join(err, tx.Respond(response))
	}

	server.OnInvite(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
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
			return fmt.Errorf("handling new INVITE failed: %w", err)
		}

		// Server-side digest auth is opt-in: call DialogServerSession.Authorize
		// (DigestAuthServer.AuthorizeDialog) as the first action of your serve
		// handler to challenge/validate the INVITE (RFC 2617).
		mediaConf := dg.mediaConfigForTransport(tran)
		dWrap := &DialogServerSession{
			DialogServerSession: dialog,
			dialogMedia:         DialogMedia{log: mediaConf.Logger},
			// TODO we may actually just build media session with this conf here
			mediaConf: mediaConf,
		}

		defer closeAndLog(dWrap.logger(), dWrap, "closing dialog server returned error")

		if err := dg.cache.server.store(dWrap.Context(), dWrap.DialogServerSession.ID, dWrap); err != nil {
			return fmt.Errorf("failed to store server dialog: %w", err)
		}
		defer func() {
			// TODO: have better context
			if err := dg.cache.server.remove(context.Background(), dWrap.DialogServerSession.ID); err != nil {
				dg.log.Error("Failed to delete server dialog", "error", err)
			}
		}()

		hctx, hcancel := dg.dialogHandlerContext(dialog.Context())
		herr := dg.serveHandler(hctx, dWrap)
		hcancel()

		// Check is dialog closed
		dialogCtx := dialog.Context()
		// Always try hanguping call
		ctx, cancel := context.WithTimeout(dialogCtx, 10*time.Second)
		defer cancel()

		if herr != nil {
			if err := dWrap.respondHandlerError(ctx, herr); err != nil {
				return fmt.Errorf("handler error handling failed: %w", err)
			}
			return nil
		}

		if err := dWrap.Hangup(ctx); err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				// Already hangup
				return nil
			}

			return fmt.Errorf("hanguping call failed: %w", err)
		}
		return nil
	}))

	server.OnCancel(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
		// INVITE transaction should be terminated by transaction layer and 200 response will be sent
		// In case of stateless proxy this we would need to forward
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
	}))

	server.OnAck(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
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

	server.OnBye(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
		sd, cd, err := dg.cache.matchDialog(req)
		if err != nil {
			return handleNoDialog(req, tx, err)
		}

		// Respond to BYE
		// Terminate our media processing
		// As user may stuck in playing or reading media, this unblocks that goroutine
		if cd != nil {
			defer closeAndLog(cd.logger(), &cd.dialogMedia, "failed to close client media")

			return cd.ReadBye(req, tx)
		}

		defer closeAndLog(sd.logger(), &sd.dialogMedia, "failed to close server media")
		return sd.ReadBye(req, tx)
	}))

	server.OnInfo(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
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
	dg.server.OnOptions(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
		methods := dg.server.RegisteredMethods()
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		res.AppendHeader(sip.NewHeader("Allow", strings.Join(methods, ", ")))
		res.AppendHeader(sip.NewHeader("Accept", "application/sdp"))
		// res.AppendHeader(sip.NewHeader("Supported", "replaces, 100rel"))
		return tx.Respond(res)
	}))

	dg.server.OnRefer(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
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

	dg.server.OnNotify(errHandler(func(req *sip.Request, tx sip.ServerTransaction) error {
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
	// server.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
	// 	d, err := matchDialogServer(req)
	// 	if err != nil {
	// 		// Security? When to answer this?
	// 		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, err.Error(), nil))
	// 		return
	// 	}
	// })

	return dg, nil
}

func (dg *Diago) handleReInvite(req *sip.Request, tx sip.ServerTransaction, id string) error {
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

// Serve starts 'Server' handle for SIP.
// Should be called for UAS but can be skipped for UAC behavior
//
// Handler contract: when the DialogHandler returns,
// the framework tears the call down — a nil error hangs up the dialog (BYE
// once confirmed, otherwise a 480 decline) with a 10s timeout, then closes
// the dialog and its media. A non-nil error is mapped to a final response
// (or a BYE Reason header when already answered), see DialogHandler. A
// handler that wants the call alive must block until the call ends (or hang
// up itself before returning); goroutines spawned in the handler must not
// rely on the dialog afterwards.
//
// Listener contract: Serve returns the first listener error joined with the
// errors of the remaining listeners, and it only returns after every listener
// has stopped — a non-nil return means nothing is serving. This also applies
// to ServeBackground, which reports the same error while Serve is tearing the
// remaining listeners down.
func (dg *Diago) Serve(ctx context.Context, h DialogHandler) error {
	return dg.serve(ctx, h, func() {})
}

func (dg *Diago) serve(ctx context.Context, h DialogHandler, readyCh func()) error {
	if dg.stopped.Load() {
		return fmt.Errorf("diago is shut down and cannot serve again")
	}
	if !dg.serving.CompareAndSwap(false, true) {
		return fmt.Errorf("diago is already serving")
	}
	if h == nil {
		h = defaultDialogHandler
	}
	dg.serveHandler = h

	// Derived ctx so Shutdown can stop the listeners even when Serve was
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
		// Shutdown ran concurrently with this serve startup: refuse instead
		// of leaking listeners it could no longer stop.
		dg.serveMu.Unlock()
		cancel()
		return fmt.Errorf("diago is shut down and cannot serve again")
	}
	dg.serveCtx = serveCtx
	dg.serveCancel = cancel
	dg.serveStartupDone = startupDone
	dg.serveMu.Unlock()

	defer dg.serving.Store(false)

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
			ctx := context.WithValue(serveCtx, sipgo.ListenReadyCtxKey, sipgo.ListenReadyFuncCtxValue(func(network, addr string) {
				// This now fixes port for empheral binding
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

			if tran.Transport == "tls" || tran.Transport == "wss" {
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
func (dg *Diago) ServeBackground(ctx context.Context, h DialogHandler) error {
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

// shutdownDialogHangupTimeout bounds the per-dialog Hangup during Shutdown.
// context.WithTimeout takes the earlier deadline when the passed ctx already
// has one.
const shutdownDialogHangupTimeout = 3 * time.Second

// Shutdown gracefully stops the Diago instance:
//
//  1. It hangs up and closes every dialog tracked in the dialog caches
//     (server dialogs are tracked from the incoming INVITE, client dialogs
//     once confirmed). Media sockets close with the dialogs and the cache
//     entries are evicted.
//  2. It stops the SIP listeners started by Serve/ServeBackground and waits
//     for them to exit.
//
// Ownership boundaries:
//   - The UserAgent is NOT closed: it is caller-owned and may be shared by
//     multiple Diago instances. Call ua.Close() yourself when done.
//   - Outgoing dialogs that never reached an answer are not in the cache;
//     cancel their Invite contexts instead (which sends CANCEL).
//   - Register transactions are caller-owned goroutines: cancel their
//     context (which triggers Unregister/loop exit) around Shutdown.
//
// In-flight serve handlers keep running concurrently with phase 1; their own
// Hangup/Close on an already-torn-down dialog is a tolerated no-op path.
// Shutdown is idempotent. After Shutdown, Serve/ServeBackground return an
// error. Register transaction loops must be cancelled by their owner.
func (dg *Diago) Shutdown(ctx context.Context) error {
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
		// observe a consistent state right after Shutdown returns.
		dg.serving.Store(false)
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("shutdown timed out waiting for listeners: %w", ctx.Err()))
	}

	return errors.Join(errs...)
}

func (dg *Diago) shutdownDialogs(ctx context.Context) []error {
	var errs []error
	if err := dg.cache.server.rangeDialogs(ctx, func(id string, d *DialogServerSession) bool {
		errs = append(errs, shutdownDialog(ctx, id, d)...)
		return true
	}); err != nil {
		errs = append(errs, err)
	}
	if err := dg.cache.client.rangeDialogs(ctx, func(id string, d *DialogClientSession) bool {
		errs = append(errs, shutdownDialog(ctx, id, d)...)
		return true
	}); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func shutdownDialog(ctx context.Context, id string, d DialogSession) []error {
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
// It is helper that calls
// - NewDialog
// - dialog.Invite
//
// Options allow full call customization, checkout SignalOption.
// Each SignalOption func is executed exactly once for the whole call setup.
// The final ACK is sent without options; call Ack directly if you need ACK headers.
// Scope: everything DialogClientSession.Invite accepts, plus
// WithDialogTransport and WithDialogTransportID.
//
// For better control more details use above functions instead.
// If you want to bridge call then use helper DialBridge
func (dg *Diago) Dial(ctx context.Context, recipient sip.Uri, opts ...SignalOption) (d *DialogClientSession, err error) {
	params, err := newSignalParams(opts, "Dial", scopeDial)
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

// DialBridge makes outgoing call leg and does bridging.
// Outgoing session will be added into bridge on answer
// If bridge is a *Bridge with Originator (first participant) it will be used
// for creating outgoing call leg as in B2BUA.
// When bridge is provided then this call will be bridged with any participant
// already present in bridge.
// Scope: same as Dial; for a *Bridge, Originator defaults to the bridge
// originator when not set. Options execute exactly once.
func (dg *Diago) DialBridge(ctx context.Context, recipient sip.Uri, bridge Bridger, opts ...SignalOption) (d *DialogClientSession, err error) {
	if bridge == nil {
		return nil, fmt.Errorf("DialBridge: bridge is nil")
	}
	params, err := newSignalParams(opts, "DialBridge", scopeDial)
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
		if b, ok := bridge.(*Bridge); ok && b.Originator != nil {
			params.Dialog.Originator = b.Originator
		}
	}

	if err := d.inviteWithParams(ctx, params); err != nil {
		return nil, errors.Join(err, d.Hangup(d.Context()), d.Close())
	}

	// Do bridging now
	if err := bridge.AddDialogSession(d); err != nil {
		return nil, errors.Join(err, d.Close())
	}

	if err := d.Ack(ctx); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return d, nil
}

// NewDialog creates a new client dialog session after you can perform dialog Invite
// - You call Invite(...) after this call followed with ACK
// Options allow selecting transport (WithDialogTransport, WithDialogTransportID)
// and overriding Contact.
// Scope: WithDialogTransport, WithDialogTransportID, WithContact.
// Pass the remaining options to the later Invite call; this method rejects them.
func (dg *Diago) NewDialog(recipient sip.Uri, opts ...SignalOption) (d *DialogClientSession, err error) {
	params, err := newSignalParams(opts, "NewDialog", scopeNewDialog)
	if err != nil {
		return nil, err
	}
	return dg.newDialogWithParams(recipient, params)
}

// newDialogWithParams creates a client dialog session from pre-computed params.
// It exists so helpers (Dial, DialBridge) can execute user-supplied
// SignalOption funcs a single time across dialog creation and Invite.
func (dg *Diago) newDialogWithParams(recipient sip.Uri, params *signalParams) (d *DialogClientSession, err error) {
	transport := ""
	transportID := ""
	var contact *sip.ContactHeader
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
		return nil, fmt.Errorf("transport %q does not exists", transport)
	}

	return dg.newSipDialog(recipient, tran, contact)
}

// mediaConfigForTransport builds the per-dialog media config by overlaying
// the transport media overrides on the global media config. The rule is
// "zero inherits global" per field; a BindHost that pins a concrete IP also
// pins the media bind IP unless overridden explicitly. A configured DTLS
// profile implies DTLS-SRTP unless an explicit SRTP mode was chosen.
func (dg *Diago) mediaConfigForTransport(tran *Transport) MediaConfig {
	conf := dg.mediaConf
	ov := tran.Media

	if ov.SRTP.Mode != SecureRTPNone {
		conf.SRTP.Mode = ov.SRTP.Mode
	}
	if ov.SRTP.Alg != 0 {
		conf.SRTP.Alg = ov.SRTP.Alg
	}
	if !dtlsConfIsZero(ov.SRTP.DTLS) {
		conf.SRTP.DTLS = ov.SRTP.DTLS
	}
	if ov.Network.BindIP != nil {
		conf.Network.BindIP = ov.Network.BindIP
	} else if bindIP := tran.bindIP; bindIP != nil && !bindIP.IsUnspecified() {
		conf.Network.BindIP = bindIP
	} else if bindIP := net.ParseIP(tran.BindHost); bindIP != nil && !bindIP.IsUnspecified() {
		// A Transport built as a literal (not through WithTransport) still
		// pins a concrete BindHost.
		conf.Network.BindIP = bindIP
	}
	if ov.Network.ExternalIP != nil {
		conf.Network.ExternalIP = ov.Network.ExternalIP
	}
	if ov.Network.RTPNAT != RTPNATDisabled {
		conf.Network.RTPNAT = ov.Network.RTPNAT
	}
	if ov.Network.RTPPortStart != 0 {
		conf.Network.RTPPortStart = ov.Network.RTPPortStart
	}
	if ov.Network.RTPPortEnd != 0 {
		conf.Network.RTPPortEnd = ov.Network.RTPPortEnd
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

func (dg *Diago) newSipDialog(recipient sip.Uri, tran *Transport, contact *sip.ContactHeader) (d *DialogClientSession, err error) {
	transport := tran.Transport

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

	d = &DialogClientSession{
		DialogClientSession: &sipgo.DialogClientSession{
			UA: &dialogUA,
			Dialog: sipgo.Dialog{
				InviteRequest: inviteReq,
			},
		},
	}
	d.Init()

	// Per-dialog media config: global media config with transport values
	// overlaid, so it carries all fields (codecs, RTPNAT, SRTP alg, port
	// range, codec order).
	d.mediaConfig = dg.mediaConfigForTransport(tran)
	d.dialogMedia.log = d.mediaConfig.Logger

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

func (dg *Diago) contactHDRFromTransport(tran *Transport, contact *sip.ContactHeader) {
	// Find contact hdr matching transport
	scheme := "sip"
	if tran.TLSConf != nil && !tran.TLSURINoSIPS {
		scheme = "sips"
	}

	contact.DisplayName = "" //TODO
	contact.Address = sip.Uri{
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
		contact.Address.UriParams.Add("transport", tran.Transport)
	}
}

func (dg *Diago) getClient(tran *Transport) *sipgo.Client {
	if dg.client != nil {
		// Use global one if exists
		return dg.client
	}

	return tran.client
}

func (dg *Diago) getTransport(transport string) (*Transport, bool) {
	if transport == "" {
		return &dg.transports[0], true
	}
	for _, t := range dg.transports {
		if sip.NetworkToLower(transport) == t.Transport {
			return &t, true
		}
	}
	return nil, false
}

func (dg *Diago) findTransport(transport string, id string) (*Transport, bool) {
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

// Register will create register transaction and keep registration ongoing until error is hit.
// For more granular control over registrations use RegisterTransaction.
// Options configure the transaction (WithRegisterExpiry,
// WithRegisterRetryInterval, WithRegisterProxyHost, WithRegisterAllowHeaders,
// WithOnRegistered) and every REGISTER attempt (WithAuthCredentials,
// WithContact, WithHeaders, WithRequestMutator).
// Scope: WithHeaders, WithContact, WithRequestMutator, WithAuthCredentials,
// WithRegisterExpiry, WithRegisterRetryInterval, WithRegisterAllowHeaders,
// WithRegisterProxyHost, WithOnRegistered.
func (dg *Diago) Register(ctx context.Context, recipient sip.Uri, opts ...SignalOption) error {
	t, err := dg.RegisterTransaction(ctx, recipient, opts...)
	if err != nil {
		return err
	}

	if err := t.Register(ctx); err != nil {
		return err
	}

	// Unregister
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := t.Unregister(ctx)
		if err != nil {
			dg.log.Error("Failed to unregister", "error", err)
			return
		}
		dg.log.Debug("Unregister successfull")
	}()

	for i := 0; ; i++ {
		dg.log.Debug("Qualify sent", "n", i)
		err := t.QualifyLoop(ctx)

		retryAfter := 5 * time.Second
		var rr *RegisterResponseError
		if errors.As(err, &rr) {
			resCode := rr.RegisterRes.StatusCode
			if !shouldRetryRegister(resCode) {
				return err
			}

			if h := rr.RegisterRes.GetHeader("Retry-After"); h != nil {
				ra, _ := strconv.Atoi(h.Value())
				if ra > 0 {
					retryAfter = time.Duration(ra) * time.Second
				}
			}
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// Retry pacing: explicit RetryInterval > Retry-After value > the
		// default computed from the negotiated expiry (see calcRetry).
		t.retryAfter = retryAfter
	}
}

// Register transaction creates register transaction object that can be used for Register Unregister requests
// Options configure the transaction (see Register) and every REGISTER
// attempt. Scope: WithHeaders, WithContact, WithRequestMutator,
// WithAuthCredentials, WithRegisterExpiry, WithRegisterRetryInterval,
// WithRegisterAllowHeaders, WithRegisterProxyHost, WithOnRegistered.
func (dg *Diago) RegisterTransaction(ctx context.Context, recipient sip.Uri, opts ...SignalOption) (*RegisterTransaction, error) {
	params, err := newSignalParams(opts, "RegisterTransaction", scopeRegisterTx)
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
		return nil, fmt.Errorf("transport=%q does not exists", transport)
	}

	contactHDR := sip.ContactHeader{}
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

func (dg *Diago) createClient(tran Transport) (client *sipgo.Client) {
	ua := dg.ua
	// When transport is not binding to specific IP

	opts := []sipgo.ClientOption{
		sipgo.WithClientNAT(),
		sipgo.WithClientLogger(dg.log),
	}

	// UDP is good to be reused
	// 1. Prefer resolved IP when sending out, as sending as unspecifed makes issue in receiving responses
	// 2. Use bind port if IP is specified. ISSUE: if ip is unspecified and SIP starts listen on 0.0.0.0:5060, SIPGO will try to use 192.168.1.1:5060 and therefore fail
	if tran.Transport == "udp" {
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

// MatchDialog resolves an in-dialog SIP request to the dialog tracked by
// this engine. It mirrors internal routing order: server (UAS) role via
// DialogIDFromRequestUAS first, then client (UAC) via DialogIDFromRequestUAC.
// ok is false for out-of-dialog, missing headers, or unknown dialogs. When ok,
// the returned DialogSession is the live *DialogServerSession or
// *DialogClientSession (type-assert if role matters).
func (dg *Diago) MatchDialog(req *sip.Request) (DialogSession, bool) {
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
