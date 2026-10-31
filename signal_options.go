// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
)

// CloseOptions configures Call.Close. Headers are added to the outgoing BYE,
// CANCEL or decline; Contact replaces the default Contact; MutateRequest is
// the last-chance request hook.
type CloseOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	MutateRequest func(req *Request) error
}

func (o CloseOptions) signalParams() (*signalParams, error) {
	p := &signalParams{}
	for _, h := range o.Headers {
		if h == nil {
			return nil, fmt.Errorf("%w: CloseOptions: nil header provided", ErrInvalidOption)
		}
	}
	p.Msg.Headers = append(p.Msg.Headers, o.Headers...)
	if o.Contact != nil {
		p.Msg.Contact = o.Contact
	}
	if o.MutateRequest != nil {
		p.Msg.MutateRequest = o.MutateRequest
	}
	return p, nil
}

func closeSignalParams(opts ...CloseOptions) (*signalParams, error) {
	if len(opts) == 0 {
		return &signalParams{}, nil
	}
	if len(opts) > 1 {
		return nil, fmt.Errorf("%w: Close accepts at most one CloseOptions", ErrInvalidOption)
	}
	return opts[0].signalParams()
}

// AckOptions configures the final ACK of an outbound call.
type AckOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	MutateRequest func(req *Request) error
}

func (o AckOptions) signalParams() (*signalParams, error) {
	return messageSignalParams(o.Headers, o.Contact, nil, o.MutateRequest, nil)
}

// ResponseOptions configures Trying, Ringing, Progress and RespondSDP.
type ResponseOptions struct {
	Headers        []Header
	Contact        *ContactHeader
	MutateResponse func(res *Response) error
}

func (o ResponseOptions) signalParams() (*signalParams, error) {
	return messageSignalParams(o.Headers, o.Contact, nil, nil, o.MutateResponse)
}

// RequestOptions configures in-dialog requests without media negotiation:
// ReInvite and Unhold.
type RequestOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	Body          []byte
	MutateRequest func(req *Request) error
}

func (o RequestOptions) signalParams() (*signalParams, error) {
	return messageSignalParams(o.Headers, o.Contact, o.Body, o.MutateRequest, nil)
}

// HoldOptions configures Hold (a re-INVITE with optional hold music).
type HoldOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	Body          []byte
	MutateRequest func(req *Request) error
	MusicOnHold   *audio.Tone
}

func (o HoldOptions) signalParams() (*signalParams, error) {
	p, err := messageSignalParams(o.Headers, o.Contact, o.Body, o.MutateRequest, nil)
	if err != nil {
		return nil, err
	}
	if o.MusicOnHold != nil {
		p.Media.MusicOnHold = o.MusicOnHold
	}
	return p, nil
}

// ReferOptions configures REFER on both caller and callee dialogs.
type ReferOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	MutateRequest func(req *Request) error
	OnReferNotify func(statusCode int)
}

func (o ReferOptions) signalParams() (*signalParams, error) {
	p, err := messageSignalParams(o.Headers, o.Contact, nil, o.MutateRequest, nil)
	if err != nil {
		return nil, err
	}
	if o.OnReferNotify != nil {
		p.Dialog.OnReferNotify = o.OnReferNotify
	}
	return p, nil
}

func messageSignalParams(headers []Header, contact *ContactHeader, body []byte, mutateReq func(*Request) error, mutateResp func(*Response) error) (*signalParams, error) {
	p := &signalParams{}
	for _, h := range headers {
		if h == nil {
			return nil, fmt.Errorf("%w: nil header provided", ErrInvalidOption)
		}
	}
	p.Msg.Headers = slices.Clone(headers)
	if contact != nil {
		p.Msg.Contact = contact
	}
	if body != nil {
		p.Msg.Body = slices.Clone(body)
	}
	if mutateReq != nil {
		p.Msg.MutateRequest = mutateReq
	}
	if mutateResp != nil {
		p.Msg.MutateResponse = mutateResp
	}
	return p, nil
}

func singleSignalParams[T any](name string, opts []T, apply func(T) (*signalParams, error)) (*signalParams, error) {
	if len(opts) == 0 {
		return &signalParams{}, nil
	}
	if len(opts) > 1 {
		return nil, fmt.Errorf("%w: %s accepts at most one options value", ErrInvalidOption, name)
	}
	return apply(opts[0])
}

