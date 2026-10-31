// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"fmt"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
)

// SignalOption is implemented by every option constructor. APIs take the
// interface matching their operation (for example AnswerOption for Answer), so
// an option that the operation does not consume fails to compile instead of
// being silently ignored.
type SignalOption interface {
	applySignal(*signalParams) error
}

// The per-operation option interfaces below define the valid option set of
// each API. They are implemented only by the With* constructors of this
// package (the marker methods are unexported), so compiling an API call is the
// option-scope check.
type (
	// ResponseOption configures Trying, Ringing, Progress and RespondSDP.
	ResponseOption interface {
		SignalOption
		isResponseOption()
	}
	// ProgressMediaOption configures ProgressMedia (183 with early media).
	ProgressMediaOption interface {
		SignalOption
		isProgressMediaOption()
	}
	// AnswerOption configures Answer and AnswerLate.
	AnswerOption interface {
		SignalOption
		isAnswerOption()
	}
	// CloseOption configures Close on both caller and callee dialogs.
	CloseOption interface {
		SignalOption
		isCloseOption()
	}
	// RequestOption configures in-dialog requests without media negotiation:
	// ReInvite and Unhold.
	RequestOption interface {
		SignalOption
		isRequestOption()
	}
	// HoldOption configures Hold (a ReInvite with optional hold music).
	HoldOption interface {
		SignalOption
		isHoldOption()
	}
	// AckOption configures the final ACK of an outbound call.
	AckOption interface {
		SignalOption
		isAckOption()
	}
	// ReferOption configures REFER on both caller and callee dialogs.
	ReferOption interface {
		SignalOption
		isReferOption()
	}
	// InviteOption configures the initial INVITE of an outbound call.
	InviteOption interface {
		SignalOption
		isInviteOption()
	}
	// DialOption configures Dial/DialURI and DialBridge/DialBridgeURI.
	DialOption interface {
		SignalOption
		isDialOption()
	}
	// NewCallOption configures NewCall (transport selection and Contact only).
	NewCallOption interface {
		SignalOption
		isNewCallOption()
	}
	// WaitAnswerOption configures WaitAnswer after early media detection.
	WaitAnswerOption interface {
		SignalOption
		isWaitAnswerOption()
	}
	// RegisterAttemptOption configures a single REGISTER attempt
	// (RegisterTransaction.Register/Unregister/Qualify).
	RegisterAttemptOption interface {
		SignalOption
		isRegisterAttemptOption()
	}
	// RegisterOption configures a REGISTER transaction
	// (Agent.Register/RegisterURI/RegisterTransaction).
	RegisterOption interface {
		SignalOption
		isRegisterOption()
	}
)

// signalParams carries per-call signaling customizations applied by the With*
// options. It is constructed by the library (newSignalParams); user code
// customizes it through the constructors. A nil *signalParams means "use
// defaults".
type signalParams struct {
	// Msg shapes the outgoing SIP message (request or response).
	Msg signalMsgParams
	// Media overrides the per-call media configuration.
	Media signalMediaParams
	// Dialog controls dialog establishment and lifecycle callbacks.
	Dialog signalDialogParams
	// Register configures REGISTER transactions. Register only.
	Register signalRegisterParams
}

// signalMsgParams shapes the outgoing SIP message.
type signalMsgParams struct {
	// Headers are appended to the outgoing SIP message (request or response).
	// Nil headers are skipped.
	Headers []Header

	// Contact replaces the default Contact header:
	// - on server responses (Trying/Ringing/ProgressMedia/Answer/AnswerLate/RespondSDP...)
	// - on client requests (Invite/ReInvite/Ack/Close...)
	// When set, the library default Contact header is NOT added.
	Contact *ContactHeader

	// Body overrides the outgoing body (usually a custom SDP).
	// When the outgoing message carries a body and no Content-Type header is
	// provided, "application/sdp" is added automatically.
	// Body is only emitted by methods that send a body (ProgressMedia, Answer,
	// AnswerLate, RespondSDP); Trying/Ringing reject it.
	Body []byte

	// MutateRequest is the last-chance hook invoked just before a request is sent.
	MutateRequest func(req *Request) error
	// MutateResponse is the last-chance hook invoked just before a response is sent.
	MutateResponse func(res *Response) error
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
	// Transport selects the transport by name ("udp", "tcp", ...). NewCall only.
	Transport string
	// TransportID selects the transport by its configured ID. NewCall only.
	TransportID string

	// Originator reuses SDP/codecs of another dialog to avoid transcoding. Invite only.
	Originator Call
	// Username/Password for digest authentication. Invite/Register only.
	Username string
	Password string
	// EarlyMediaDetect stops dialog establishment when 183 Session Progress
	// with SDP is received. ErrClientEarlyMedia is returned. Invite only.
	EarlyMediaDetect bool

	// OnResponse is invoked for responses during dialog establishment (client side).
	OnResponse func(res *Response) error
	// OnMediaUpdate is called on media updates (re-INVITE).
	OnMediaUpdate func(d *Media)
	// OnRefer is called on successful REFER handling.
	OnRefer OnReferFunc
	// OnReferNotify receives the transfer status (NOTIFY sipfrag code) of an
	// outgoing REFER sent with Refer. Refer only.
	OnReferNotify func(statusCode int)
}

