// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// CallState is the signaling state of a call leg.
type CallState uint8

const (
	// CallStateNew is a call whose INVITE has not been answered yet.
	CallStateNew CallState = iota
	// CallStateEarly is an early dialog (provisional response with SDP).
	CallStateEarly
	// CallStateAnswered means the final 2xx was sent/received but its ACK has
	// not been processed yet.
	CallStateAnswered
	// CallStateConfirmed means the ACK was processed and the dialog is
	// established.
	CallStateConfirmed
	// CallStateEnded means the dialog terminated.
	CallStateEnded
)

func (s CallState) String() string {
	switch s {
	case CallStateNew:
		return "new"
	case CallStateEarly:
		return "early"
	case CallStateAnswered:
		return "answered"
	case CallStateConfirmed:
		return "confirmed"
	case CallStateEnded:
		return "ended"
	default:
		return "unknown"
	}
}

func callStateFromDialog(d *sipgo.Dialog) CallState {
	if d == nil {
		return CallStateEnded
	}
	if d.InviteResponse != nil && d.InviteResponse.StatusCode < 200 {
		return CallStateEarly
	}
	switch d.LoadState() {
	case sip.DialogStateEstablished:
		return CallStateAnswered
	case sip.DialogStateConfirmed:
		return CallStateConfirmed
	case sip.DialogStateEnded:
		return CallStateEnded
	default:
		return CallStateNew
	}
}

// State returns the call's signaling state.
func (d *InboundCall) State() CallState {
	if d == nil || d.sess == nil {
		return CallStateEnded
	}
	return callStateFromDialog(&d.sess.Dialog)
}

// State returns the call's signaling state.
func (d *OutboundCall) State() CallState {
	if d == nil || d.sess == nil {
		return CallStateEnded
	}
	return callStateFromDialog(&d.sess.Dialog)
}
