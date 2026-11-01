// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/dtls/v3"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/sjlit/diago/v2/media/sdp"
)

// RTPTracer receives decoded RTP packets when MediaSession.RTPDebug is
// enabled and a Tracer is configured.
//
// Traces run synchronously on the media path. Implementations must not block
// and must clone packets that are retained after the method returns.
type RTPTracer interface {
	RTPTraceRead(laddr string, raddr string, packet *rtp.Packet)
	RTPTraceWrite(laddr string, raddr string, packet *rtp.Packet)
}

// RTPBufSize is the size of buffers used on RTP read paths (MTU guard).
// It is a constant so no packet path depends on process-wide mutable state.
const RTPBufSize = 1500

var rtpPortOffset = atomic.Int32{}

// SecureRTP selects the SRTP keying mode of a media session.
type SecureRTP int

const (
	// SecureRTPNone disables SRTP.
	SecureRTPNone SecureRTP = iota
	// SecureRTPSDES selects SDES keying.
	SecureRTPSDES
	// SecureRTPDTLS selects DTLS-SRTP.
	SecureRTPDTLS
)

// SRTPAlg is an SRTP protection profile.
type SRTPAlg uint16

// RTPNATMode selects how RTP behind NAT is handled.
type RTPNATMode int

// RTPNAT options.
const (
	RTPNATDisabled RTPNATMode = iota
	RTPNATLearnSource
	// RTPNATSymetric is the historical name for RTPNATLearnSource.
	RTPNATSymetric = RTPNATLearnSource
)

func logRTPRead(m *MediaSession, raddr net.Addr, p *rtp.Packet) {
	if !m.RTPDebug {
		return
	}

	laddr := m.Laddr.String()
	raddrStr := raddr.String()
	if m.Tracer != nil {
		m.Tracer.RTPTraceRead(laddr, raddrStr, p)
		return
	}

	m.logger().Debug(fmt.Sprintf("RTP read %s < %s:\n%s", laddr, raddrStr, p.String()))
}

func logRTPWrite(m *MediaSession, p *rtp.Packet) {
	if !m.RTPDebug {
		return
	}

	laddr := m.Laddr.String()
	raddr := m.Raddr.String()
	if m.Tracer != nil {
		m.Tracer.RTPTraceWrite(laddr, raddr, p)
		return
	}

	m.logger().Debug(fmt.Sprintf("RTP write %s > %s:\n%s", laddr, raddr, p.String()))
}

func logRTCPRead(m *MediaSession, pkts []rtcp.Packet) {
	if m.RTCPDebug {
		laddr := m.rtcpConn.LocalAddr()
		for _, p := range pkts {
			m.logger().Debug(fmt.Sprintf("RTCP read %s < %s:\n%s", laddr.String(), m.rtcpRaddr.String(), StringRTCP(p)))
		}
	}
}

func logRTCPWrite(m *MediaSession, p rtcp.Packet) {
	if m.RTCPDebug {
		laddr := m.rtcpConn.LocalAddr()
		m.logger().Debug(fmt.Sprintf("RTCP write %s > %s:\n%s", laddr.String(), m.rtcpRaddr.String(), StringRTCP(p)))
	}
}

// MediaSession represents a single media (audio) session with RTP/RTCP over
// one Laddr <-> Raddr pair.
//
// Lifecycle contract:
//   - Config phase: fields may be set freely before Init().
//   - Active phase: after a completed offer/answer exchange the session is
//     FROZEN. Only IO (ReadRTP/WriteRTP/ReadRTCP/WriteRTCP/StopRTP/StartRTP)
//     and read-only queries (CommonCodecs, DTMFCodecNegotiated) are allowed. Any change
//     (codecs, direction, addresses) requires Fork() and installing the fork
//     through diago's Media.
//
// TODO: multiple media descriptions.
// Consider https://datatracker.ietf.org/doc/rfc3388/ for grouping multiple media
//
// NOTE: Not thread safe. Negotiation (RemoteSDP/LocalSDP/Finalize) must be
// funneled through a single goroutine; IO goes through diago's
// RTPPacketReader/RTPPacketWriter stable handles. Concurrent WriteRTP calls do
// not corrupt each other's packet bytes (pooled per-call marshaling), but
// ordering, sequence numbers and SSRC bookkeeping still assume a single
// writer per direction.

type MediaSession struct {
	// SDP stuff
	// TODO:
	// 1. make this list of codecs as we need to match also sample rate and ptime
	// 2. rtp session when matching incoming packet sample rate for RTCP should use this

	// Codecs are initial list of Codecs that would be used in SDP generation
	Codecs []Codec

	sdp []byte

	// Creation-time config normally set from diago's MediaConfig. Set only in
	// the Config phase (before Init); carried through Fork for re-INVITE
	// consistency.
	RTPPortStart             int
	RTPPortEnd               int
	SDPCodecPreferLocalOrder bool
	// SDPSessionName overrides the SDP "s=" line. Empty keeps the default
	// "Sip Go Media". Set only in the Config phase; carried through Fork.
	SDPSessionName string

	// Logger receives this session's logs. Nil falls back to the media
	// package default (slog.Default). Carried through Fork.
	Logger *slog.Logger

	// RTPDebug enables RTP packet tracing on this session. When Tracer is
	// set packets are delivered to it; otherwise they are logged through
	// Logger. Set only in the Config phase; carried through Fork.
	RTPDebug bool
	// RTCPDebug enables RTCP packet logging through Logger. Set only in the
	// Config phase; carried through Fork.
	RTCPDebug bool
	// Tracer receives RTP packets when RTPDebug is enabled. Nil logs instead.
	// Set only in the Config phase; carried through Fork.
	Tracer RTPTracer
	// SAVPDisable keeps RTP/AVP in the offered SDP profile even when SRTP is
	// enabled (a=crypto compatibility mode), unless the remote offer already
	// uses RTP/SAVP. Set only in the Config phase; carried through Fork.
	SAVPDisable bool
	// SourceLock enables RTP source locking after IP changes. Set only in the
	// Config phase; carried through Fork.
	SourceLock bool
	// OnReadRTCP/OnWriteRTCP are the RTCP hooks installed on RTPSession
	// instances created from this session. Nil keeps the previous hook
	// behavior. Set only in the Config phase; carried through Fork.
	OnReadRTCP  func(pkt rtcp.Packet, rtpStats RTPReadStats)
	OnWriteRTCP func(pkt rtcp.Packet, rtpStats RTPWriteStats)

	// Mode is sdp mode. Check consts sdp.ModeRecvOnly etc...
	Mode string
	// Laddr our local address which has full IP and port after media session creation
	Laddr net.UDPAddr
	// Raddr is our target remote address. Normally it is resolved by SDP parsing.
	Raddr net.UDPAddr
	// ExternalIP that should be used for building SDP
	ExternalIP net.IP

	SecureRTP SecureRTP // 0 none, 1 - SDES, 2 - DTLS
	// TODO support multile for offering
	SRTPAlg SRTPAlg

	// DTLSConf used for DTLS
	DTLSConf DTLSConfig

	// mode set after negotiation
	mode string

	// filterCodecs is common list of codecs after negotiation
	filterCodecs []Codec
	rtpConn      net.PacketConn
	rtcpConn     net.PacketConn
	rtcpRaddr    net.UDPAddr

	// SRTP
	localCtxSRTP  *srtp.Context
	remoteCtxSRTP *srtp.Context
	srtpRemoteTag int

	// RTP NAT enables handling RTP behind NAT. Checkout also MediaSession.SourceLock
	RTPNAT          RTPNATMode // 0 - disabled, 1 - Learn source change (RTP Symetric)
	learnedRTPFrom  atomic.Pointer[net.UDPAddr]
	learnedRTCPFrom atomic.Pointer[net.UDPAddr]

	// ReadRTPFromAddr is set after Read operation. NOT THREAD SAFE and should be only used together with Read
	// It can be used to validate source of RTP packet
	ReadRTPFromAddr net.Addr
	// remoteProto is the media protocol from the remote SDP offer (e.g. "RTP/AVP", "RTP/SAVP")
	remoteProto string

	// DTLS
	dtlsConn *dtls.Conn
	// dtlsEstablished is set after the first successful DTLS handshake.
	// Carried through Fork so a re-INVITE reuses the established association
	// and its SRTP contexts instead of downgrading to plain RTP.
	isDTLSEstablished bool

	onFinalize func() error

	sessionID      uint64
	sessionVersion uint64
}