// signalRegisterParams configures a REGISTER transaction. All fields are
// construction-time: they are read when the transaction is created with
// Agent.Register/RegisterTransaction and have no effect when passed to the
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
	// Detached keeps the registration out of Agent.Close (WithRegistrationDetached).
	Detached bool
}

// newSignalParams applies opts and returns the accumulated per-call
// configuration. Option validity is enforced by the per-operation option
// interfaces; errors here are invalid option values (ex. an empty name).
func newSignalParams[T SignalOption](opts ...T) (*signalParams, error) {
	p := &signalParams{}
	for _, o := range opts {
		if any(o) == nil {
			continue
		}
		if err := o.applySignal(p); err != nil {
			return nil, err
		}
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
	ReplaceHeader(header Header)
	Contact() *ContactHeader
}

// applySignalHeaders appends option headers to the message. Nil entries are
// skipped defensively; WithHeaders is expected to have rejected nil headers
// at option-construction time.
func applySignalHeaders(msg signalMessage, headers []Header) {
	for _, h := range headers {
		if h == nil {
			continue
		}
		msg.AppendHeader(h)
	}
}

// setSignalContact sets the Contact header keeping any existing one replaced.
func setSignalContact(msg signalMessage, contact *ContactHeader) {
	if msg.Contact() == nil {
		msg.AppendHeader(contact)
		return
	}
	msg.ReplaceHeader(contact)
}

// applyRequestSignal applies generic request options (headers + mutator).
// It must be called just before the request is written so the mutator sees
// the final message. sipgo fills mandatory dialog headers afterwards.
func applyRequestSignal(req *Request, params *signalParams) error {
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
func buildReInviteRequest(req *Request, baseSDP []byte, defaultContact *ContactHeader, params *signalParams) error {
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
func WithHeaders(headers ...Header) headersOption {
	return func(p *signalParams) error {
		for _, h := range headers {
			if h == nil {
				return fmt.Errorf("%w: WithHeaders: nil header provided", ErrInvalidOption)
			}
		}
		p.Msg.Headers = append(p.Msg.Headers, headers...)
		return nil
	}
}

// WithContact replaces the default Contact header of the outgoing message.
// On server side it applies to responses, on client side to requests.
func WithContact(contact *ContactHeader) contactOption {
	return func(p *signalParams) error {
		if contact == nil {
			return fmt.Errorf("%w: WithContact: contact header is nil", ErrInvalidOption)
		}
		p.Msg.Contact = contact
		return nil
	}
}

// WithBody overrides the outgoing body. Content-Type defaults to
// "application/sdp" unless provided within Headers.
func WithBody(body []byte) bodyOption {
	return func(p *signalParams) error {
		if body == nil {
			return fmt.Errorf("%w: WithBody: body is nil", ErrInvalidOption)
		}
		p.Msg.Body = body
		return nil
	}
}

// WithCodecs overrides the codecs offered in the SDP for this call.
func WithCodecs(codecs ...media.Codec) codecsOption {
	return func(p *signalParams) error {
		if len(codecs) == 0 {
			return fmt.Errorf("%w: WithCodecs: no codecs provided", ErrInvalidOption)
		}
		p.Media.Codecs = codecs
		return nil
	}
}

// WithRTPNAT sets the RTP NAT mode for this call
// (RTPNATDisabled or RTPNATLearnSource).
func WithRTPNAT(n RTPNATMode) rtpnatOption {
	return func(p *signalParams) error {
		if n != RTPNATDisabled && n != RTPNATLearnSource {
			return fmt.Errorf("%w: WithRTPNAT: invalid mode %d", ErrInvalidOption, n)
		}
		v := n
		p.Media.RTPNAT = &v
		return nil
	}
}

// WithMediaBindIP overrides the local RTP/RTCP bind IP for this call.
func WithMediaBindIP(ip net.IP) mediaBindIPOption {
	return func(p *signalParams) error {
		if ip == nil {
			return fmt.Errorf("%w: WithMediaBindIP: ip is nil", ErrInvalidOption)
		}
		p.Media.MediaBindIP = ip
		return nil
	}
}

// WithMediaExternalIP overrides the IP advertised in the SDP (c= line) for this call.
func WithMediaExternalIP(ip net.IP) mediaExternalIPOption {
	return func(p *signalParams) error {
		if ip == nil {
			return fmt.Errorf("%w: WithMediaExternalIP: ip is nil", ErrInvalidOption)
		}
		p.Media.MediaExternalIP = ip
		return nil
	}
}

// WithMediaDTLS overrides the DTLS configuration for this call. A non-empty
// configuration also enables DTLS-SRTP when the call is not using SRTP yet.
func WithMediaDTLS(conf media.DTLSConfig) mediaDTLSOption {
	return func(p *signalParams) error {
		if dtlsConfIsZero(conf) {
			return fmt.Errorf("%w: WithMediaDTLS: configuration is empty", ErrInvalidOption)
		}
		c := conf
		p.Media.MediaDTLSConf = &c
		return nil
	}
}

// WithSDPSessionName overrides the SDP "s=" session-name line for this call
// only. Overlays MediaConfig.SDP.SessionName; the empty string is rejected (it
// carries no information and is indistinguishable from "not set"), as are
// line breaks — an "s=" value must stay on one SDP line.
func WithSDPSessionName(name string) sdpSessionNameOption {
	return func(p *signalParams) error {
		if name == "" {
			return fmt.Errorf("%w: WithSDPSessionName: name is empty", ErrInvalidOption)
		}
		if err := media.ValidateSDPSessionName(name); err != nil {
			return fmt.Errorf("%w: WithSDPSessionName: %w", ErrInvalidOption, err)
		}
		p.Media.SDPSessionName = name
		return nil
	}
}

// WithMediaSession passes a fully custom/pre-created media session.
// The library will use it as is instead of creating its own session.
func WithMediaSession(m *media.MediaSession) mediaSessionOption {
	return func(p *signalParams) error {
		if m == nil {
			return fmt.Errorf("%w: WithMediaSession: media session is nil", ErrInvalidOption)
		}
		p.Media.MediaSession = m
		return nil
	}
}

// WithMusicOnHold sets the hold music played by Hold for this call, replacing
// the dialog-level MediaConfig.MusicOnHold. A zero tone (no segments)
// explicitly disables hold music for the call. On Invite/Answer it becomes
// the dialog-level default; on Hold it applies to that hold only.
func WithMusicOnHold(tone audio.Tone) musicOnHoldOption {
	return func(p *signalParams) error {
		t := tone
		p.Media.MusicOnHold = &t
		return nil
	}
}

// WithOnResponse sets a response callback used during dialog establishment (client side).
func WithOnResponse(fn func(res *Response) error) onResponseOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithOnResponse: fn is nil", ErrInvalidOption)
		}
		p.Dialog.OnResponse = fn
		return nil
	}
}

