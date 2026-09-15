// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// shortWriter accepts at most limit bytes per Write and reports that as a
// successful short write (io.Writer contract violation), which the copy
// helpers must detect instead of silently dropping the remainder.
type shortWriter struct {
	limit int
	buf   bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.limit >= 0 && len(p) > w.limit {
		n, _ := w.buf.Write(p[:w.limit])
		return n, nil
	}
	return w.buf.Write(p)
}

// zeroReader returns one empty read with a nil error before the underlying
// reader produces data, mimicking RTPPacketReader's "packet not consumed"
// path.
type zeroReader struct {
	once bool
	r    io.Reader
}

func (r *zeroReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		return 0, nil
	}
	return r.r.Read(p)
}

func TestCopyWithBufShortWrite(t *testing.T) {
	data := bytes.Repeat([]byte{1}, 100)

	t.Run("full write", func(t *testing.T) {
		w := &shortWriter{limit: -1}
		n, err := CopyWithBuf(bytes.NewReader(data), w, make([]byte, RTPBufSize))
		require.ErrorIs(t, err, io.EOF)
		require.EqualValues(t, len(data), n)
		require.Equal(t, data, w.buf.Bytes())
	})

	t.Run("short write is detected", func(t *testing.T) {
		w := &shortWriter{limit: 50}
		n, err := CopyWithBuf(bytes.NewReader(data), w, make([]byte, RTPBufSize))
		require.ErrorIs(t, err, io.ErrShortWrite)
		require.EqualValues(t, 50, n)
		require.Len(t, w.buf.Bytes(), 50)
	})

	t.Run("empty read with nil error continues", func(t *testing.T) {
		w := &shortWriter{limit: -1}
		n, err := CopyWithBuf(&zeroReader{r: bytes.NewReader(data)}, w, make([]byte, RTPBufSize))
		require.ErrorIs(t, err, io.EOF)
		require.EqualValues(t, len(data), n)
		require.Equal(t, data, w.buf.Bytes())
	})
}

func TestCopyWithBufContextShortWrite(t *testing.T) {
	data := bytes.Repeat([]byte{2}, 100)
	ctx := context.Background()

	t.Run("full write", func(t *testing.T) {
		w := &shortWriter{limit: -1}
		n, err := CopyWithBufContext(ctx, bytes.NewReader(data), w, make([]byte, RTPBufSize))
		require.ErrorIs(t, err, io.EOF)
		require.EqualValues(t, len(data), n)
		require.Equal(t, data, w.buf.Bytes())
	})

	t.Run("short write is detected", func(t *testing.T) {
		w := &shortWriter{limit: 50}
		n, err := CopyWithBufContext(ctx, bytes.NewReader(data), w, make([]byte, RTPBufSize))
		require.ErrorIs(t, err, io.ErrShortWrite)
		require.EqualValues(t, 50, n)
		require.Len(t, w.buf.Bytes(), 50)
	})

	t.Run("canceled context wins over read error", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		w := &shortWriter{limit: -1}
		n, err := CopyWithBufContext(canceled, bytes.NewReader(data), w, make([]byte, RTPBufSize))
		require.ErrorIs(t, err, context.Canceled)
		require.EqualValues(t, 0, n)
		require.Empty(t, w.buf.Bytes())
	})
}
