// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// parseDTMFRelay parses application/dtmf-relay body (RFC via common practice,
// e.g. from Asterisk or WebRTC gateways):
//
//	Signal=8
//	Duration=120
//
// Keys are case-insensitive, lines may end with \r\n. Signal accepts 0-9, *,
// #, a-d (lower and upper case).
func parseDTMFRelay(body []byte) (dtmf rune, dur time.Duration, err error) {
	var signal string
	for _, line := range bytes.Split(body, []byte("\n")) {
		key, val, found := cutRelayField(line)
		if !found {
			continue
		}
		switch key {
		case "signal":
			signal = val
		case "duration":
			ms, convErr := strconv.Atoi(val)
			if convErr != nil || ms < 0 {
				return 0, 0, fmt.Errorf("invalid DTMF duration value=%q", val)
			}
			dur = time.Duration(ms) * time.Millisecond
		default:
			// Ignore unknown fields (some senders add extra ones)
		}
	}

	if signal == "" {
		return 0, 0, fmt.Errorf("DTMF signal missing")
	}
	r, ok := dtmfRelaySignal(signal)
	if !ok {
		return 0, 0, fmt.Errorf("invalid DTMF signal=%q", signal)
	}
	return r, dur, nil
}

// cutRelayField splits "Key=Value" trimming spaces and trailing \r
func cutRelayField(line []byte) (key string, val string, found bool) {
	line = bytes.TrimSpace(line)
	k, v, ok := bytes.Cut(line, []byte("="))
	if !ok {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(string(k))), strings.TrimSpace(string(v)), true
}

func dtmfRelaySignal(signal string) (rune, bool) {
	if len(signal) != 1 {
		return 0, false
	}
	r := rune(signal[0])
	switch {
	case r >= '0' && r <= '9', r == '*', r == '#':
		return r, true
	case r >= 'A' && r <= 'D':
		return r + ('a' - 'A'), true
	case r >= 'a' && r <= 'd':
		return r, true
	}
	return 0, false
}

// readSIPInfoDTMF handles an in-dialog INFO with application/dtmf-relay body.
// It is shared by server and client dialog sessions: valid DTMF is answered
// 200 OK and delivered through the same callbacks used for RTP DTMF
// (DTMFReader listeners, AudioPlaybackDTMF channel).
func readSIPInfoDTMF(media *DialogMedia, req *sip.Request, tx sip.ServerTransaction) error {
	dtmf, dur, err := parseDTMFRelay(req.Body())
	if err != nil {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
	}

	if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
		return err
	}
	media.deliverDTMF(dtmf, dur)
	return nil
}