func NewMediaSession(ip net.IP, port int) (s *MediaSession, e error) {
	s = &MediaSession{
		Codecs: []Codec{
			CodecAudioUlaw, CodecAudioAlaw, CodecTelephoneEvent8000,
		},
		Mode: sdp.ModeSendrecv,
	}
	s.Laddr.IP = ip
	s.Laddr.Port = port

	return s, s.Init()
}

// Init should be called if session is created manually
// Use NewMediaSession for default building
func (s *MediaSession) Init() error {
	if len(s.Codecs) == 0 {
		return fmt.Errorf("media session: formats can not be empty")
	}

	if s.Mode == "" {
		return fmt.Errorf("media session: mode must be set")
	}

	if s.Laddr.IP == nil {
		return fmt.Errorf("media session: local addr must be set")
	}

	if s.SecureRTP > 0 && s.SRTPAlg == 0 {
		s.SRTPAlg = SRTPAlg(srtp.ProtectionProfileAes128CmHmacSha1_80)
	}

	// Try to listen on this ports
	if err := s.createListeners(&s.Laddr); err != nil {
		return err
	}

	return nil
}

// logger returns the session logger with the package default as fallback.
func (s *MediaSession) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return DefaultLogger()
}

func (s *MediaSession) String() string {
	return fmt.Sprintln(
		"rtp.connection", s.rtpConn.LocalAddr().String(),
		"rtcp.connection", s.rtcpConn.LocalAddr().String(),
		"remote.addr", s.Raddr.String(),
	)
}

// StopRTP pauses reading and/or writing by expiring the shared conn deadline.
// This is a durable, global state on the conn: any later StartRTP clears it
// for everyone, so concurrent components overwrite each other.
//
// It is a low-level primitive retained for the deadline-based components
// (bridge mix polling, DTMF read windows). New code should use the refcounted
// gate APIs on the stable handles: diago's Media.PauseAudioRead /
// PauseAudioWrite (PauseRead/PauseWrite in this package). The rw mask values
// 1 (read) and 2 (write) can be combined.
func (s *MediaSession) StopRTP(rw int8, dur time.Duration) error {
	t := time.Now().Add(dur)
	if rw&1 != 0 && rw&2 != 0 {
		return s.rtpConn.SetDeadline(t)
	}
	if rw&1 != 0 {
		return s.rtpConn.SetReadDeadline(t)
	}
	if rw&2 != 0 {
		return s.rtpConn.SetWriteDeadline(t)
	}
	return s.rtpConn.SetDeadline(t)
}

// StartRTP clears the conn deadline (both directions when the mask covers
// them), restoring blocking reads and writes. It is the counterpart of
// StopRTP; see there for when to prefer the gate APIs.
func (s *MediaSession) StartRTP(rw int8) error {
	if rw&1 != 0 && rw&2 != 0 {
		return s.rtpConn.SetDeadline(time.Time{})
	}
	if rw&1 != 0 {
		return s.rtpConn.SetReadDeadline(time.Time{})
	}
	if rw&2 != 0 {
		return s.rtpConn.SetWriteDeadline(time.Time{})
	}
	return s.rtpConn.SetDeadline(time.Time{})
}

// Fork creates a draft copy for a session update (re-INVITE, hold, ...).
// The draft shares the parent's RTP/RTCP connections; everything negotiated
// (Raddr, rtcpRaddr, negotiated mode, filterCodecs, SRTP/DTLS state, NAT-learned
// addresses) is reset and must be re-established via RemoteSDP on the draft.
// The draft becomes active only when installed through diago's Media
// mutating the active session in place is forbidden.
//
// An established DTLS-SRTP association is the exception: its keys cannot be
// re-derived without re-running the handshake (which is impossible while media
// is flowing on the shared socket), so the SRTP contexts are carried over and
// the fork keeps encrypting with the existing association (RFC 5763 §6.6).
func (s *MediaSession) Fork() *MediaSession {
	cp := MediaSession{
		Laddr:          s.Laddr, // TODO clone it although it is read only
		ExternalIP:     slices.Clone(s.ExternalIP),
		rtpConn:        s.rtpConn,
		rtcpConn:       s.rtcpConn,
		Codecs:         slices.Clone(s.Codecs),
		Mode:           s.Mode,
		RTPNAT:         s.RTPNAT,
		sdp:            slices.Clone(s.sdp),
		sessionID:      s.sessionID,
		sessionVersion: s.sessionVersion,
		// Preserve negotiated security state, otherwise re-INVITE would downgrade
		// secured calls to plain RTP. SDES contexts are re-established by the
		// offer/answer path from a=crypto; DTLS keys are carried over below.
		SecureRTP:     s.SecureRTP,
		SRTPAlg:       s.SRTPAlg,
		remoteProto:   s.remoteProto,
		srtpRemoteTag: s.srtpRemoteTag,
		DTLSConf:      s.DTLSConf,
		// Established association is reused (see the context carry-over below).
		isDTLSEstablished: s.isDTLSEstablished,

		// Carry creation-time config so re-INVITE forks keep the same
		// port range and negotiation policy.
		RTPPortStart:             s.RTPPortStart,
		RTPPortEnd:               s.RTPPortEnd,
		SDPCodecPreferLocalOrder: s.SDPCodecPreferLocalOrder,
		SDPSessionName:           s.SDPSessionName,
		Logger:                   s.Logger,
		RTPDebug:                 s.RTPDebug,
		RTCPDebug:                s.RTCPDebug,
		Tracer:                   s.Tracer,
		SAVPDisable:              s.SAVPDisable,
		SourceLock:               s.SourceLock,
		OnReadRTCP:               s.OnReadRTCP,
		OnWriteRTCP:              s.OnWriteRTCP,
	}

	if cp.SecureRTP == 2 && cp.isDTLSEstablished {
		// The DTLS association is reused, not renegotiated: RemoteSDP detects
		// the carried contexts and does not re-arm the handshake.
		cp.localCtxSRTP = s.localCtxSRTP
		cp.remoteCtxSRTP = s.remoteCtxSRTP
	}
	return &cp
}

