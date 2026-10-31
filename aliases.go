// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Aliases for the sipgo types used in diago's public API. They let
// applications use diago without importing sipgo directly; because they are
// type aliases, sipgo values remain assignable in both directions, so advanced
// code can keep mixing the underlying types. Use the explicit escape hatches
// (Config.UserAgent, Config.Server, Config.Client, Agent.Server, call.SIP)
// when the alias surface is not enough.
type (
	// URI is a parsed SIP URI (alias of sip.Uri).
	URI = sip.Uri
	// Header is a SIP header (alias of sip.Header).
	Header = sip.Header
	// ContactHeader is a SIP Contact header (alias of sip.ContactHeader).
	ContactHeader = sip.ContactHeader
	// Request is a SIP request (alias of sip.Request).
	Request = sip.Request
	// Response is a SIP response (alias of sip.Response).
	Response = sip.Response
	// ServerTransaction is a server-side SIP transaction (alias of
	// sip.ServerTransaction).
	ServerTransaction = sip.ServerTransaction
	// RequestMethod is a SIP method (alias of sip.RequestMethod).
	RequestMethod = sip.RequestMethod
	// RequestHandler is the sipgo server request handler signature.
	RequestHandler = sipgo.RequestHandler
	// UserAgent is the sipgo transport/transaction user agent.
	UserAgent = sipgo.UserAgent
	// Client is the sipgo client handle.
	Client = sipgo.Client
	// Server is the sipgo server handle.
	Server = sipgo.Server
	// Dialog is the sipgo dialog state shared by inbound and outbound calls.
	Dialog = sipgo.Dialog
)
