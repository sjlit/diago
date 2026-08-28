// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"fmt"
	"net"

	"github.com/sjlit/diago/media"
	"github.com/emiago/sipgo/sip"
)

// SignalParams carries per-call signaling customizations applied by SignalOption.
// A nil *SignalParams means "use defaults". Not all fields apply to every API;
// fields that are irrelevant to the called method are ignored.
type SignalParams struct {
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
	// AnswerLate, RespondSDP); Trying/Ringing always ignore it.
	Body []byte

	// Media overrides applied on top of the dialog media config.
	// Precedence: MediaSession > granular options > dialog defaults.
	// Granular fields (Codecs, RTPNAT, MediaBindIP, MediaExternalIP, MediaDTLSConf)
	// are silently ignored when MediaSession is set, since the caller takes full
	// ownership of the session.
	Codecs          []media.Codec
	RTPNAT          *int
	MediaBindIP     net.IP
	MediaExternalIP net.IP
	MediaDTLSConf   *media.DTLSConfig

	// MediaSession allows passing a fully custom/pre-created media session.
	// When set the library skips its own media session creation and uses this one.
	MediaSession *media.MediaSession

	// OnResponse is invoked for responses during dialog establishment (client side).
	OnResponse func(res *sip.Response) error
	// OnMediaUpdate is called on media updates (re-INVITE).
	OnMediaUpdate func(d *DialogMedia)
	// OnRefer is called on successful REFER handling.
	OnRefer OnReferDialogFunc

	// MutateRequest is the last-chance hook invoked just before a request is sent.
	MutateRequest func(req *sip.Request) error
	// MutateResponse is the last-chance hook invoked just before a response is sent.
	MutateResponse func(res *sip.Response) error

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
}

// SignalOption configures per-call signaling behavior of diago APIs.
// It is accepted by all signaling methods (server, client, NewDialog and REGISTER).
type SignalOption func(*SignalParams) error