func ackSignalParams(opts ...AckOptions) (*signalParams, error) {
	return singleSignalParams("Ack", opts, AckOptions.signalParams)
}
func responseSignalParams(opts ...ResponseOptions) (*signalParams, error) {
	return singleSignalParams("Response", opts, ResponseOptions.signalParams)
}
func requestSignalParams(opts ...RequestOptions) (*signalParams, error) {
	return singleSignalParams("Request", opts, RequestOptions.signalParams)
}
func holdSignalParams(opts ...HoldOptions) (*signalParams, error) {
	return singleSignalParams("Hold", opts, HoldOptions.signalParams)
}
func referSignalParams(opts ...ReferOptions) (*signalParams, error) {
	return singleSignalParams("Refer", opts, ReferOptions.signalParams)
}

// MediaOptions overrides the per-call media configuration.
type MediaOptions struct {
	// Codecs overrides the offered codecs. Nil keeps the dialog defaults.
	Codecs []media.Codec
	// RTPNAT selects the RTP NAT mode. Nil keeps the dialog default.
	RTPNAT *RTPNATMode
	// BindIP overrides the local RTP/RTCP bind IP.
	BindIP net.IP
	// ExternalIP overrides the IP advertised in SDP.
	ExternalIP net.IP
	// DTLS overrides the DTLS configuration. A non-zero value enables
	// DTLS-SRTP unless SRTP mode is already set.
	DTLS *media.DTLSConfig
	// SessionName overrides the SDP "s=" line for this call.
	SessionName string
	// Session supplies a fully custom/pre-created media session. The library
	// takes ownership of it.
	Session *media.MediaSession
	// MusicOnHold overrides the dialog-level hold music. A zero tone disables
	// hold music for the call.
	MusicOnHold *audio.Tone
}

func (o MediaOptions) apply(p *signalParams) error {
	if o.Codecs != nil {
		if len(o.Codecs) == 0 {
			return fmt.Errorf("%w: MediaOptions.Codecs is empty", ErrInvalidOption)
		}
		p.Media.Codecs = slices.Clone(o.Codecs)
	}
	if o.RTPNAT != nil {
		if *o.RTPNAT != RTPNATDisabled && *o.RTPNAT != RTPNATLearnSource {
			return fmt.Errorf("%w: MediaOptions.RTPNAT: invalid mode %d", ErrInvalidOption, *o.RTPNAT)
		}
		v := *o.RTPNAT
		p.Media.RTPNAT = &v
	}
	if o.BindIP != nil {
		p.Media.MediaBindIP = o.BindIP
	}
	if o.ExternalIP != nil {
		p.Media.MediaExternalIP = o.ExternalIP
	}
	if o.DTLS != nil {
		if dtlsConfIsZero(*o.DTLS) {
			return fmt.Errorf("%w: MediaOptions.DTLS: configuration is empty", ErrInvalidOption)
		}
		c := *o.DTLS
		p.Media.MediaDTLSConf = &c
	}
	if o.SessionName != "" {
		if err := media.ValidateSDPSessionName(o.SessionName); err != nil {
			return fmt.Errorf("%w: MediaOptions.SessionName: %w", ErrInvalidOption, err)
		}
		p.Media.SDPSessionName = o.SessionName
	}
	if o.Session != nil {
		p.Media.MediaSession = o.Session
	}
	if o.MusicOnHold != nil {
		p.Media.MusicOnHold = o.MusicOnHold
	}
	return nil
}

// ProgressMediaOptions configures ProgressMedia (183 with early media).
type ProgressMediaOptions struct {
	Headers        []Header
	Contact        *ContactHeader
	Body           []byte
	MutateResponse func(res *Response) error
	Media          MediaOptions
}

func (o ProgressMediaOptions) signalParams() (*signalParams, error) {
	p, err := messageSignalParams(o.Headers, o.Contact, o.Body, nil, o.MutateResponse)
	if err != nil {
		return nil, err
	}
	return p, o.Media.apply(p)
}

// AnswerOptions configures Answer and AnswerLate.
type AnswerOptions struct {
	Headers        []Header
	Contact        *ContactHeader
	Body           []byte
	MutateResponse func(res *Response) error
	Media          MediaOptions
	OnMediaUpdate  func(d *Media)
	OnRefer        OnReferFunc
}

