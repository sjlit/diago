// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package sdp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

var bufReader = sync.Pool{
	New: func() interface{} {
		// The Pool's New function should generally only return pointer
		// types, since a pointer can be put into the return interface
		// value without an allocation:
		return new(bytes.Buffer)
	},
}

const (
	// https://datatracker.ietf.org/doc/html/rfc4566#section-6
	ModeRecvonly string = "recvonly"
	ModeSendrecv string = "sendrecv"
	ModeSendonly string = "sendonly"
	ModeInactive string = "inactive"
)

// cSectionKey records, for every parsed c= line, how many m= lines preceded
// it. SDP types are a single character and Unmarshal enforces that, so parsed
// input can never collide with this key. Descriptions built manually without
// it fall back to session-level-only connection lookups.
const cSectionKey = "c_sections"

// aSectionKey does the same for a= lines so direction attributes can be
// resolved against the media section they belong to (session level is 0).
const aSectionKey = "a_sections"

type SessionDescription map[string][]string

func (sd SessionDescription) Values(key string) []string {
	return sd[key]
}

func (sd SessionDescription) Value(key string) string {
	values := sd[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// MediaDescription represents a media type.
// m=<media> <port>/<number of ports> <proto> <fmt> ...
// https://tools.ietf.org/html/rfc4566#section-5.14
type MediaDescription struct {
	MediaType string

	Port        int
	PortNumbers int

	Proto string

	Formats []string
}

func (m *MediaDescription) String() string {
	ports := strconv.Itoa(m.Port)
	if m.PortNumbers > 0 {
		ports += "/" + strconv.Itoa(m.PortNumbers)
	}

	return fmt.Sprintf("m=%s %s %s %s", m.MediaType, ports, m.Proto, strings.Join(m.Formats, " "))
}

func (sd SessionDescription) MediaDescription(mediaType string) (MediaDescription, error) {
	md, _, err := sd.mediaDescriptionIdx(mediaType)
	return md, err
}

// mediaDescriptionIdx returns the first media description of the given type
// together with its index among all m= lines (its section number). SDPs with
// several m= lines (audio + video, audio + image, ...) are accepted: the
// section of the requested type is selected and the rest ignored. Multiple
// lines of the requested type select the first one.
func (sd SessionDescription) mediaDescriptionIdx(mediaType string) (MediaDescription, int, error) {
	values := sd.Values("m")
	md := MediaDescription{}

	var v string
	idx := -1
	for i, val := range values {
		ind := strings.Index(val, " ")
		if ind < 1 {
			continue
		}
		media := val[:ind]
		if media == mediaType {
			v = val
			idx = i
			break
		}
	}

	if v == "" {
		return md, idx, fmt.Errorf("Media not found for %q", mediaType)
	}

	fields := strings.Fields(v)
	// RFC 4566 section 5.7: m=<media> <port> <proto> <fmt> - all four
	// fields are mandatory
	if len(fields) < 4 {
		return md, idx, fmt.Errorf("Not enough fields in media description")
	}

	md.MediaType = fields[0]

	ports := strings.Split(fields[1], "/")
	md.Port, _ = strconv.Atoi(ports[0])
	if len(ports) > 1 {
		md.PortNumbers, _ = strconv.Atoi(ports[1])
	}

	md.Proto = fields[2]

	md.Formats = fields[3:]
	return md, idx, nil
}

// c=<nettype> <addrtype> <connection-address>
// https://tools.ietf.org/html/rfc4566#section-5.7
type ConnectionInformation struct {
	NetworkType string
	AddressType string
	IP          net.IP
	TTL         int
	Range       int
}

func (sd SessionDescription) ConnectionInformation() (ci ConnectionInformation, err error) {
	v := sd.Value("c")
	if v == "" {
		return ci, fmt.Errorf("Connection information does not exists")
	}
	return parseConnectionInformation(v)
}

// ConnectionInformationFor returns the connection information that applies to
// the first media description of the given type: the media-level c= line of
// that media section when present (RFC 4566 §5.7: it overrides the session
// level), otherwise the session-level c=. It fails when neither exists - an
// SDP without session-level c= must carry one per media section.
func (sd SessionDescription) ConnectionInformationFor(mediaType string) (ci ConnectionInformation, err error) {
	_, idx, err := sd.mediaDescriptionIdx(mediaType)
	if err != nil {
		return ci, err
	}

	sections := sd.Values(cSectionKey)
	if len(sections) == 0 {
		// Manually built description: only session-level lookup is possible.
		return sd.ConnectionInformation()
	}

	values := sd.Values("c")
	// Media-level c= of this section: recorded after idx+1 m= lines were seen.
	for i, s := range sections {
		if i >= len(values) {
			break
		}
		if n, _ := strconv.Atoi(s); n == idx+1 {
			return parseConnectionInformation(values[i])
		}
	}
	// Session-level c=: recorded before any m= line.
	for i, s := range sections {
		if i >= len(values) {
			break
		}
		if n, _ := strconv.Atoi(s); n == 0 {
			return parseConnectionInformation(values[i])
		}
	}
	return ci, fmt.Errorf("Connection information does not exists for media %q", mediaType)
}

func parseConnectionInformation(v string) (ci ConnectionInformation, err error) {
	fields := strings.Fields(v)
	if len(fields) < 3 {
		return ci, fmt.Errorf("sdp - malformed connection line c=%s", v)
	}
	ci.NetworkType = fields[0]
	ci.AddressType = fields[1]
	addr := strings.Split(fields[2], "/")
	ci.IP = net.ParseIP(addr[0])

	switch ci.AddressType {
	case "IP4":
		ci.IP = ci.IP.To4()
		if ci.IP == nil {
			return ci, fmt.Errorf("sdp - failed to convert to IP4 c=%s", v)
		}
	case "IP6":
		ci.IP = ci.IP.To16()
		if ci.IP == nil {
			return ci, fmt.Errorf("sdp - failed to convert to IP6 c=%s", v)
		}
	}

	if len(addr) > 1 {
		ci.TTL, _ = strconv.Atoi(addr[1])
	}

	if len(addr) > 2 {
		ci.Range, _ = strconv.Atoi(addr[2])
	}
	return ci, nil
}

type SessionInformation struct {
	Origin         string
	SessionID      uint64
	SessionVersion uint64
	NetworkType    string
	AddressType    string
	Address        string // Informational address
}

func (sd SessionDescription) SessionInformation() (i SessionInformation, err error) {
	v := sd.Value("o")
	if v == "" {
		return i, fmt.Errorf("Connection information does not exists")
	}
	fields := strings.Fields(v)
	if len(fields) < 6 {
		return i, fmt.Errorf("Not enough session fields")
	}
	i.Origin = fields[0]
	sessId, sessVersion := fields[1], fields[2]
	i.SessionID, err = strconv.ParseUint(sessId, 10, 64)
	if err != nil {
		return i, err
	}
	i.SessionVersion, err = strconv.ParseUint(sessVersion, 10, 64)
	if err != nil {
		return i, err
	}
	i.NetworkType = fields[3]
	i.AddressType = fields[4]
	i.Address = fields[5]
	return i, nil
}

func (sd SessionDescription) MediaDirection() string {
	attrs := sd.Values("a")
	if attrs == nil {
		// Assume default per
		return ModeSendrecv
	}
	mode := ModeSendrecv
	for _, v := range attrs {
		if m := directionAttr(v); m != "" {
			mode = m
		}
	}
	return mode
}

// MediaDirectionFor returns the direction that applies to the first media
// description of the given type. A session-level direction applies to every
// section; a media-level direction overrides it (RFC 4566 §6). Descriptions
// built manually (no section bookkeeping) fall back to MediaDirection, whose
// last-wins scan cannot distinguish sections.
func (sd SessionDescription) MediaDirectionFor(mediaType string) string {
	_, idx, err := sd.mediaDescriptionIdx(mediaType)
	if err != nil {
		return sd.MediaDirection()
	}

	sections := sd.Values(aSectionKey)
	attrs := sd.Values("a")
	if len(sections) == 0 || len(sections) != len(attrs) {
		return sd.MediaDirection()
	}

	sessionMode, mediaMode := "", ""
	for i, section := range sections {
		sectionNum, err := strconv.Atoi(section)
		if err != nil {
			continue
		}
		if sectionNum != 0 && sectionNum != idx+1 {
			continue
		}
		mode := directionAttr(attrs[i])
		if mode == "" {
			continue
		}
		if sectionNum == 0 {
			sessionMode = mode
		} else {
			mediaMode = mode
		}
	}

	if mediaMode != "" {
		return mediaMode
	}
	if sessionMode != "" {
		return sessionMode
	}
	return ModeSendrecv
}

func directionAttr(v string) string {
	switch v {
	case ModeSendrecv, ModeSendonly, ModeRecvonly, ModeInactive:
		return v
	}
	return ""
}

// Unmarshal is non validate version of sdp parsing
// Validation of values needs to be seperate
// NOT OPTIMIZED
func Unmarshal(data []byte, sdptr *SessionDescription) error {
	reader := bufReader.Get().(*bytes.Buffer)
	defer bufReader.Put(reader)
	reader.Reset()
	reader.Write(data)

	sd := *sdptr
	// mCount is the number of m= lines seen so far; it is the section number
	// of the line being processed (0 = session level, before any m= line).
	mCount := 0
	for {
		line, err := nextLine(reader)
		if err == io.EOF && len(line) == 0 {
			return nil
		}

		if len(line) >= 2 {
			ind := strings.Index(line, "=")
			if ind < 1 {
				return fmt.Errorf("Not a type=value line found. line=%q", line)
			}
			key := line[:ind]
			value := line[ind+1:]
			// RFC 4566: <type>=<value>, type is a single character. Enforcing
			// it also keeps parsed input from forging the internal
			// section-tracking keys (c_sections/a_sections).
			if len(key) != 1 {
				return fmt.Errorf("sdp: type must be a single character, got %q", key)
			}

			sd[key] = append(sd[key], value)
			switch key {
			case "m":
				mCount++
			case "c":
				// Remember in which section this c= line appeared, so
				// media-scoped connection lines can be resolved.
				sd[cSectionKey] = append(sd[cSectionKey], strconv.Itoa(mCount))
			case "a":
				// Same bookkeeping for direction attributes: a media update
				// must not let another section's a=sendonly/recvonly leak in.
				sd[aSectionKey] = append(sd[aSectionKey], strconv.Itoa(mCount))
			}
		}

		if err != nil {
			if err == io.EOF {
				// Last line was pending without newline terminator, it is processed above
				return nil
			}
			return err
		}
	}

}

func nextLine(reader *bytes.Buffer) (line string, err error) {
	// Scan full line without buffer
	// If we need to continue then try to grow
	line, err = reader.ReadString('\n')
	if err != nil {
		// We may get io.EOF and line till it was read
		return line, err
	}

	lenline := len(line)

	// Be tolerant for CRLF. Line always contains at least the delimiter here,
	// so never index below zero on blank lines
	if lenline >= 2 && line[lenline-2] == '\r' {
		return line[:lenline-2], nil
	}

	return line[:lenline-1], nil
}
