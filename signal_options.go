// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
)

// optionScope identifies one configurable capability. Every With* constructor
// marks the bit it touches; each public API declares the bits it consumes and
// newSignalParams rejects the rest with UnsupportedOptionError.
type optionScope uint32

const (
	scopeHeaders optionScope = 1 << iota
	scopeContact
	scopeBody
	scopeMutateRequest
	scopeMutateResponse
	scopeCodecs
	scopeRTPNAT
	scopeMediaBindIP
	scopeMediaExternalIP
	scopeMediaDTLS
	scopeMediaSession
	scopeSDPSessionName
	scopeMusicOnHold
	scopeTransport
	scopeTransportID
	scopeOriginator
	scopeCredentials
	scopeEarlyMediaDetect
	scopeOnResponse
	scopeOnMediaUpdate
	scopeOnRefer
	scopeOnReferNotify
	scopeRegisterExpiry
	scopeRegisterRetryInterval
	scopeRegisterAllowHeaders
	scopeRegisterProxyHost
	scopeOnRegistered
)

// Common unions of scope bits.
const (
	// scopeMsgRequest shapes outgoing requests.
	scopeMsgRequest = scopeHeaders | scopeContact | scopeBody | scopeMutateRequest
	// scopeMsgRequestNoBody shapes requests that never carry an SDP body
	// (Ack/Hangup/Refer/REGISTER attempts).
	scopeMsgRequestNoBody = scopeHeaders | scopeContact | scopeMutateRequest
	// scopeMsgResponse shapes outgoing responses.
	scopeMsgResponse = scopeHeaders | scopeContact | scopeMutateResponse
	// scopeMediaAll are the options consumed through signalMediaConfig.
	scopeMediaAll = scopeCodecs | scopeRTPNAT | scopeMediaBindIP | scopeMediaExternalIP |
		scopeMediaDTLS | scopeMediaSession | scopeSDPSessionName | scopeMusicOnHold
	// scopeRegisterAll is the construction-time REGISTER configuration.
	scopeRegisterAll = scopeRegisterExpiry | scopeRegisterRetryInterval |
		scopeRegisterAllowHeaders | scopeRegisterProxyHost | scopeOnRegistered

	// Per-API allowed sets. Keep in sync with the "Scope:" lines in godoc.
	scopeTrying        = scopeMsgResponse
	scopeProgress      = scopeMsgResponse
	scopeProgressMedia = scopeMsgResponse | scopeBody | scopeMediaAll
	scopeRinging       = scopeMsgResponse
	scopeRespondSDP    = scopeMsgResponse
	scopeAnswer        = scopeProgressMedia | scopeOnMediaUpdate | scopeOnRefer
	scopeAnswerLate    = scopeProgressMedia
	scopeServerHangup  = scopeMsgResponse | scopeMutateRequest
	scopeReInvite      = scopeMsgRequest
	scopeHold          = scopeMsgRequest | scopeMusicOnHold
	scopeUnhold        = scopeMsgRequest
	scopeClientInvite  = scopeMsgRequest | scopeMediaAll | scopeOriginator | scopeCredentials |
		scopeEarlyMediaDetect | scopeOnResponse | scopeOnMediaUpdate | scopeOnRefer
	scopeDial            = scopeClientInvite | scopeTransport | scopeTransportID
	scopeNewDialog       = scopeTransport | scopeTransportID | scopeContact
	scopeClientHangup    = scopeMsgRequestNoBody
	scopeClientAck       = scopeMsgRequestNoBody
	scopeRefer           = scopeMsgRequestNoBody | scopeOnReferNotify
	scopeWaitAnswer      = scopeCredentials | scopeOnResponse
	scopeRegisterAttempt = scopeMsgRequestNoBody | scopeCredentials
	scopeRegisterTx      = scopeRegisterAttempt | scopeRegisterAll
)

// touchedOption records one applied option for scope validation.
type touchedOption struct {
	name  string
	scope optionScope
}

// UnsupportedOptionError reports SignalOptions that the called API does not
// consume. v1 silently ignored them; v2 makes the misuse explicit.
type UnsupportedOptionError struct {
	// Method is the public API the options were passed to (ex. "Trying").
	Method string
	// Options are the constructor names that are not supported (ex. "WithBody").
	Options []string
}