// newSignalParams applies all options. It always returns a non-nil params
// struct so callers can safely dereference fields.
func newSignalParams(opts []SignalOption) (*SignalParams, error) {
	p := &SignalParams{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// signalMediaConfig overlays per-call media options on top of the base config.
func signalMediaConfig(base MediaConfig, p *SignalParams) MediaConfig {
	conf := base
	if p == nil {
		return conf
	}
	if p.Codecs != nil {
		conf.Codecs = p.Codecs
	}
	if p.RTPNAT != nil {
		conf.RTPNAT = *p.RTPNAT
	}
	if p.MediaBindIP != nil {
		conf.BindIP = p.MediaBindIP
	}
	if p.MediaExternalIP != nil {
		conf.ExternalIP = p.MediaExternalIP
	}
	if p.MediaDTLSConf != nil {
		conf.DTLSConf = *p.MediaDTLSConf
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
func applyRequestSignal(req *sip.Request, params *SignalParams) error {
	if params == nil {
		return nil
	}
	if params.Contact != nil {
		setSignalContact(req, params.Contact)
	}
	applySignalHeaders(req, params.Headers)
	if params.MutateRequest != nil {
		return params.MutateRequest(req)
	}
	return nil
}

// buildReInviteRequest fills a re-INVITE request from base SDP body and
// SignalParams: it applies default Contact (if non-nil), lets params.Contact
// override, swaps the body when WithBody is set, sets Content-Type, and runs
// the request mutator. Caller is responsible for sending the request.
func buildReInviteRequest(req *sip.Request, baseSDP []byte, defaultContact *sip.ContactHeader, params *SignalParams) error {
	body := baseSDP
	if params != nil && params.Body != nil {
		body = params.Body
	}
	if defaultContact != nil && (params == nil || params.Contact == nil) {
		req.AppendHeader(defaultContact)
	}
	if params != nil && params.Contact != nil {
		setSignalContact(req, params.Contact)
	}
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(body)
	return applyRequestSignal(req, params)
}

// WithHeaders appends custom headers to the outgoing SIP message.
func WithHeaders(headers ...sip.Header) SignalOption {
	return func(p *SignalParams) error {
		for _, h := range headers {
			if h == nil {
				return fmt.Errorf("WithHeaders: nil header provided")
			}
		}
		p.Headers = append(p.Headers, headers...)
		return nil
	}
}

// WithHeader is a convenience wrapper appending a single header by name and value.
func WithHeader(name string, value string) SignalOption {
	return WithHeaders(sip.NewHeader(name, value))
}

// WithContact replaces the default Contact header of the outgoing message.
// On server side it applies to responses, on client side to requests.
func WithContact(contact *sip.ContactHeader) SignalOption {
	return func(p *SignalParams) error {
		if contact == nil {
			return fmt.Errorf("WithContact: contact header is nil")
		}
		p.Contact = contact
		return nil
	}
}

// WithBody overrides the outgoing body. Content-Type defaults to
// "application/sdp" unless provided within Headers.
func WithBody(body []byte) SignalOption {
	return func(p *SignalParams) error {
		p.Body = body
		return nil
	}
}

// WithCodecs overrides the codecs offered in the SDP for this call.
func WithCodecs(codecs ...media.Codec) SignalOption {
	return func(p *SignalParams) error {
		p.Codecs = codecs
		return nil
	}
}

// WithRTPNAT sets media.MediaSession.RTPNAT for this call.
// Check media.RTPNAT options.
func WithRTPNAT(n int) SignalOption {
	return func(p *SignalParams) error {
		v := n
		p.RTPNAT = &v
		return nil
	}
}

// WithMediaBindIP overrides the local RTP/RTCP bind IP for this call.
func WithMediaBindIP(ip net.IP) SignalOption {
	return func(p *SignalParams) error {
		if ip == nil {
			return fmt.Errorf("WithMediaBindIP: ip is nil")
		}
		p.MediaBindIP = ip
		return nil
	}
}

// WithMediaExternalIP overrides the IP advertised in the SDP (c= line) for this call.
func WithMediaExternalIP(ip net.IP) SignalOption {
	return func(p *SignalParams) error {
		if ip == nil {
			return fmt.Errorf("WithMediaExternalIP: ip is nil")
		}
		p.MediaExternalIP = ip
		return nil
	}
}

// WithMediaDTLS overrides the DTLS configuration for this call.
func WithMediaDTLS(conf media.DTLSConfig) SignalOption {
	return func(p *SignalParams) error {
		c := conf
		p.MediaDTLSConf = &c
		return nil
	}
}

// WithMediaSession passes a fully custom/pre-created media session.
// The library will use it as is instead of creating its own session.
func WithMediaSession(m *media.MediaSession) SignalOption {
	return func(p *SignalParams) error {
		if m == nil {
			return fmt.Errorf("WithMediaSession: media session is nil")
		}
		p.MediaSession = m
		return nil
	}
}

// WithOnResponse sets a response callback used during dialog establishment (client side).
func WithOnResponse(fn func(res *sip.Response) error) SignalOption {
	return func(p *SignalParams) error {
		p.OnResponse = fn
		return nil
	}
}

// WithOnMediaUpdate sets the media update callback (re-INVITE handling).
func WithOnMediaUpdate(fn func(d *DialogMedia)) SignalOption {
	return func(p *SignalParams) error {
		p.OnMediaUpdate = fn
		return nil
	}
}

// WithOnRefer sets the REFER handler callback.
func WithOnRefer(fn OnReferDialogFunc) SignalOption {
	return func(p *SignalParams) error {
		p.OnRefer = fn
		return nil
	}
}

// WithRequestMutator registers a last-chance hook invoked with the outgoing
// request just before it is sent. Use it for anything not covered by
// dedicated options.
func WithRequestMutator(fn func(req *sip.Request) error) SignalOption {
	return func(p *SignalParams) error {
		if fn == nil {
			return fmt.Errorf("WithRequestMutator: fn is nil")
		}
		p.MutateRequest = fn
		return nil
	}
}

// WithResponseMutator registers a last-chance hook invoked with the outgoing
// response just before it is sent. Use it for anything not covered by
// dedicated options.
func WithResponseMutator(fn func(res *sip.Response) error) SignalOption {
	return func(p *SignalParams) error {
		if fn == nil {
			return fmt.Errorf("WithResponseMutator: fn is nil")
		}
		p.MutateResponse = fn
		return nil
	}
}

// WithDialogTransport selects the transport used for a new dialog by name (udp, tcp, ...).
// NewDialog only.
func WithDialogTransport(name string) SignalOption {
	return func(p *SignalParams) error {
		p.Transport = name
		return nil
	}
}

// WithDialogTransportID selects the transport used for a new dialog by its configured ID.
// NewDialog only.
func WithDialogTransportID(id string) SignalOption {
	return func(p *SignalParams) error {
		p.TransportID = id
		return nil
	}
}

// WithOriginator sets the originator dialog whose SDP/codecs are reused for the
// outgoing INVITE, avoiding media transcoding. Invite only.
func WithOriginator(o DialogSession) SignalOption {
	return func(p *SignalParams) error {
		if o == nil {
			return fmt.Errorf("WithOriginator: originator is nil")
		}
		p.Originator = o
		return nil
	}
}

// WithAuthCredentials sets username/password used for digest authentication.
// Invite/Register only.
func WithAuthCredentials(username string, password string) SignalOption {
	return func(p *SignalParams) error {
		p.Username = username
		p.Password = password
		return nil
	}
}

// WithEarlyMediaDetect enables early media detection on outgoing INVITE.
// Invite returns ErrClientEarlyMedia when 183 Session Progress with SDP is received.
// Invite only.
func WithEarlyMediaDetect() SignalOption {
	return func(p *SignalParams) error {
		p.EarlyMediaDetect = true
		return nil
	}
}