func (s *MediaSession) Close() error {
	// panic("calling close")
	var e1, e2 error
	if s.rtcpConn != nil {
		e1 = s.rtcpConn.Close()
	}

	if s.rtpConn != nil {
		e2 = s.rtpConn.Close()
	}
	return errors.Join(e1, e2)
}

// SetRemoteAddr is helper to set Raddr and rtcp address.
// The RTCP address is derived as RTP port + 1 (rtcp-mux is not supported).
// It is not thread safe
func (s *MediaSession) SetRemoteAddr(raddr *net.UDPAddr) {
	s.Raddr = *raddr
	s.rtcpRaddr = net.UDPAddr{
		IP:   raddr.IP,
		Port: raddr.Port + 1,
		Zone: raddr.Zone,
	}
}

// LocalSDP generates SDP based on local settings and remote SDP.
// It is part of the offer/answer flow, not a pure getter: the first call
// allocates the o= session id, every call increments its version, and on an
// SRTP offer it creates the local crypto context. SDES setup failures are
// logged and the SDP continues in plaintext. It must never be called in
// parallel to RemoteSDP.
func (s *MediaSession) LocalSDP() []byte {
	if len(s.sdp) > 0 {
		// If media session is static then just return sdp.
		return s.sdp
	}

	ip := s.Laddr.IP
	rtpPort := s.Laddr.Port
	connIP := s.ExternalIP
	if connIP == nil {
		connIP = ip
	}

	codecs := s.Codecs
	if len(s.filterCodecs) > 0 {
		codecs = s.filterCodecs
	}
	// #39: never advertise opus without with_opus_c support.
	codecs = filterUnsupportedCodecs(codecs)

	var localSDES sdesInline
	rtpProfile := "RTP/AVP"
	if s.SecureRTP == 1 {
		// RFC 4568/8643: only include crypto when offering (no remote SDP yet)
		// or when the peer actually offered SRTP
		if s.Raddr.IP == nil || s.remoteCtxSRTP != nil {
			err := func() error {
				// TODO detect algorithm
				profile := srtp.ProtectionProfile(s.SRTPAlg)
				keysalt, keyLen, err := generateMasterKeySalt(profile)
				if err != nil {
					return err
				}
				masterKey, masterSalt := keysalt[:keyLen], keysalt[keyLen:]

				inline := base64.StdEncoding.EncodeToString(keysalt)
				localSDES = sdesInline{
					alg:    srtpProfileString(profile),
					base64: inline,
					tag:    1,
				}

				ctx, err := srtp.CreateContext(masterKey, masterSalt, profile)
				if err != nil {
					return fmt.Errorf("CreateContext failed: %v", err)
				}

				s.localCtxSRTP = ctx

				if s.srtpRemoteTag > 0 {
					// Match remote tag if exists
					localSDES.tag = s.srtpRemoteTag
				}

				// NOTE: For some compatibility reasons (like asterisk) it would be required that this stays on RTP/AVP
				// When the remote offer uses RTP/SAVP, we must mirror it per RFC 3264
				if !s.SAVPDisable || s.remoteProto == "RTP/SAVP" {
					rtpProfile = "RTP/SAVP"
				}
				return nil
			}()
			if err != nil {
				s.logger().Error("Failed to setup SRTP context", "error", err)
			}
		}
	}

	var dtlsSet *dtlsSetup
	if s.SecureRTP == 2 {
		rtpProfile = "UDP/TLS/RTP/SAVP"
		dtlsSet = &dtlsSetup{
			setup:        "active",
			fingerprints: make([]sdpFingerprints, len(s.DTLSConf.Certificates)),
		}
		if s.Raddr.IP != nil {
			// We do have remote IP, so probably we are server
			//  lets be then passive roll
			dtlsSet.setup = "passive"
		}

		// Allow overriding
		if s.DTLSConf.SDPSetupRole != nil {
			dtlsSet.setup = s.DTLSConf.SDPSetupRole(s.Raddr.IP != nil)
		}
		// DTLS
		// This is only needed for self signed certificates?
		// https://datatracker.ietf.org/doc/html/rfc5763#section-2
		// 	 If Alice uses only self- signed certificates for the communication with Bob, a fingerprint is
		//    included in the SDP offer/answer exchange.
		// 		The fingerprint alone protects against active attacks on the media
		//    but not active attacks on the signaling.  In order to prevent active
		//    attacks on the signaling, "Enhancements for Authenticated Identity
		//    Management in the Session Initiation Protocol (SIP)" [RFC4474]
		for i, cert := range s.DTLSConf.Certificates {
			fingerprint, err := dtlsSHA256Fingerprint(cert)
			if err != nil {
				s.logger().Error("Failed to generate dtls certificate fingerprint", "error", err)
				continue
			}
			dtlsSet.fingerprints[i] = sdpFingerprints{
				fingerprint: fingerprint,
				alg:         "SHA-256",
			}
		}

	}

	if s.sessionID == 0 {
		// Use NTP seconds for the SDP o= session id (mainstream practice); the full 64-bit
		// NTP value exceeds int64 when parsed as signed, breaking strict peers (488).
		s.sessionID = CurrentNTPTimestamp() >> 32
		s.sessionVersion = s.sessionID
	} else {
		s.sessionVersion++
	}

	// handle media direction mode
	mode := s.mode
	if mode == "" {
		mode = s.Mode
	}

	sessionName := s.SDPSessionName
	if sessionName == "" {
		sessionName = "Sip Go Media"
	}
	if err := ValidateSDPSessionName(sessionName); err != nil {
		// LocalSDP has no error return and a caller may still need a well
		// formed offer; degrade to the default name instead of emitting the
		// injected payload.
		s.logger().Error("Invalid SDP session name, using default", "error", err)
		sessionName = "Sip Go Media"
	}

	return generateSDPForAudio(s.sessionID, s.sessionVersion, rtpProfile, ip, connIP, rtpPort, mode, codecs, localSDES, dtlsSet, sessionName, s.logger())
}

// NegotiatedDirection returns the direction negotiated in the last completed
// offer/answer exchange: one of sdp.ModeSendrecv, sdp.ModeSendonly,
// sdp.ModeRecvonly or sdp.ModeInactive. It returns "" before the first
// negotiation and after a Fork (forks reset it until RemoteSDP applies).
//
// The value is written while the owning Media holds its lock (all
// RemoteSDP/install paths do); read it under the same discipline. From
// component code, resolve through Media instead of capturing a session.
func (s *MediaSession) NegotiatedDirection() string {
	return s.mode
}

// writeBlockedByDirection reports whether the negotiated direction forbids
// sending (the remote holds us, or the stream is inactive). WriteRTP drops
// silently in that case; callers can use this to keep statistics honest.
func (m *MediaSession) writeBlockedByDirection() bool {
	return m.mode == sdp.ModeRecvonly || m.mode == sdp.ModeInactive
}

// ValidateSDPSessionName rejects session names that could break out of the
// "s=" line: SDP line breaks are CR or LF, so a name carrying them would
// inject additional SDP attributes. Empty keeps the library default and is
// valid here; option-level empty checks are callers' responsibility.
func ValidateSDPSessionName(name string) error {
	if strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("sdp: session name must not contain line breaks, got %q", name)
	}
	return nil
}