// WithOnMediaUpdate sets the media update callback (re-INVITE handling).
func WithOnMediaUpdate(fn func(d *Media)) onMediaUpdateOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithOnMediaUpdate: fn is nil", ErrInvalidOption)
		}
		p.Dialog.OnMediaUpdate = fn
		return nil
	}
}

// WithOnRefer sets the REFER handler callback.
func WithOnRefer(fn OnReferFunc) onReferOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithOnRefer: fn is nil", ErrInvalidOption)
		}
		p.Dialog.OnRefer = fn
		return nil
	}
}

// WithOnReferNotify sets the callback receiving the transfer status
// (NOTIFY sipfrag code) of an outgoing REFER sent with Refer.
// Refer only.
func WithOnReferNotify(fn func(statusCode int)) onReferNotifyOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithOnReferNotify: fn is nil", ErrInvalidOption)
		}
		p.Dialog.OnReferNotify = fn
		return nil
	}
}

// WithRequestMutator registers a last-chance hook invoked with the outgoing
// request just before it is sent. Use it for anything not covered by
// dedicated options.
func WithRequestMutator(fn func(req *Request) error) requestMutatorOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithRequestMutator: fn is nil", ErrInvalidOption)
		}
		p.Msg.MutateRequest = fn
		return nil
	}
}