func (e *UnsupportedOptionError) Error() string {
	if len(e.Options) == 1 {
		return fmt.Sprintf("%s: unsupported option %s", e.Method, e.Options[0])
	}
	return fmt.Sprintf("%s: unsupported options %s", e.Method, strings.Join(e.Options, ", "))
}

// signalParams carries per-call signaling customizations applied by
// SignalOption. It is constructed by the library (newSignalParams); user code
// customizes it through the With* constructors. A nil *signalParams means
// "use defaults".
type signalParams struct {
	// Msg shapes the outgoing SIP message (request or response).
	Msg signalMsgParams
	// Media overrides the per-call media configuration.
	Media signalMediaParams
	// Dialog controls dialog establishment and lifecycle callbacks.
	Dialog signalDialogParams
	// Register configures REGISTER transactions. Register only.
	Register signalRegisterParams

	// touched records every applied option for scope validation.
	touched []touchedOption
}

// mark records that an option touched scope. It is called by every With*
// constructor.
func (p *signalParams) mark(name string, scope optionScope) {
	p.touched = append(p.touched, touchedOption{name: name, scope: scope})
}

// signalMsgParams shapes the outgoing SIP message.
type signalMsgParams struct {
	// Headers are appended to the outgoing SIP message (request or response).
	// Nil headers are skipped.
	Headers []sip.Header

	// Contact replaces the default Contact header:
	// - on server responses (Trying/Ringing/ProgressMedia/Answer/AnswerLate/RespondSDP...)
	// - on client requests (Invite/ReInvite/Ack/Hangup...)
	// When set, the library default Contact header is NOT added.
	Contact *sip.ContactHeader

	// Body overrides the outgoing body (usually a custom SDP).
	// When the outgoing message carries a body and no Content-Type header is
	// provided, "application/sdp" is added automatically.
	// Body is only emitted by methods that send a body (ProgressMedia, Answer,
	// AnswerLate, RespondSDP); Trying/Ringing reject it.
	Body []byte

	// MutateRequest is the last-chance hook invoked just before a request is sent.
	MutateRequest func(req *sip.Request) error
	// MutateResponse is the last-chance hook invoked just before a response is sent.
	MutateResponse func(res *sip.Response) error
}

// signalMediaParams overrides the per-call media configuration on top of the
// dialog media config. Precedence: MediaSession > granular options > dialog
// defaults. Granular fields are ignored when MediaSession is set, since the
// caller takes full ownership of the session.
type signalMediaParams struct {
	Codecs          []media.Codec
	RTPNAT          *RTPNATMode
	MediaBindIP     net.IP
	MediaExternalIP net.IP
	MediaDTLSConf   *media.DTLSConfig
	// SDPSessionName overrides the SDP "s=" line for this call only. Empty
	// means "no per-call change".
	SDPSessionName string

	// MusicOnHold overrides the hold music consumed by Hold/Unhold. Non-nil
	// is an explicit per-call choice: a tone with segments replaces the
	// dialog-level default, a zero tone (no segments) disables music for the
	// call. Nil means "no per-call change". On Invite/Answer it is persisted
	// as the dialog-level default (via signalMediaConfig); on Hold it applies
	// to that hold only.
	MusicOnHold *audio.Tone

	// MediaSession allows passing a fully custom/pre-created media session.
	// When set the library skips its own media session creation and uses this one.
	MediaSession *media.MediaSession
}

// signalDialogParams controls dialog establishment and lifecycle callbacks.
type signalDialogParams struct {
	// Transport selects the transport by name ("udp", "tcp", ...). NewDialog only.
	Transport string
	// TransportID selects the transport by its configured ID. NewDialog only.
	TransportID string

	// Originator reuses SDP/codecs of another dialog to avoid transcoding. Invite only.
	Originator DialogSession
	// Username/Password for digest authentication. Invite/Register only.
	Username string
	Password string
	// EarlyMediaDetect stops dialog establishment when 183 Session Progress
	// with SDP is received. ErrClientEarlyMedia is returned. Invite only.
	EarlyMediaDetect bool

	// OnResponse is invoked for responses during dialog establishment (client side).
	OnResponse func(res *sip.Response) error
	// OnMediaUpdate is called on media updates (re-INVITE).
	OnMediaUpdate func(d *DialogMedia)
	// OnRefer is called on successful REFER handling.
	OnRefer OnReferDialogFunc
	// OnReferNotify receives the transfer status (NOTIFY sipfrag code) of an
	// outgoing REFER sent with Refer. Refer only.
	OnReferNotify func(statusCode int)
}

