// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"io"
	"log/slog"
	"strings"
	"sync"

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

func uri2Header(uri URI) string {
	buf := &strings.Builder{}
	buf.WriteString("<")
	uri.StringWrite(buf)
	buf.WriteString(">")
	return buf.String()
}