// RemoteSDP applies remote SDP.
// NOTE: It must called ONCE or single thread while negotiation happening.
// For multi negotiation Fork Must be called before
func (s *MediaSession) RemoteSDP(sdpReceived []byte) error {
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(sdpReceived, &sd); err != nil {
		return fmt.Errorf("fail to parse received sdp: %w", err)
	}

	si, err := sd.SessionInformation()
	if err != nil {
		return err
	}

	// 	the origin line MUST
	//    be different in the answer, since the answer is generated by a
	//    different entity.  In that case, the version number in the "o=" line
	//    of the answer is unrelated to the version number in the o line of the
	//    offer.
	answerer := s.sessionID == 0
	if s.sessionID != si.SessionID {
		s.sessionID = si.SessionID
		s.sessionVersion = si.SessionVersion + 1
	}
	// TODO: check below. For now we expect and handle only single audio media
	// For each "m=" line in the offer, there MUST be a corresponding "m="
	//    line in the answer.  The answer MUST contain exactly the same number
	//    of "m=" lines as the offer.
	md, err := sd.MediaDescription("audio")
	if err != nil {
		return err
	}

	// Confirm it is supported profile
	secureRequest := false
	switch md.Proto {
	case "RTP/AVP":
	case "RTP/SAVP":
		secureRequest = true
	case "UDP/TLS/RTP/SAVP", "UDP/TLS/RTP/SAVPF":
		// SAVPF (RTCP feedback) is accepted as DTLS like SAVP; the answer
		// stays SAVP (feedback optional). Do not break browsers offering SAVPF.
	default:
		return fmt.Errorf("unsupported media description protocol proto=%q", md.Proto)
	}
	s.remoteProto = md.Proto

	// #34 (RFC 3264 §6): answer proto must match offer semantics.
	// Do not silently answer RTP/AVP to a DTLS offer or vice versa;
	// callers map these errors to 488.
	isDTLSRemote := md.Proto == "UDP/TLS/RTP/SAVP" || md.Proto == "UDP/TLS/RTP/SAVPF"
	if s.SecureRTP == 2 && !isDTLSRemote {
		return fmt.Errorf("sdp: proto mismatch: dtls-srtp configured but remote offered proto=%q", md.Proto)
	}
	if s.SecureRTP != 2 && isDTLSRemote {
		return fmt.Errorf("sdp: proto mismatch: remote offered dtls-srtp but local SecureRTP=%d", s.SecureRTP)
	}

	codecs := make([]Codec, len(md.Formats))
	attrs := sd.Values("a")
	n, err := CodecsFromSDPRead(md.Formats, attrs, codecs)
	if err != nil {
		if n == 0 {
			// Nothing parsed, break
			return err
		}
		// Partial parse: a malformed attribute on one format must not reject
		// the whole offer (RFC 3264 allows answering with a subset). Keep the
		// codecs that parsed and surface the rest for debugging.
		s.logger().Warn("reading codecs from SDP was not full", "error", err)
	}
	if n == 0 {
		return fmt.Errorf("no codecs found in sdp")
	}

	// https://datatracker.ietf.org/doc/html/rfc3264#section-6.1
	// 	Although the answerer MAY list the formats in their desired order of
	//    preference, it is RECOMMENDED that unless there is a specific reason,
	//    the answerer list formats in the same relative order they were
	//    present in the offer.  In other words, if a stream in the offer lists
	//    audio codecs 8, 22 and 48, in that order, and the answerer only
	//    supports codecs 8 and 48, it is RECOMMENDED that, if the answerer has
	// 	  no reason to change it, the ordering of codecs in the answer be 8,
	//    48, and not 48, 8.  This helps assure that the same codec is used in
	//    both directions.
	if s.updateRemoteCodecs(codecs[:n], answerer) == 0 {
		return fmt.Errorf("no supported codecs found")
	}

	// The connection line may be session level or carried inside the audio
	// media section itself (some peers send no session-level c= at all).
	ci, err := sd.ConnectionInformationFor("audio")
	if err != nil {
		return err
	}
	s.SetRemoteAddr(&net.UDPAddr{IP: ci.IP, Port: md.Port})

	// Check mode for media direction. Resolve against the audio section only:
	// a video section's direction must not enable/disable audio (RFC 4566 §6).
	mode := sd.MediaDirectionFor("audio")
	s.mode = negotiateMediaDirection(mode, s.Mode)

	// Check for SDES
	for _, v := range attrs {
		if strings.HasPrefix(v, "crypto:") {
			vals := strings.Split(v, " ")
			if len(vals) < 3 {
				return fmt.Errorf("sdp: bad crypto attribute attr=%q", v)
			}
			// Parse crypto tag
			tagString := strings.TrimPrefix(vals[0], "crypto:")
			if s.srtpRemoteTag, err = strconv.Atoi(tagString); err != nil {
				return fmt.Errorf("bad crypto tag in %q", v)
			}

			// Parse algorithm
			alg := vals[1]
			profile := srtpProfileParse(alg)
			if profile == 0 {
				continue
			}

			// When this gets into array, we need to check do we want to support it
			if s.SRTPAlg != SRTPAlg(profile) {
				continue
			}

			inline := strings.TrimPrefix(vals[2], "inline:")

			keyBytes, err := base64.StdEncoding.DecodeString(inline)
			if err != nil {
				return fmt.Errorf("failed to decode SDES key: %v", err)
			}
			if len(keyBytes) != 30 {
				return fmt.Errorf("expected 30-byte key, got %d", len(keyBytes))
			}
			// Split into master key (16 bytes) and master salt (14 bytes)
			masterKey := keyBytes[:16]
			masterSalt := keyBytes[16:]

			ctx, err := srtp.CreateContext(masterKey, masterSalt, profile)
			if err != nil {
				return fmt.Errorf("CreateContext failed: %v", err)
			}
			s.remoteCtxSRTP = ctx

			break
		}

	}

	// Check for DTLS
	if len(s.DTLSConf.Certificates) > 0 || s.SecureRTP == 2 {
		setup := ""
		fingerprints := make([]sdpFingerprints, 0, 1) // at least must be 1
		for _, v := range attrs {
			if strings.HasPrefix(v, "setup:") {
				setup = strings.TrimSpace(v[len("setup:"):])
				continue
			}

			// This may only needed to be done when answering call
			if strings.HasPrefix(v, "fingerprint:") {
				vals := strings.Split(v[len("fingerprint:"):], " ")
				if len(vals) < 2 {
					return fmt.Errorf("sdp: bad fingerprint attribute attr=%q", v)
				}
				alg := vals[0]
				fp := vals[1]
				// Verified against the DTLS peer certificate during the
				// handshake (dtlsVerifyConnection, fails closed on mismatch).
				fingerprints = append(fingerprints, sdpFingerprints{
					alg:         alg,
					fingerprint: fp,
				})
			}
		}

		if setup == "" {
			return fmt.Errorf("empty a=setup value attribute for dtls")
		}

		// N7 (RFC 5763): DTLS-SRTP without any a=fingerprint must be
		// rejected. Otherwise stripping fingerprints downgrades to
		// unverified transport and bypasses N1 protection.
		if len(fingerprints) == 0 {
			return fmt.Errorf("sdp: missing a=fingerprint for dtls-srtp")
		}

		// Reuse path: a fork carried the established SRTP contexts over
		// (RFC 5763 §6.6). Re-running the handshake mid-call would race the
		// RTP reader on the shared socket, and the existing association keys
		// stay in effect. setup/fingerprint were parsed and validated above,
		// so a malformed re-offer is still rejected.
		if s.isDTLSEstablished && s.localCtxSRTP != nil && s.remoteCtxSRTP != nil {
			s.logger().Debug("DTLS association reused for media update")
			return nil
		}

		// THIS may need external or after SIP ACK establishment
		dtlsConf := s.DTLSConf.ToLibConf(fingerprints, s.logger())
		role := "client"
		switch setup {
		case "actpass", "passive":
			// we are client, as remote wants to be server
			// if s.dtlsConn == nil {
			// 	panic("No dtls connection")
			// }
			s.dtlsConn, err = dtls.Client(s.rtpConn, &s.Raddr, dtlsConf)
			if err != nil {
				return fmt.Errorf("failed to setup dlts client conn: %w", err)
			}

		case "active":
			role = "server"
			// we are server as remote wants to be client
			s.dtlsConn, err = dtls.Server(s.rtpConn, &s.Raddr, dtlsConf)
			if err != nil {
				return fmt.Errorf("failed to setup dlts server conn: %w", err)
			}
			// return nil

			// NOTE: setup:active allows the answer and the DTLS handshake to occur in parallel.
		default:
			return fmt.Errorf("unknown setup value %q", setup)
		}

		s.onFinalize = func() error {
			s.logger().Debug("Starting dtls handshake",
				"setup", setup,
				"role", role,
				"laddr", s.dtlsConn.LocalAddr().String(),
				"raddr", s.dtlsConn.RemoteAddr().String(),
			)
			if err := s.dtlsConn.Handshake(); err != nil {
				return fmt.Errorf("dtls conn handshake: %w", err)
			}

			s.logger().Debug("Handshake finished. Checking DTLS State")

			state, ok := s.dtlsConn.ConnectionState()
			if !ok {
				return fmt.Errorf("failed to get dtls client state")
			}

			// Setup now SRTP for encryption
			prof, _ := s.dtlsConn.SelectedSRTPProtectionProfile()
			p := srtp.ProtectionProfile(prof)
			masterKeyLen, err := p.KeyLen()
			if err != nil {
				return fmt.Errorf("dtls - failed to get master keylen: %w", err)
			}
			masterSaltLen, err := p.SaltLen()
			if err != nil {
				return fmt.Errorf("dtls - failed to get master saltlen: %w", err)
			}
			keyingMaterial, err := state.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 2*(masterKeyLen+masterSaltLen))
			if err != nil {
				return fmt.Errorf("dtls - failed to export keying material: %w", err)
			}

			clientKey := keyingMaterial[:masterKeyLen]
			serverKey := keyingMaterial[masterKeyLen : 2*masterKeyLen]
			clientSalt := keyingMaterial[2*masterKeyLen : 2*masterKeyLen+masterSaltLen]
			serverSalt := keyingMaterial[2*masterKeyLen+masterSaltLen:]

			if role == "server" {
				// Change order
				clientKey, serverKey = serverKey, clientKey
				clientSalt, serverSalt = serverSalt, clientSalt
			}

			s.localCtxSRTP, err = srtp.CreateContext(clientKey, clientSalt, p)
			if err != nil {
				return fmt.Errorf("failed to create srtp context: %w", err)
			}

			// s.localCtxSRTP, err = srtp.CreateContext(serverKey, serverSalt, p)
			s.remoteCtxSRTP, err = srtp.CreateContext(serverKey, serverSalt, p)
			if err != nil {
				return fmt.Errorf("failed to create srtp context: %w", err)
			}

			if s.localCtxSRTP == nil && s.remoteCtxSRTP == nil {
				return fmt.Errorf("dtls - no srtp context was set up")
			}

			s.isDTLSEstablished = true
			s.logger().Debug("DTLS SRTP setuped")
			return nil
		}
	}

	// Offerer path: we called LocalSDP() before RemoteSDP(), so localCtxSRTP
	// may already be set. If the remote answer has no crypto, clear it to
	// avoid sending encrypted RTP to a peer expecting plaintext.
	if s.localCtxSRTP != nil && s.remoteCtxSRTP == nil && !secureRequest {
		s.localCtxSRTP = nil
	}

	if secureRequest && s.remoteCtxSRTP == nil {
		return fmt.Errorf("remote requested secure rtp, but no context is created proto=%q", md.Proto)
	}
	return nil
}