// signalRegisterParams configures a REGISTER transaction. All fields are
// construction-time: they are read when the transaction is created with
// Diago.Register/RegisterTransaction and have no effect when passed to the
// per-request methods (Register/Unregister/Qualify). Register only.
type signalRegisterParams struct {
	// Expiry is for Expire header
	Expiry time.Duration
	// RetryInterval is interval before next Register is sent
	RetryInterval time.Duration
	// AllowHeaders lists the Allow header values sent with REGISTER
	AllowHeaders []string
	// ProxyHost overrides the REGISTER request destination
	ProxyHost string

	// OnRegistered is called after a successful initial REGISTER
	OnRegistered func()
}

// SignalOption configures per-call signaling behavior of diago APIs.
// It is accepted by all signaling methods (server, client, NewDialog and
// REGISTER). Construct options with the With* functions; an option that the
// called method does not consume is rejected with *UnsupportedOptionError
// instead of being silently ignored.
type SignalOption func(*signalParams) error

// newSignalParams applies all options and verifies that every applied option
// is consumed by method (allowed scope). It always returns a non-nil params
// struct on success so callers can safely dereference fields.
func newSignalParams(opts []SignalOption, method string, allowed optionScope) (*signalParams, error) {
	p := &signalParams{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(p); err != nil {
			return nil, err
		}
	}

	var unsupported []string
	for _, t := range p.touched {
		if t.scope&^allowed == 0 {
			continue
		}
		if !slices.Contains(unsupported, t.name) {
			unsupported = append(unsupported, t.name)
		}
	}
	if len(unsupported) > 0 {
		return nil, &UnsupportedOptionError{Method: method, Options: unsupported}
	}
	return p, nil
}

// signalMediaConfig overlays per-call media options on top of the base config.
func signalMediaConfig(base MediaConfig, p *signalParams) MediaConfig {
	conf := base
	if p == nil {
		return conf
	}
	if p.Media.Codecs != nil {
		conf.Codecs = p.Media.Codecs
	}
	if p.Media.RTPNAT != nil {
		conf.Network.RTPNAT = *p.Media.RTPNAT
	}
	if p.Media.MediaBindIP != nil {
		conf.Network.BindIP = p.Media.MediaBindIP
	}
	if p.Media.MediaExternalIP != nil {
		conf.Network.ExternalIP = p.Media.MediaExternalIP
	}
	if p.Media.MediaDTLSConf != nil {
		conf.SRTP.DTLS = *p.Media.MediaDTLSConf
	}
	if p.Media.SDPSessionName != "" {
		conf.SDP.SessionName = p.Media.SDPSessionName
	}
	if p.Media.MusicOnHold != nil {
		conf.MusicOnHold = *p.Media.MusicOnHold
	}
	return conf
}

// signalMessage is the subset of sip messages needed for applying options.
type signalMessage interface {
	sip.Message
	ReplaceHeader(header sip.Header)
	Contact() *sip.ContactHeader
}

// applySignalHeaders appends option headers to the message. Nil entries are
// skipped defensively; WithHeaders is expected to have rejected nil headers
// at option-construction time.
func applySignalHeaders(msg signalMessage, headers []sip.Header) {
	for _, h := range headers {
		if h == nil {
			continue
		}
		msg.AppendHeader(h)
	}
}

// setSignalContact sets the Contact header keeping any existing one replaced.
func setSignalContact(msg signalMessage, contact *sip.ContactHeader) {
	if msg.Contact() == nil {
		msg.AppendHeader(contact)
		return
	}
	msg.ReplaceHeader(contact)
}