// WithResponseMutator registers a last-chance hook invoked with the outgoing
// response just before it is sent. Use it for anything not covered by
// dedicated options.
func WithResponseMutator(fn func(res *Response) error) responseMutatorOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithResponseMutator: fn is nil", ErrInvalidOption)
		}
		p.Msg.MutateResponse = fn
		return nil
	}
}

// WithDialogTransport selects the transport used for a new dialog by name (udp, tcp, ...).
// NewCall only.
func WithDialogTransport(name string) dialogTransportOption {
	return func(p *signalParams) error {
		if name == "" {
			return fmt.Errorf("%w: WithDialogTransport: name is empty", ErrInvalidOption)
		}
		p.Dialog.Transport = name
		return nil
	}
}

// WithDialogTransportID selects the transport used for a new dialog by its configured ID.
// NewCall only.
func WithDialogTransportID(id string) dialogTransportIDOption {
	return func(p *signalParams) error {
		if id == "" {
			return fmt.Errorf("%w: WithDialogTransportID: id is empty", ErrInvalidOption)
		}
		p.Dialog.TransportID = id
		return nil
	}
}

// WithOriginator sets the originator dialog whose SDP/codecs are reused for the
// outgoing INVITE, avoiding media transcoding. Invite only.
func WithOriginator(o Call) originatorOption {
	return func(p *signalParams) error {
		if o == nil {
			return fmt.Errorf("%w: WithOriginator: originator is nil", ErrInvalidOption)
		}
		p.Dialog.Originator = o
		return nil
	}
}

// WithAuthCredentials sets username/password used for digest authentication.
// Invite/Register only. Both values empty is rejected.
func WithAuthCredentials(username string, password string) authCredentialsOption {
	return func(p *signalParams) error {
		if username == "" && password == "" {
			return fmt.Errorf("%w: WithAuthCredentials: username and password are empty", ErrInvalidOption)
		}
		p.Dialog.Username = username
		p.Dialog.Password = password
		return nil
	}
}

// WithEarlyMediaDetect enables early media detection on outgoing INVITE.
// Invite returns ErrClientEarlyMedia when 183 Session Progress with SDP is
// received. It is not a DialOption: Dial treats every Invite error as fatal.
// Use NewCall + Invite to handle early media.
func WithEarlyMediaDetect() earlyMediaDetectOption {
	return func(p *signalParams) error {
		p.Dialog.EarlyMediaDetect = true
		return nil
	}
}

// WithRegisterExpiry sets the Expires header value of the REGISTER requests
// of this transaction. Zero (default) sends no Expires header unless the
// server provides one. Register only.
func WithRegisterExpiry(d time.Duration) registerExpiryOption {
	return func(p *signalParams) error {
		if d < 0 {
			return fmt.Errorf("%w: WithRegisterExpiry: negative duration %s", ErrInvalidOption, d)
		}
		p.Register.Expiry = d
		return nil
	}
}

// WithRegisterRetryInterval sets a fixed interval before the next REGISTER
// is sent. Zero (default) derives it from the negotiated expiry.
// Register only.
func WithRegisterRetryInterval(d time.Duration) registerRetryIntervalOption {
	return func(p *signalParams) error {
		if d < 0 {
			return fmt.Errorf("%w: WithRegisterRetryInterval: negative duration %s", ErrInvalidOption, d)
		}
		p.Register.RetryInterval = d
		return nil
	}
}

// WithRegisterAllowHeaders sets the Allow header values sent with REGISTER.
// Register only.
func WithRegisterAllowHeaders(headers ...string) registerAllowHeadersOption {
	return func(p *signalParams) error {
		if len(headers) == 0 {
			return fmt.Errorf("%w: WithRegisterAllowHeaders: no headers provided", ErrInvalidOption)
		}
		for _, h := range headers {
			if h == "" {
				return fmt.Errorf("%w: WithRegisterAllowHeaders: empty header value", ErrInvalidOption)
			}
		}
		p.Register.AllowHeaders = headers
		return nil
	}
}