// Finalize finalizes negotiation and does verification
// Should be called only after exchage of SDP is done
func (s *MediaSession) Finalize() error {
	if s.onFinalize != nil {
		err := s.onFinalize()
		s.onFinalize = nil
		return err
	}
	return nil
}

// codecsMatch compares codecs by their media properties, ignoring payload type.
// Payload type is negotiated per peer and may differ from our local defaults
// (e.g. browsers commonly offer opus at PT 111 instead of 96).
func codecsMatch(a, b Codec) bool {
	return strings.EqualFold(a.Name, b.Name) && a.SampleRate == b.SampleRate && a.NumChannels == b.NumChannels
}

func (s *MediaSession) updateRemoteCodecs(codecs []Codec, answerer bool) int {
	// #39: intersect against usable codecs only, so an opus-only offer
	// fails here with 488 instead of mid-call. Do not mutate s.Codecs:
	// it stays the configured preference; LocalSDP filters on advertise.
	local := filterUnsupportedCodecs(s.Codecs)
	codecs = filterUnsupportedCodecs(codecs)
	if len(local) == 0 {
		// No usable local codec (ex. opus-only config without with_opus_c).
		// Accepting the remote list wholesale would negotiate codecs this
		// build cannot run and fail mid-call; reject instead (callers map this
		// to 488).
		s.filterCodecs = nil
		return 0
	}

	s.logger().Debug("Remote Codecs Update", "local", s.Codecs, "remote", codecs)

	// Some systems may like to answer with local order of preference.
	// Per-session value from MediaConfig.
	preferLocalOrder := s.SDPCodecPreferLocalOrder
	if preferLocalOrder && answerer {
		filter := make([]Codec, 0, len(codecs))
		for _, lc := range local {
			for _, c := range codecs {
				// Match by media properties, not payload type. Remote payload type
				// is adopted so that the answer mirrors the offer (RFC 3264)
				if codecsMatch(lc, c) {
					filter = append(filter, c)
					break
				}
			}
		}
		s.filterCodecs = filter
		return len(s.filterCodecs)
	}

	filter := codecs[:0] // reuse buffer
	for _, rc := range codecs {
		for _, c := range local {
			if codecsMatch(rc, c) {
				filter = append(filter, rc)
				break
			}
		}
	}
	s.filterCodecs = filter
	return len(s.filterCodecs)
}

// CommonCodecs returns common codecs if negotiation is finished, that is Local and Remote SDP are exchanged
// NOTE: Not thread safe, should be called after negotiation Only!
func (s *MediaSession) CommonCodecs() []Codec {
	return s.filterCodecs
}

// DTMFCodecNegotiated resolves the telephone-event codec from the effective
// codec list (negotiated filterCodecs when present, otherwise the local list).
// ok=false means the peer does not support RFC 4733 — callers should fall back
// to inband tones or SIP INFO. It never invents PT 101.
func (s *MediaSession) DTMFCodecNegotiated() (Codec, bool) {
	codecs := s.filterCodecs
	if len(codecs) == 0 {
		codecs = s.Codecs
	}
	for _, c := range codecs {
		if strings.EqualFold(c.Name, CodecTelephoneEvent8000.Name) {
			return c, true
		}
	}
	return Codec{}, false
}