// applyRequestSignal applies generic request options (headers + mutator).
// It must be called just before the request is written so the mutator sees
// the final message. sipgo fills mandatory dialog headers afterwards.
func applyRequestSignal(req *sip.Request, params *signalParams) error {
	if params == nil {
		return nil
	}
	if params.Msg.Contact != nil {
		setSignalContact(req, params.Msg.Contact)
	}
	applySignalHeaders(req, params.Msg.Headers)
	if params.Msg.MutateRequest != nil {
		return params.Msg.MutateRequest(req)
	}
	return nil
}

// buildReInviteRequest fills a re-INVITE request from base SDP body and
// signalParams: it applies default Contact (if non-nil), lets params.Msg.Contact
// override, swaps the body when WithBody is set, sets Content-Type, and runs
// the request mutator. Caller is responsible for sending the request.
func buildReInviteRequest(req *sip.Request, baseSDP []byte, defaultContact *sip.ContactHeader, params *signalParams) error {
	body := baseSDP
	if params != nil && params.Msg.Body != nil {
		body = params.Msg.Body
	}
	if defaultContact != nil && (params == nil || params.Msg.Contact == nil) {
		req.AppendHeader(defaultContact)
	}
	if params != nil && params.Msg.Contact != nil {
		setSignalContact(req, params.Msg.Contact)
	}
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(body)
	return applyRequestSignal(req, params)
}

// WithHeaders appends custom headers to the outgoing SIP message.
func WithHeaders(headers ...sip.Header) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithHeaders", scopeHeaders)
		for _, h := range headers {
			if h == nil {
				return fmt.Errorf("WithHeaders: nil header provided")
			}
		}
		p.Msg.Headers = append(p.Msg.Headers, headers...)
		return nil
	}
}

// WithHeader is a convenience wrapper appending a single header by name and value.
func WithHeader(name string, value string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithHeader", scopeHeaders)
		if name == "" {
			return fmt.Errorf("WithHeader: name is empty")
		}
		p.Msg.Headers = append(p.Msg.Headers, sip.NewHeader(name, value))
		return nil
	}
}

// WithContact replaces the default Contact header of the outgoing message.
// On server side it applies to responses, on client side to requests.
func WithContact(contact *sip.ContactHeader) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithContact", scopeContact)
		if contact == nil {
			return fmt.Errorf("WithContact: contact header is nil")
		}
		p.Msg.Contact = contact
		return nil
	}
}

// WithBody overrides the outgoing body. Content-Type defaults to
// "application/sdp" unless provided within Headers.
func WithBody(body []byte) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithBody", scopeBody)
		if body == nil {
			return fmt.Errorf("WithBody: body is nil")
		}
		p.Msg.Body = body
		return nil
	}
}

// WithCodecs overrides the codecs offered in the SDP for this call.
func WithCodecs(codecs ...media.Codec) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithCodecs", scopeCodecs)
		if len(codecs) == 0 {
			return fmt.Errorf("WithCodecs: no codecs provided")
		}
		p.Media.Codecs = codecs
		return nil
	}
}

// WithRTPNAT sets the RTP NAT mode for this call
// (RTPNATDisabled or RTPNATLearnSource).
func WithRTPNAT(n RTPNATMode) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithRTPNAT", scopeRTPNAT)
		if n != RTPNATDisabled && n != RTPNATLearnSource {
			return fmt.Errorf("WithRTPNAT: invalid mode %d", n)
		}
		v := n
		p.Media.RTPNAT = &v
		return nil
	}
}

// WithMediaBindIP overrides the local RTP/RTCP bind IP for this call.
func WithMediaBindIP(ip net.IP) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithMediaBindIP", scopeMediaBindIP)
		if ip == nil {
			return fmt.Errorf("WithMediaBindIP: ip is nil")
		}
		p.Media.MediaBindIP = ip
		return nil
	}
}

// WithMediaExternalIP overrides the IP advertised in the SDP (c= line) for this call.
func WithMediaExternalIP(ip net.IP) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithMediaExternalIP", scopeMediaExternalIP)
		if ip == nil {
			return fmt.Errorf("WithMediaExternalIP: ip is nil")
		}
		p.Media.MediaExternalIP = ip
		return nil
	}
}