// WithRegisterProxyHost overrides the REGISTER request destination.
// Register only.
func WithRegisterProxyHost(host string) registerProxyHostOption {
	return func(p *signalParams) error {
		if host == "" {
			return fmt.Errorf("%w: WithRegisterProxyHost: host is empty", ErrInvalidOption)
		}
		p.Register.ProxyHost = host
		return nil
	}
}

// WithOnRegistered sets the callback invoked after a successful initial
// REGISTER. Register only.
func WithOnRegistered(fn func()) onRegisteredOption {
	return func(p *signalParams) error {
		if fn == nil {
			return fmt.Errorf("%w: WithOnRegistered: fn is nil", ErrInvalidOption)
		}
		p.Register.OnRegistered = fn
		return nil
	}
}

// WithRegistrationDetached keeps the registration created by Register or
// RegisterURI out of Agent.Close. The caller then owns the handle and stops it
// with Registration.Close (or by canceling its context). Scope: Register and
// RegisterURI; RegisterTransaction rejects it because it runs no loop.
func WithRegistrationDetached() registrationDetachedOption {
	return func(p *signalParams) error {
		p.Register.Detached = true
		return nil
	}
}

// Concrete option types. They are unexported on purpose: options are
// constructed with the With* functions and stored in the per-operation
// interfaces above (ex. []AnswerOption).

// headersOption is the concrete type returned by WithHeaders.
type headersOption func(*signalParams) error

func (o headersOption) applySignal(p *signalParams) error { return o(p) }
func (headersOption) isResponseOption()                   {}
func (headersOption) isProgressMediaOption()              {}
func (headersOption) isAnswerOption()                     {}
func (headersOption) isCloseOption()                      {}
func (headersOption) isRequestOption()                    {}
func (headersOption) isHoldOption()                       {}
func (headersOption) isAckOption()                        {}
func (headersOption) isReferOption()                      {}
func (headersOption) isInviteOption()                     {}
func (headersOption) isDialOption()                       {}
func (headersOption) isRegisterAttemptOption()            {}
func (headersOption) isRegisterOption()                   {}

// contactOption is the concrete type returned by WithContact.
type contactOption func(*signalParams) error

func (o contactOption) applySignal(p *signalParams) error { return o(p) }
func (contactOption) isResponseOption()                   {}
func (contactOption) isProgressMediaOption()              {}
func (contactOption) isAnswerOption()                     {}
func (contactOption) isCloseOption()                      {}
func (contactOption) isRequestOption()                    {}
func (contactOption) isHoldOption()                       {}
func (contactOption) isAckOption()                        {}
func (contactOption) isReferOption()                      {}
func (contactOption) isInviteOption()                     {}
func (contactOption) isDialOption()                       {}
func (contactOption) isRegisterAttemptOption()            {}
func (contactOption) isRegisterOption()                   {}
func (contactOption) isNewCallOption()                    {}

// bodyOption is the concrete type returned by WithBody.
type bodyOption func(*signalParams) error

func (o bodyOption) applySignal(p *signalParams) error { return o(p) }
func (bodyOption) isProgressMediaOption()              {}
func (bodyOption) isAnswerOption()                     {}
func (bodyOption) isRequestOption()                    {}
func (bodyOption) isHoldOption()                       {}
func (bodyOption) isInviteOption()                     {}
func (bodyOption) isDialOption()                       {}

// codecsOption is the concrete type returned by WithCodecs.
type codecsOption func(*signalParams) error

func (o codecsOption) applySignal(p *signalParams) error { return o(p) }
func (codecsOption) isProgressMediaOption()              {}
func (codecsOption) isAnswerOption()                     {}
func (codecsOption) isInviteOption()                     {}
func (codecsOption) isDialOption()                       {}

// rtpnatOption is the concrete type returned by WithRTPNAT.
type rtpnatOption func(*signalParams) error

func (o rtpnatOption) applySignal(p *signalParams) error { return o(p) }
func (rtpnatOption) isProgressMediaOption()              {}
func (rtpnatOption) isAnswerOption()                     {}
func (rtpnatOption) isInviteOption()                     {}
func (rtpnatOption) isDialOption()                       {}