func (o AnswerOptions) signalParams() (*signalParams, error) {
	p, err := messageSignalParams(o.Headers, o.Contact, o.Body, nil, o.MutateResponse)
	if err != nil {
		return nil, err
	}
	if err := o.Media.apply(p); err != nil {
		return nil, err
	}
	if o.OnMediaUpdate != nil {
		p.Dialog.OnMediaUpdate = o.OnMediaUpdate
	}
	if o.OnRefer != nil {
		p.Dialog.OnRefer = o.OnRefer
	}
	return p, nil
}

// InviteOptions configures the initial INVITE of an outbound call.
type InviteOptions struct {
	Headers          []Header
	Contact          *ContactHeader
	Body             []byte
	MutateRequest    func(req *Request) error
	Media            MediaOptions
	Originator       Call
	Username         string
	Password         string
	EarlyMediaDetect bool
	OnResponse       func(res *Response) error
	OnMediaUpdate    func(d *Media)
	OnRefer          OnReferFunc
}

func (o InviteOptions) signalParams() (*signalParams, error) {
	return inviteSignalParams(inviteOptions{
		Headers: o.Headers, Contact: o.Contact, Body: o.Body,
		MutateRequest: o.MutateRequest, Media: o.Media, Originator: o.Originator,
		Username: o.Username, Password: o.Password,
		EarlyMediaDetect: o.EarlyMediaDetect,
		OnResponse:       o.OnResponse, OnMediaUpdate: o.OnMediaUpdate, OnRefer: o.OnRefer,
	})
}

// DialOptions configures Dial/DialURI and DialBridge/DialBridgeURI.
type DialOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	Body          []byte
	MutateRequest func(req *Request) error
	Media         MediaOptions
	Originator    Call
	Username      string
	Password      string
	OnResponse    func(res *Response) error
	OnMediaUpdate func(d *Media)
	OnRefer       OnReferFunc
	Transport     string
	TransportID   string
}

func (o DialOptions) signalParams() (*signalParams, error) {
	p, err := inviteSignalParams(inviteOptions{
		Headers: o.Headers, Contact: o.Contact, Body: o.Body,
		MutateRequest: o.MutateRequest, Media: o.Media, Originator: o.Originator,
		Username: o.Username, Password: o.Password,
		OnResponse: o.OnResponse, OnMediaUpdate: o.OnMediaUpdate, OnRefer: o.OnRefer,
	})
	if err != nil {
		return nil, err
	}
	p.Dialog.Transport = o.Transport
	p.Dialog.TransportID = o.TransportID
	return p, nil
}

// NewCallOptions configures NewCall (transport selection and Contact only).
type NewCallOptions struct {
	Contact     *ContactHeader
	Transport   string
	TransportID string
}

func (o NewCallOptions) signalParams() (*signalParams, error) {
	p := &signalParams{}
	if o.Contact != nil {
		p.Msg.Contact = o.Contact
	}
	p.Dialog.Transport = o.Transport
	p.Dialog.TransportID = o.TransportID
	return p, nil
}

// WaitAnswerOptions configures WaitAnswer after early media detection.
type WaitAnswerOptions struct {
	OnResponse func(res *Response) error
	Username   string
	Password   string
}

func (o WaitAnswerOptions) signalParams() (*signalParams, error) {
	p := &signalParams{}
	if o.OnResponse != nil {
		p.Dialog.OnResponse = o.OnResponse
	}
	p.Dialog.Username = o.Username
	p.Dialog.Password = o.Password
	return p, nil
}

type inviteOptions struct {
	Headers          []Header
	Contact          *ContactHeader
	Body             []byte
	MutateRequest    func(req *Request) error
	Media            MediaOptions
	Originator       Call
	Username         string
	Password         string
	EarlyMediaDetect bool
	OnResponse       func(res *Response) error
	OnMediaUpdate    func(d *Media)
	OnRefer          OnReferFunc
}

func inviteSignalParams(o inviteOptions) (*signalParams, error) {
	p, err := messageSignalParams(o.Headers, o.Contact, o.Body, o.MutateRequest, nil)
	if err != nil {
		return nil, err
	}
	if err := o.Media.apply(p); err != nil {
		return nil, err
	}
	if o.Originator != nil {
		p.Dialog.Originator = o.Originator
	}
	if o.Username == "" && o.Password == "" {
		// no credentials
	} else {
		p.Dialog.Username = o.Username
		p.Dialog.Password = o.Password
	}
	p.Dialog.EarlyMediaDetect = o.EarlyMediaDetect
	if o.OnResponse != nil {
		p.Dialog.OnResponse = o.OnResponse
	}
	if o.OnMediaUpdate != nil {
		p.Dialog.OnMediaUpdate = o.OnMediaUpdate
	}
	if o.OnRefer != nil {
		p.Dialog.OnRefer = o.OnRefer
	}
	return p, nil
}

