// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
)

var rtpBufPool = sync.Pool{
	New: func() any {
		return make([]byte, media.RTPBufSize)
	},
}

func copyWithBuf(reader io.Reader, writer io.Writer, payloadBuf []byte) (int64, error) {
	return media.CopyWithBuf(reader, writer, payloadBuf)
}

func closeAndLog(log *slog.Logger, closer io.Closer, msg string) {
	if err := closer.Close(); err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.Error(msg, "error", err)
	}
}

type loggingTransport struct{}

func (s *loggingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	reqBytes, _ := httputil.DumpRequestOut(r, false)

	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		slog.Debug("HTTP Debug", "request", string(reqBytes), "error", err)
		return nil, err
	}

	respBytes, _ := httputil.DumpResponse(resp, false)

	slog.Debug("HTTP Debug", "request", string(reqBytes), "response", string(respBytes))

	return resp, nil
}

func uri2Header(uri sip.Uri) string {
	buf := &strings.Builder{}
	buf.WriteString("<")
	uri.StringWrite(buf)
	buf.WriteString(">")
	return buf.String()
}