// mediaBindIPOption is the concrete type returned by WithMediaBindIP.
type mediaBindIPOption func(*signalParams) error

func (o mediaBindIPOption) applySignal(p *signalParams) error { return o(p) }
func (mediaBindIPOption) isProgressMediaOption()              {}
func (mediaBindIPOption) isAnswerOption()                     {}
func (mediaBindIPOption) isInviteOption()                     {}
func (mediaBindIPOption) isDialOption()                       {}

// mediaExternalIPOption is the concrete type returned by WithMediaExternalIP.
type mediaExternalIPOption func(*signalParams) error

func (o mediaExternalIPOption) applySignal(p *signalParams) error { return o(p) }
func (mediaExternalIPOption) isProgressMediaOption()              {}
func (mediaExternalIPOption) isAnswerOption()                     {}
func (mediaExternalIPOption) isInviteOption()                     {}
func (mediaExternalIPOption) isDialOption()                       {}

// mediaDTLSOption is the concrete type returned by WithMediaDTLS.
type mediaDTLSOption func(*signalParams) error

func (o mediaDTLSOption) applySignal(p *signalParams) error { return o(p) }
func (mediaDTLSOption) isProgressMediaOption()              {}
func (mediaDTLSOption) isAnswerOption()                     {}
func (mediaDTLSOption) isInviteOption()                     {}
func (mediaDTLSOption) isDialOption()                       {}

// mediaSessionOption is the concrete type returned by WithMediaSession.
type mediaSessionOption func(*signalParams) error

func (o mediaSessionOption) applySignal(p *signalParams) error { return o(p) }
func (mediaSessionOption) isProgressMediaOption()              {}
func (mediaSessionOption) isAnswerOption()                     {}
func (mediaSessionOption) isInviteOption()                     {}
func (mediaSessionOption) isDialOption()                       {}

// sdpSessionNameOption is the concrete type returned by WithSDPSessionName.
type sdpSessionNameOption func(*signalParams) error

func (o sdpSessionNameOption) applySignal(p *signalParams) error { return o(p) }
func (sdpSessionNameOption) isProgressMediaOption()              {}
func (sdpSessionNameOption) isAnswerOption()                     {}
func (sdpSessionNameOption) isInviteOption()                     {}
func (sdpSessionNameOption) isDialOption()                       {}

// musicOnHoldOption is the concrete type returned by WithMusicOnHold.
type musicOnHoldOption func(*signalParams) error

func (o musicOnHoldOption) applySignal(p *signalParams) error { return o(p) }
func (musicOnHoldOption) isProgressMediaOption()              {}
func (musicOnHoldOption) isAnswerOption()                     {}
func (musicOnHoldOption) isHoldOption()                       {}
func (musicOnHoldOption) isInviteOption()                     {}
func (musicOnHoldOption) isDialOption()                       {}

// onResponseOption is the concrete type returned by WithOnResponse.
type onResponseOption func(*signalParams) error

func (o onResponseOption) applySignal(p *signalParams) error { return o(p) }
func (onResponseOption) isInviteOption()                     {}
func (onResponseOption) isDialOption()                       {}
func (onResponseOption) isWaitAnswerOption()                 {}

// onMediaUpdateOption is the concrete type returned by WithOnMediaUpdate.
type onMediaUpdateOption func(*signalParams) error

func (o onMediaUpdateOption) applySignal(p *signalParams) error { return o(p) }
func (onMediaUpdateOption) isAnswerOption()                     {}
func (onMediaUpdateOption) isInviteOption()                     {}
func (onMediaUpdateOption) isDialOption()                       {}

// onReferOption is the concrete type returned by WithOnRefer.
type onReferOption func(*signalParams) error

func (o onReferOption) applySignal(p *signalParams) error { return o(p) }
func (onReferOption) isAnswerOption()                     {}
func (onReferOption) isInviteOption()                     {}
func (onReferOption) isDialOption()                       {}

// onReferNotifyOption is the concrete type returned by WithOnReferNotify.
type onReferNotifyOption func(*signalParams) error

func (o onReferNotifyOption) applySignal(p *signalParams) error { return o(p) }
func (onReferNotifyOption) isReferOption()                      {}

// requestMutatorOption is the concrete type returned by WithRequestMutator.
type requestMutatorOption func(*signalParams) error