// Listen creates listeners instead
func (s *MediaSession) createListeners(laddr *net.UDPAddr) error {
	// Per-session port range from MediaConfig (zero means ephemeral ports).
	portStart, portEnd := s.RTPPortStart, s.RTPPortEnd

	// var err error

	if laddr.Port != 0 {
		return s.listenRTPandRTCP(laddr)
	}

	if laddr.Port == 0 && portStart > 0 && portEnd > portStart {
		// Get next available port
		port := portStart + int(rtpPortOffset.Load())
		var err error
		for ; port < portEnd; port += 2 {
			laddr.Port = port
			err = s.listenRTPandRTCP(laddr)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("no available ports in range %d:%d: %w", portStart, portEnd, err)
		}
		// Add some offset so that we use more from range
		offset := (port + 2 - portStart) % (portEnd - portStart)
		rtpPortOffset.Store(int32(offset)) // Reset to zero with module
		return nil
	}

	// Because we want to go +2 with ports in racy situations this will always fail
	// So we need to add some control and retry if needed
	// We are always in race with other services so only try to offset to reduce retries
	var err error
	for retries := 0; retries < 10; retries += 1 {
		err = s.listenRTPandRTCP(laddr)
		if err == nil {
			break
		}
	}

	return err
}

func (s *MediaSession) listenRTPandRTCP(laddr *net.UDPAddr) error {
	var err error
	s.rtpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port})
	if err != nil {
		return err
	}
	laddr = s.rtpConn.LocalAddr().(*net.UDPAddr)

	s.rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port + 1})
	if err != nil {
		_ = s.rtpConn.Close()
		return err
	}

	// Update laddr as it can be empheral
	s.Laddr = *laddr
	return nil
}

// ReadRTP reads data from network and parses to pkt
// buffer is passed in order to avoid extra allocs
func (m *MediaSession) ReadRTP(buf []byte, pkt *rtp.Packet) (int, error) {
	if len(buf) < RTPBufSize {
		return 0, io.ErrShortBuffer
	}

	n, from, err := m.rtpConn.ReadFrom(buf)
	if err != nil {
		return 0, err
	}

	if m.remoteCtxSRTP != nil {
		decrypted, err := m.remoteCtxSRTP.DecryptRTP(buf, buf[:n], &pkt.Header)
		if err != nil {
			return n, fmt.Errorf("read srtp decrypt error: %w", err)
		}
		if len(decrypted) > len(buf) {
			m.logger().Warn("Growing Decrypted RTP buffer", "diff", len(decrypted)-len(buf))
		}

		buf = decrypted
		n = len(decrypted)

		// NOTE this is optimiation to avoid double unmarshaling RTP header
		if err := rtpUnmarshalPayload(buf, pkt); err != nil {
			return n, fmt.Errorf("rtp unmarshal failed: %w", err)
		}
	} else {
		if err := RTPUnmarshal(buf[:n], pkt); err != nil {
			return n, err
		}
	}

	logRTPRead(m, from, pkt)
	m.ReadRTPFromAddr = from

	// Handle NAT
	if m.RTPNAT == 1 && !udpAddrEqual(from, &m.Raddr) {
		// Moving this to RTP session could have simplify validation (sequence tracking), but for now here is more easier to maintain
		func() {
			// Make sure it is valid pkt
			if pkt.Version != 2 {
				return
			}

			fromAddr, ok := from.(*net.UDPAddr)
			if !ok {
				return
			}

			if m.learnedRTPFrom.CompareAndSwap(nil, fromAddr) {
				// Allow only first swap, rest is ignored
				m.logger().Debug("RTP NAT switch to new source", "addr", from.String())
			}
		}()
	}

	if m.mode == sdp.ModeSendonly || m.mode == sdp.ModeInactive {
		// We allow parsing of pkt but we indicate that this pkt should not be consumed
		return 0, nil
	}

	return n, err
}

// Deprecated
// Will be replaced with readRTPDeadlineNoAlloc in next releases
// func (m *MediaSession) ReadRTPDeadline(t time.Time) (rtp.Packet, error) {
// 	m.rtpConn.SetReadDeadline(t)
// 	return m.ReadRTP()
// }

func (m *MediaSession) ReadRTPRaw(buf []byte) (int, error) {
	n, from, err := m.rtpConn.ReadFrom(buf)

	if m.RTPNAT == 1 {
		addr, _ := from.(*net.UDPAddr)
		m.learnedRTPFrom.Store(addr)
	}
	return n, err
}

// DiscardQueuedRTP drains datagrams already queued on the RTP socket and
// returns how many were discarded. It exists for the moment a media consumer
// starts (ex. a Bridge about to proxy): without it, whatever arrived while
// nobody was reading is replayed at the line rate and becomes permanent
// one-way latency.
//
// The caller must guarantee that no other component is reading this session
// concurrently - the drain consumes datagrams from the same socket. Packets
// are dropped raw: no SRTP decryption, RTP/RTCP statistics, RTPNAT source
// learning or reader-chain taps observe them. Any read deadline is cleared
// before returning, so normal blocking reads keep working. A nil conn
// (media never initialized) is a no-op.
//
// On a DTLS-SRTP session the drain is a no-op until the handshake completes
// (Finalize returns): DTLS flights share the RTP socket with SRTP, so draining
// before then would eat handshake records. Afterwards the socket carries SRTP
// only.
func (m *MediaSession) DiscardQueuedRTP() (int, error) {
	if m.rtpConn == nil {
		return 0, nil
	}
	if m.SecureRTP == 2 && !m.isDTLSEstablished {
		m.logger().Debug("DiscardQueuedRTP skipped: DTLS handshake not established")
		return 0, nil
	}
	defer func() {
		_ = m.rtpConn.SetReadDeadline(time.Time{})
	}()

	// A short deadline before every read makes queued datagrams return
	// immediately (the syscall succeeds) while an empty socket fails fast
	// with a timeout. The deadline is refreshed per iteration because an
	// already-expired deadline would reject even queued data. The overall cap
	// keeps a peer flooding media from extending the drain indefinitely.
	buf := make([]byte, RTPBufSize)
	discarded := 0
	start := time.Now()
	for {
		if time.Since(start) > rtpDiscardMaxWait {
			return discarded, nil
		}
		if err := m.rtpConn.SetReadDeadline(time.Now().Add(rtpDiscardReadWait)); err != nil {
			return discarded, err
		}
		if _, _, err := m.rtpConn.ReadFrom(buf); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return discarded, nil
			}
			return discarded, err
		}
		discarded++
	}
}

const (
	// rtpDiscardReadWait bounds the blocking wait for the next queued datagram
	// while DiscardQueuedRTP drains a socket. It is only paid once per empty
	// socket, so it stays small.
	rtpDiscardReadWait = 5 * time.Millisecond
	// rtpDiscardMaxWait caps the total drain time against a flooding peer.
	// The kernel receive buffer alone drains orders of magnitude faster.
	rtpDiscardMaxWait = 50 * time.Millisecond
)