// WithMediaDTLS overrides the DTLS configuration for this call. A non-empty
// configuration also enables DTLS-SRTP when the call is not using SRTP yet.
func WithMediaDTLS(conf media.DTLSConfig) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithMediaDTLS", scopeMediaDTLS)
		if dtlsConfIsZero(conf) {
			return fmt.Errorf("WithMediaDTLS: configuration is empty")
		}
		c := conf
		p.Media.MediaDTLSConf = &c
		return nil
	}
}

// WithSDPSessionName overrides the SDP "s=" session-name line for this call
// only. Overlays MediaConfig.SDPSessionName; the empty string is rejected (it
// carries no information and is indistinguishable from "not set"), as are
// line breaks — an "s=" value must stay on one SDP line.
func WithSDPSessionName(name string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithSDPSessionName", scopeSDPSessionName)
		if name == "" {
			return fmt.Errorf("WithSDPSessionName: name is empty")
		}
		if err := media.ValidateSDPSessionName(name); err != nil {
			return fmt.Errorf("WithSDPSessionName: %w", err)
		}
		p.Media.SDPSessionName = name
		return nil
	}
}

// WithMediaSession passes a fully custom/pre-created media session.
// The library will use it as is instead of creating its own session.
func WithMediaSession(m *media.MediaSession) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithMediaSession", scopeMediaSession)
		if m == nil {
			return fmt.Errorf("WithMediaSession: media session is nil")
		}
		p.Media.MediaSession = m
		return nil
	}
}

// WithMusicOnHold sets the hold music played by Hold for this call, replacing
// the dialog-level MediaConfig.MusicOnHold. A zero tone (no segments)
// explicitly disables hold music for the call. On Invite/Answer it becomes
// the dialog-level default; on Hold it applies to that hold only.
func WithMusicOnHold(tone audio.Tone) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithMusicOnHold", scopeMusicOnHold)
		t := tone
		p.Media.MusicOnHold = &t
		return nil
	}
}

// WithOnResponse sets a response callback used during dialog establishment (client side).
func WithOnResponse(fn func(res *sip.Response) error) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithOnResponse", scopeOnResponse)
		if fn == nil {
			return fmt.Errorf("WithOnResponse: fn is nil")
		}
		p.Dialog.OnResponse = fn
		return nil
	}
}

// WithOnMediaUpdate sets the media update callback (re-INVITE handling).
func WithOnMediaUpdate(fn func(d *DialogMedia)) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithOnMediaUpdate", scopeOnMediaUpdate)
		if fn == nil {
			return fmt.Errorf("WithOnMediaUpdate: fn is nil")
		}
		p.Dialog.OnMediaUpdate = fn
		return nil
	}
}

// WithOnRefer sets the REFER handler callback.
func WithOnRefer(fn OnReferDialogFunc) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithOnRefer", scopeOnRefer)
		if fn == nil {
			return fmt.Errorf("WithOnRefer: fn is nil")
		}
		p.Dialog.OnRefer = fn
		return nil
	}
}

// WithOnReferNotify sets the callback receiving the transfer status
// (NOTIFY sipfrag code) of an outgoing REFER sent with Refer.
// Refer only.
func WithOnReferNotify(fn func(statusCode int)) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithOnReferNotify", scopeOnReferNotify)
		if fn == nil {
			return fmt.Errorf("WithOnReferNotify: fn is nil")
		}
		p.Dialog.OnReferNotify = fn
		return nil
	}
}

// WithRequestMutator registers a last-chance hook invoked with the outgoing
// request just before it is sent. Use it for anything not covered by
// dedicated options.
func WithRequestMutator(fn func(req *sip.Request) error) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithRequestMutator", scopeMutateRequest)
		if fn == nil {
			return fmt.Errorf("WithRequestMutator: fn is nil")
		}
		p.Msg.MutateRequest = fn
		return nil
	}
}

// WithResponseMutator registers a last-chance hook invoked with the outgoing
// response just before it is sent. Use it for anything not covered by
// dedicated options.
func WithResponseMutator(fn func(res *sip.Response) error) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithResponseMutator", scopeMutateResponse)
		if fn == nil {
			return fmt.Errorf("WithResponseMutator: fn is nil")
		}
		p.Msg.MutateResponse = fn
		return nil
	}
}