func (o requestMutatorOption) applySignal(p *signalParams) error { return o(p) }
func (requestMutatorOption) isCloseOption()                      {}
func (requestMutatorOption) isRequestOption()                    {}
func (requestMutatorOption) isHoldOption()                       {}
func (requestMutatorOption) isAckOption()                        {}
func (requestMutatorOption) isReferOption()                      {}
func (requestMutatorOption) isInviteOption()                     {}
func (requestMutatorOption) isDialOption()                       {}
func (requestMutatorOption) isRegisterAttemptOption()            {}
func (requestMutatorOption) isRegisterOption()                   {}

// responseMutatorOption is the concrete type returned by WithResponseMutator.
type responseMutatorOption func(*signalParams) error

func (o responseMutatorOption) applySignal(p *signalParams) error { return o(p) }
func (responseMutatorOption) isResponseOption()                   {}
func (responseMutatorOption) isProgressMediaOption()              {}
func (responseMutatorOption) isAnswerOption()                     {}

// dialogTransportOption is the concrete type returned by WithDialogTransport.
type dialogTransportOption func(*signalParams) error

func (o dialogTransportOption) applySignal(p *signalParams) error { return o(p) }
func (dialogTransportOption) isDialOption()                       {}
func (dialogTransportOption) isNewCallOption()                    {}

// dialogTransportIDOption is the concrete type returned by WithDialogTransportID.
type dialogTransportIDOption func(*signalParams) error

func (o dialogTransportIDOption) applySignal(p *signalParams) error { return o(p) }
func (dialogTransportIDOption) isDialOption()                       {}
func (dialogTransportIDOption) isNewCallOption()                    {}

// originatorOption is the concrete type returned by WithOriginator.
type originatorOption func(*signalParams) error

func (o originatorOption) applySignal(p *signalParams) error { return o(p) }
func (originatorOption) isInviteOption()                     {}
func (originatorOption) isDialOption()                       {}

// authCredentialsOption is the concrete type returned by WithAuthCredentials.
type authCredentialsOption func(*signalParams) error

func (o authCredentialsOption) applySignal(p *signalParams) error { return o(p) }
func (authCredentialsOption) isInviteOption()                     {}
func (authCredentialsOption) isDialOption()                       {}
func (authCredentialsOption) isWaitAnswerOption()                 {}
func (authCredentialsOption) isRegisterAttemptOption()            {}
func (authCredentialsOption) isRegisterOption()                   {}

// earlyMediaDetectOption is the concrete type returned by WithEarlyMediaDetect.
type earlyMediaDetectOption func(*signalParams) error

func (o earlyMediaDetectOption) applySignal(p *signalParams) error { return o(p) }
func (earlyMediaDetectOption) isInviteOption()                     {}

// registerExpiryOption is the concrete type returned by WithRegisterExpiry.
type registerExpiryOption func(*signalParams) error

func (o registerExpiryOption) applySignal(p *signalParams) error { return o(p) }
func (registerExpiryOption) isRegisterOption()                   {}

// registerRetryIntervalOption is the concrete type returned by WithRegisterRetryInterval.
type registerRetryIntervalOption func(*signalParams) error

func (o registerRetryIntervalOption) applySignal(p *signalParams) error { return o(p) }
func (registerRetryIntervalOption) isRegisterOption()                   {}

// registerAllowHeadersOption is the concrete type returned by WithRegisterAllowHeaders.
type registerAllowHeadersOption func(*signalParams) error

func (o registerAllowHeadersOption) applySignal(p *signalParams) error { return o(p) }
func (registerAllowHeadersOption) isRegisterOption()                   {}

// registerProxyHostOption is the concrete type returned by WithRegisterProxyHost.
type registerProxyHostOption func(*signalParams) error

func (o registerProxyHostOption) applySignal(p *signalParams) error { return o(p) }
func (registerProxyHostOption) isRegisterOption()                   {}

// registrationDetachedOption is the concrete type returned by WithRegistrationDetached.
type registrationDetachedOption func(*signalParams) error

func (o registrationDetachedOption) applySignal(p *signalParams) error { return o(p) }
func (registrationDetachedOption) isRegisterOption()                   {}

// onRegisteredOption is the concrete type returned by WithOnRegistered.
type onRegisteredOption func(*signalParams) error

func (o onRegisteredOption) applySignal(p *signalParams) error { return o(p) }
func (onRegisteredOption) isRegisterOption()                   {}