// ReadRTCP is optimized reads and unmarshals RTCP packets. Buffers is only used for unmarshaling.
// Caller needs to be aware of size this buffer and allign with MTU
func (m *MediaSession) ReadRTCP(buf []byte, pkts []rtcp.Packet) (n int, err error) {
	nn, from, err := m.ReadRTCPRaw(buf)
	if err != nil {
		return n, err
	}
	data := buf[:nn]

	if m.remoteCtxSRTP != nil {
		data, err = m.remoteCtxSRTP.DecryptRTCP(data, data, nil)
		if err != nil {
			// Dropping the packet and surfacing the error beats feeding ciphertext
			// into unmarshal, which would silently kill RTCP feedback forever
			m.logger().Error("Failed to decrypt SRTCP", "error", err)
			return 0, errors.Join(errRTCPFailedToUnmarshal, err)
		}
	}

	n, err = RTCPUnmarshal(data, pkts)
	if err != nil {
		return 0, err
	}

	if m.RTPNAT == 1 && !udpAddrEqual(from, &m.rtcpRaddr) {
		func() {
			fromAddr, ok := from.(*net.UDPAddr)
			if !ok {
				return
			}
			rtcpA := m.learnedRTCPFrom.Load()
			if rtcpA != nil {
				// learned
				return
			}
			rtpA := m.learnedRTPFrom.Load()

			// Switch RTCP if RTP switched?
			if rtpA != nil {
				m.logger().Debug("RTCP New Source Learned", "addr", from.String())
				m.learnedRTCPFrom.Store(fromAddr)
			}
		}()
	}

	logRTCPRead(m, pkts[:n])
	return n, err
}

func (m *MediaSession) ReadRTCPRaw(buf []byte) (int, net.Addr, error) {
	if m.rtcpConn == nil {
		// just block
		return 0, nil, fmt.Errorf("no connection present")
	}
	n, a, err := m.rtcpConn.ReadFrom(buf)
	return n, a, err
}

func (m *MediaSession) WriteRTP(p *rtp.Packet) error {
	if m.writeBlockedByDirection() {
		// We block here as we would violate our media direction
		return nil
	}

	// A pooled per-call buffer instead of a session-wide one: concurrent
	// WriteRTP calls (user IO racing a pipeline component) would otherwise
	// marshal over each other's packet bytes. Steady state hits the pool, so
	// the hot path stays allocation free.
	writeBufPtr := rtpBufPool.Get().(*[]byte)
	defer rtpBufPool.Put(writeBufPtr)
	writeBuf := *writeBufPtr

	n, err := p.MarshalTo(writeBuf)
	if err != nil {
		return fmt.Errorf("failed to marshal to write rtp buf: %w", err)
	}
	data := writeBuf[:n]

	if m.localCtxSRTP != nil {
		data, err = m.localCtxSRTP.EncryptRTP(writeBuf, data, &p.Header)
		if err != nil {
			return fmt.Errorf("failed to encrypt written rtp: %w", err)
		}
	}

	n, err = m.WriteRTPRaw(data)
	if err != nil {
		return err
	}

	if n != len(data) {
		return io.ErrShortWrite
	}

	logRTPWrite(m, p)
	return nil
}

func (m *MediaSession) WriteRTPRaw(data []byte) (n int, err error) {
	addr := &m.Raddr
	if m.RTPNAT == 1 {
		if a := m.learnedRTPFrom.Load(); a != nil {
			addr = a
		}
	}

	n, err = m.rtpConn.WriteTo(data, addr)
	return
}