func progressMediaSignalParams(opts ...ProgressMediaOptions) (*signalParams, error) {
	return singleSignalParams("ProgressMedia", opts, ProgressMediaOptions.signalParams)
}
func answerSignalParams(opts ...AnswerOptions) (*signalParams, error) {
	return singleSignalParams("Answer", opts, AnswerOptions.signalParams)
}
func inviteSignalParamsFrom(opts ...InviteOptions) (*signalParams, error) {
	return singleSignalParams("Invite", opts, InviteOptions.signalParams)
}
func dialSignalParams(opts ...DialOptions) (*signalParams, error) {
	return singleSignalParams("Dial", opts, DialOptions.signalParams)
}
func newCallSignalParams(opts ...NewCallOptions) (*signalParams, error) {
	return singleSignalParams("NewCall", opts, NewCallOptions.signalParams)
}
func waitAnswerSignalParams(opts ...WaitAnswerOptions) (*signalParams, error) {
	return singleSignalParams("WaitAnswer", opts, WaitAnswerOptions.signalParams)
}

// RegisterAttemptOptions configures a single REGISTER attempt
// (RegisterTransaction.Register/Unregister/Qualify).
type RegisterAttemptOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	MutateRequest func(req *Request) error
	Username      string
	Password      string
}

func (o RegisterAttemptOptions) signalParams() (*signalParams, error) {
	return registerAttemptParams(o.Headers, o.Contact, o.MutateRequest, o.Username, o.Password)
}

// RegisterOptions configures a REGISTER transaction
// (Agent.Register/RegisterURI/RegisterTransaction).
type RegisterOptions struct {
	Headers       []Header
	Contact       *ContactHeader
	MutateRequest func(req *Request) error
	Username      string
	Password      string

	Expiry        time.Duration
	RetryInterval time.Duration
	AllowHeaders  []string
	ProxyHost     string
	OnRegistered  func()
	Detached      bool
}

func (o RegisterOptions) signalParams() (*signalParams, error) {
	p, err := registerAttemptParams(o.Headers, o.Contact, o.MutateRequest, o.Username, o.Password)
	if err != nil {
		return nil, err
	}
	if o.Expiry < 0 {
		return nil, fmt.Errorf("%w: RegisterOptions.Expiry: negative duration %s", ErrInvalidOption, o.Expiry)
	}
	if o.RetryInterval < 0 {
		return nil, fmt.Errorf("%w: RegisterOptions.RetryInterval: negative duration %s", ErrInvalidOption, o.RetryInterval)
	}
	for _, h := range o.AllowHeaders {
		if h == "" {
			return nil, fmt.Errorf("%w: RegisterOptions.AllowHeaders: empty value", ErrInvalidOption)
		}
	}
	p.Register.Expiry = o.Expiry
	p.Register.RetryInterval = o.RetryInterval
	p.Register.AllowHeaders = slices.Clone(o.AllowHeaders)
	p.Register.ProxyHost = o.ProxyHost
	if o.OnRegistered != nil {
		p.Register.OnRegistered = o.OnRegistered
	}
	p.Register.Detached = o.Detached
	return p, nil
}

func registerAttemptParams(headers []Header, contact *ContactHeader, mutateReq func(*Request) error, username, password string) (*signalParams, error) {
	p, err := messageSignalParams(headers, contact, nil, mutateReq, nil)
	if err != nil {
		return nil, err
	}
	if username == "" && password == "" {
		return p, nil
	}
	p.Dialog.Username = username
	p.Dialog.Password = password
	return p, nil
}

func registerAttemptSignalParams(opts ...RegisterAttemptOptions) (*signalParams, error) {
	return singleSignalParams("RegisterAttempt", opts, RegisterAttemptOptions.signalParams)
}
func registerSignalParams(opts ...RegisterOptions) (*signalParams, error) {
	return singleSignalParams("Register", opts, RegisterOptions.signalParams)
}

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
	// Detached keeps the registration out of Agent.Close.
	Detached bool
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
// skipped defensively; the option structs reject nil headers before applying.
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
// override, swaps the body when RequestOptions.Body is set, sets Content-Type, and runs
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
