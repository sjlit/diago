// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"io"
)

type OnRTPReadStats func(stats RTPReadStats)
type OnRTPWriteStats func(stats RTPWriteStats)

// RTPStatsReader reports RTP read statistics for every successfully read
// packet.
type RTPStatsReader struct {
	// Reader should be your AudioReade or any other interceptor RTP reader that is reading audio stream
	Reader io.Reader
	// RTPSession is the statistics source. It is ignored when Session is set.
	RTPSession *RTPSession
	// Session, when set, resolves the statistics source at read time and takes
	// precedence over RTPSession. Use it for pipelines that survive media
	// updates: a re-INVITE swaps the dialog's RTP session without replacing
	// the component holding this interceptor.
	Session func() *RTPSession
	// OnRTPReadStats is fired each time on Read RTP. Must not block
	OnRTPReadStats OnRTPReadStats
}

func (i *RTPStatsReader) session() *RTPSession {
	if i.Session != nil {
		return i.Session()
	}
	return i.RTPSession
}

func (i *RTPStatsReader) Read(b []byte) (int, error) {
	n, err := i.Reader.Read(b)
	if err != nil {
		return n, err
	}

	if sess := i.session(); sess != nil && i.OnRTPReadStats != nil {
		i.OnRTPReadStats(sess.ReadStats())
	}
	return n, err
}

// RTPStatsWriter reports RTP write statistics for every successfully written
// packet.
type RTPStatsWriter struct {
	// Writer should be your Writer or any other interceptor RTP writer that is reading audio stream
	Writer io.Writer
	// RTPSession is the statistics source. It is ignored when Session is set.
	RTPSession *RTPSession
	// Session, when set, resolves the statistics source at write time and
	// takes precedence over RTPSession.
	Session func() *RTPSession
	// OnRTPWriteStats is fired each time on Read RTP. Must not block
	OnRTPWriteStats OnRTPWriteStats
}

func (i *RTPStatsWriter) session() *RTPSession {
	if i.Session != nil {
		return i.Session()
	}
	return i.RTPSession
}

func (i *RTPStatsWriter) Write(p []byte) (int, error) {
	n, err := i.Writer.Write(p)
	if err != nil {
		return n, err
	}

	if sess := i.session(); sess != nil && i.OnRTPWriteStats != nil {
		i.OnRTPWriteStats(sess.WriteStats())
	}
	return n, err
}