func (m *MediaSession) WriteRTCP(p rtcp.Packet) error {
	logRTCPWrite(m, p)

	// TODO: rtcp library needs to support MarshalTo v2
	// https://github.com/pion/rtcp/issues/127
	data, err := p.Marshal()
	if err != nil {
		return err
	}

	if m.localCtxSRTP != nil {
		// sync pool may not be best option and needs benchmarks.
		// but as RTCP is not realtime this can reduce allocations
		// TODO: should be reusable with above marshaling
		wbuf := rtpBufPool.Get().(*[]byte)
		defer rtpBufPool.Put(wbuf)
		writeBuf := *wbuf

		data, err = m.localCtxSRTP.EncryptRTCP(writeBuf, data, nil)
		if err != nil {
			return err
		}
	}

	n, err := m.WriteRTCPRaw(data)
	if err != nil {
		return err
	}

	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (m *MediaSession) WriteRTCPRaw(data []byte) (int, error) {
	addr := &m.rtcpRaddr
	if m.RTPNAT == 1 {
		if a := m.learnedRTCPFrom.Load(); a != nil {
			addr = a
		}
	}

	n, err := m.rtcpConn.WriteTo(data, addr)
	return n, err
}

func StringRTCP(p rtcp.Packet) string {

	switch r := p.(type) {
	case *rtcp.SenderReport:
		h := r.Header()
		var out strings.Builder
		fmt.Fprintf(&out, "SenderReport from %x\n", r.SSRC)
		fmt.Fprintf(&out, "\tHeader: Count:%d Length:%d Type:%d\n", h.Count, h.Length, h.Type)
		fmt.Fprintf(&out, "\tNTPTime:\t%d\n", r.NTPTime)
		fmt.Fprintf(&out, "\tRTPTIme:\t%d\n", r.RTPTime)
		fmt.Fprintf(&out, "\tPacketCount:\t%d\n", r.PacketCount)
		fmt.Fprintf(&out, "\tOctetCount:\t%d\n", r.OctetCount)

		for _, i := range r.Reports {
			fmt.Fprintf(&out, "\tSSRC: %x Lost: %d/%d  LastSeq: %d LSR: %d.%d DLSR: %d\n", i.SSRC, i.FractionLost, i.TotalLost, i.LastSequenceNumber, i.LastSenderReport&0xFFFF0000, i.LastSenderReport&0x0000FFFF, i.Delay)
		}
		fmt.Fprintf(&out, "\tProfile Extension Data: %v\n", r.ProfileExtensions)

		return out.String()
	case *rtcp.ReceiverReport:
		h := r.Header()
		var out strings.Builder
		fmt.Fprintf(&out, "ReceiverReport from %x\n", r.SSRC)
		fmt.Fprintf(&out, "\tHeader: Count:%d Length:%d Type:%d\n", h.Count, h.Length, h.Type)
		for _, i := range r.Reports {
			fmt.Fprintf(&out, "SSRC: %x\tLost: %d/%d\t LastSeq: %d\tLSR: %d.%d\tDLSR: %d\n", i.SSRC, i.FractionLost, i.TotalLost, i.LastSequenceNumber, i.LastSenderReport&0xFFFF0000, i.LastSenderReport&0x0000FFFF, i.Delay)
		}
		fmt.Fprintf(&out, "\tProfile Extension Data: %v\n", r.ProfileExtensions)
		return out.String()
	}

	if s, ok := p.(fmt.Stringer); ok {
		return s.String()
	}
	return "can not stringify"
}

type sdesInline struct {
	alg    string
	base64 string
	tag    int
}

type sdpFingerprints struct {
	fingerprint string
	alg         string
}

type dtlsSetup struct {
	setup        string
	fingerprints []sdpFingerprints
}

func generateSDPForAudio(sessionID uint64, sessionVersion uint64, rtpProfile string, originIP net.IP, connectionIP net.IP, rtpPort int, mode string, codecs []Codec, sdes sdesInline, dtlsSet *dtlsSetup, sessionName string, log *slog.Logger) []byte {
	// ntpTime := CurrentNTPTimestamp()
	if len(codecs) == 0 {
		// Nothing usable to advertise (ex. opus-only configuration without
		// with_opus_c). RFC 4566 requires at least one fmt; emit a rejected
		// m= line (port 0) instead of "m=audio <port> RTP/AVP " with none.
		log.Error("no usable codecs to advertise; SDP marks the media stream as rejected")
		rtpPort = 0
		codecs = []Codec{CodecAudioUlaw}
	}

	fmts := make([]string, len(codecs))
	formatsMap := []string{}
	for i, f := range codecs {
		// TODO should we just go generic
		switch {
		case f.PayloadType == CodecAudioUlaw.PayloadType && f.Name == CodecAudioUlaw.Name:
			formatsMap = append(formatsMap, "a=rtpmap:0 PCMU/8000")
		case f.PayloadType == CodecAudioAlaw.PayloadType && f.Name == CodecAudioAlaw.Name:
			formatsMap = append(formatsMap, "a=rtpmap:8 PCMA/8000")
		case strings.EqualFold(f.Name, CodecAudioOpus.Name):
			formatsMap = append(formatsMap, fmt.Sprintf("a=rtpmap:%d opus/%d/%d", f.PayloadType, f.SampleRate, f.NumChannels))
			// Providing 0 when FEC cannot be used on the receiving side is RECOMMENDED.
			// https://datatracker.ietf.org/doc/html/rfc7587
			formatsMap = append(formatsMap, fmt.Sprintf("a=fmtp:%d useinbandfec=0", f.PayloadType))
		case strings.EqualFold(f.Name, CodecTelephoneEvent8000.Name):
			formatsMap = append(formatsMap, fmt.Sprintf("a=rtpmap:%d telephone-event/%d", f.PayloadType, f.SampleRate))
			formatsMap = append(formatsMap, fmt.Sprintf("a=fmtp:%d 0-16", f.PayloadType))
		default:
			s := fmt.Sprintf("a=rtpmap:%d %s/%d/%d", f.PayloadType, f.Name, f.SampleRate, f.NumChannels)
			formatsMap = append(formatsMap, s)
		}
		fmts[i] = strconv.Itoa(int(f.PayloadType))
	}

	// Support only ulaw and alaw
	// TODO optimize this with string builder
	s := []string{
		"v=0",
		fmt.Sprintf("o=- %d %d IN %s %s", sessionID, sessionVersion, sdpIP(originIP), originIP),
		fmt.Sprintf("s=%s", sessionName),
		// "b=AS:84",
		fmt.Sprintf("c=IN %s %s", sdpIP(connectionIP), connectionIP),
		"t=0 0",
		fmt.Sprintf("m=audio %d %s %s", rtpPort, rtpProfile, strings.Join(fmts, " ")),
	}

	s = append(s, formatsMap...)
	s = append(s,
		"a=ptime:20", // Needed for opus
		"a=maxptime:20",
		"a="+string(mode))

	if sdes.alg != "" {
		s = append(s, fmt.Sprintf("a=crypto:%d %s inline:%s", sdes.tag, sdes.alg, sdes.base64))
	}

	if dtlsSet != nil {
		fingerprints := dtlsSet.fingerprints
		dtlsSetup := dtlsSet.setup

		s = append(s, "a=setup:"+dtlsSetup)
		// NOTE: RFC 5763 §5 forbids the RFC 4145 "a=connection" attribute for
		// DTLS-SRTP (an association is reused based on the fingerprints, not
		// signalled as existing), so it is deliberately not emitted here.
		for _, d := range fingerprints {
			if d.fingerprint == "" {
				continue
			}
			s = append(s, fmt.Sprintf("a=fingerprint:%s %s", d.alg, d.fingerprint))
		}
	}

	// s := []string{
	// 	"v=0",
	// 	fmt.Sprintf("o=- %d %d IN IP4 %s", ntpTime, ntpTime, originIP),
	// 	"s=Sip Go Media",
	// 	// "b=AS:84",
	// 	fmt.Sprintf("c=IN IP4 %s", connectionIP),
	// 	"t=0 0",
	// 	fmt.Sprintf("m=audio %d RTP/AVP 96 97 98 99 3 0 8 9 120 121 122", rtpPort),
	// 	"a=" + string(mode),
	// 	"a=rtpmap:96 speex/16000",
	// 	"a=rtpmap:97 speex/8000",
	// 	"a=rtpmap:98 speex/32000",
	// 	"a=rtpmap:99 iLBC/8000",
	// 	"a=fmtp:99 mode=30",
	// 	"a=rtpmap:120 telephone-event/16000",
	// 	"a=fmtp:120 0-16",
	// 	"a=rtpmap:121 telephone-event/8000",
	// 	"a=fmtp:121 0-16",
	// 	"a=rtpmap:122 telephone-event/32000",
	// 	"a=rtcp-mux",
	// 	fmt.Sprintf("a=rtcp:%d IN IP4 %s", rtpPort+1, connectionIP),
	// }

	res := strings.Join(s, "\r\n") + "\r\n"
	return []byte(res)
}

func generateMasterKeySalt(profile srtp.ProtectionProfile) ([]byte, int, error) {
	keyLen, err := profile.KeyLen()
	if err != nil {
		return nil, 0, fmt.Errorf("srtp getting key len: %w", err)
	}

	saltLen, err := profile.SaltLen()
	if err != nil {
		return nil, 0, fmt.Errorf("srtp getting salt len: %w", err)
	}

	buf := make([]byte, keyLen+saltLen)
	if _, err := rand.Read(buf[:keyLen]); err != nil {
		return nil, 0, err
	}

	if _, err := rand.Read(buf[keyLen:]); err != nil {
		return nil, 0, err
	}
	return buf, keyLen, nil
}

func sdpIP(ip net.IP) string {
	if ip.To4() == nil {
		return "IP6"
	}
	return "IP4"
}

// udpAddrEqual compares without the string allocations of Addr.String() on
// the per-packet NAT check path.
func udpAddrEqual(a net.Addr, b *net.UDPAddr) bool {
	ua, ok := a.(*net.UDPAddr)
	if !ok {
		return false
	}
	return ua.IP.Equal(b.IP) && ua.Port == b.Port && ua.Zone == b.Zone
}

// negotiateMediaDirection computes our local direction based on the remote SDP offer/answer
// and our current preference. Defaults to sendrecv when nothing explicit is provided.
func negotiateMediaDirection(remoteMode, localPref string) string {
	if localPref == "" {
		localPref = sdp.ModeSendrecv
	}

	switch remoteMode {
	case "inactive":
		return "inactive"
	case sdp.ModeSendonly:
		if localPref == "inactive" {
			return "inactive"
		}
		// Offerer is sendonly, answer must be recvonly or inactive
		return sdp.ModeRecvonly
	case sdp.ModeRecvonly:
		if localPref == "inactive" {
			return "inactive"
		}
		// Offerer is recvonly, answer must be sendonly or inactive
		return sdp.ModeSendonly
	case sdp.ModeSendrecv:
		if localPref == sdp.ModeSendrecv || localPref == sdp.ModeSendonly || localPref == sdp.ModeRecvonly || localPref == "inactive" {
			return localPref
		}
		return sdp.ModeSendrecv
	default:
		return localPref
	}
}