// WithDialogTransport selects the transport used for a new dialog by name (udp, tcp, ...).
// NewDialog only.
func WithDialogTransport(name string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithDialogTransport", scopeTransport)
		if name == "" {
			return fmt.Errorf("WithDialogTransport: name is empty")
		}
		p.Dialog.Transport = name
		return nil
	}
}

// WithDialogTransportID selects the transport used for a new dialog by its configured ID.
// NewDialog only.
func WithDialogTransportID(id string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithDialogTransportID", scopeTransportID)
		if id == "" {
			return fmt.Errorf("WithDialogTransportID: id is empty")
		}
		p.Dialog.TransportID = id
		return nil
	}
}

// WithOriginator sets the originator dialog whose SDP/codecs are reused for the
// outgoing INVITE, avoiding media transcoding. Invite only.
func WithOriginator(o DialogSession) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithOriginator", scopeOriginator)
		if o == nil {
			return fmt.Errorf("WithOriginator: originator is nil")
		}
		p.Dialog.Originator = o
		return nil
	}
}

// WithAuthCredentials sets username/password used for digest authentication.
// Invite/Register only. Both values empty is rejected.
func WithAuthCredentials(username string, password string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithAuthCredentials", scopeCredentials)
		if username == "" && password == "" {
			return fmt.Errorf("WithAuthCredentials: username and password are empty")
		}
		p.Dialog.Username = username
		p.Dialog.Password = password
		return nil
	}
}

// WithEarlyMediaDetect enables early media detection on outgoing INVITE.
// Invite returns ErrClientEarlyMedia when 183 Session Progress with SDP is received.
// Invite only.
func WithEarlyMediaDetect() SignalOption {
	return func(p *signalParams) error {
		p.mark("WithEarlyMediaDetect", scopeEarlyMediaDetect)
		p.Dialog.EarlyMediaDetect = true
		return nil
	}
}

// WithRegisterExpiry sets the Expires header value of the REGISTER requests
// of this transaction. Zero (default) sends no Expires header unless the
// server provides one. Register only.
func WithRegisterExpiry(d time.Duration) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithRegisterExpiry", scopeRegisterExpiry)
		if d < 0 {
			return fmt.Errorf("WithRegisterExpiry: negative duration %s", d)
		}
		p.Register.Expiry = d
		return nil
	}
}

// WithRegisterRetryInterval sets a fixed interval before the next REGISTER
// is sent. Zero (default) derives it from the negotiated expiry.
// Register only.
func WithRegisterRetryInterval(d time.Duration) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithRegisterRetryInterval", scopeRegisterRetryInterval)
		if d < 0 {
			return fmt.Errorf("WithRegisterRetryInterval: negative duration %s", d)
		}
		p.Register.RetryInterval = d
		return nil
	}
}

// WithRegisterAllowHeaders sets the Allow header values sent with REGISTER.
// Register only.
func WithRegisterAllowHeaders(headers ...string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithRegisterAllowHeaders", scopeRegisterAllowHeaders)
		if len(headers) == 0 {
			return fmt.Errorf("WithRegisterAllowHeaders: no headers provided")
		}
		for _, h := range headers {
			if h == "" {
				return fmt.Errorf("WithRegisterAllowHeaders: empty header value")
			}
		}
		p.Register.AllowHeaders = headers
		return nil
	}
}

// WithRegisterProxyHost overrides the REGISTER request destination.
// Register only.
func WithRegisterProxyHost(host string) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithRegisterProxyHost", scopeRegisterProxyHost)
		if host == "" {
			return fmt.Errorf("WithRegisterProxyHost: host is empty")
		}
		p.Register.ProxyHost = host
		return nil
	}
}

// WithOnRegistered sets the callback invoked after a successful initial
// REGISTER. Register only.
func WithOnRegistered(fn func()) SignalOption {
	return func(p *signalParams) error {
		p.mark("WithOnRegistered", scopeOnRegistered)
		if fn == nil {
			return fmt.Errorf("WithOnRegistered: fn is nil")
		}
		p.Register.OnRegistered = fn
		return nil
	}
}
