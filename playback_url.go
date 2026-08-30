// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

// Copyright (C) 2024 Emir Aganovic

package diago

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	DefaultPlaybackURLRangeSize int = 65536
)

// PlayURL plays wav content from url.
//
// Deprecated: Use PlayURLContext.
func (p *AudioPlayback) PlayURL(urlStr string) (int64, error) {
	return p.PlayURLContext(context.Background(), urlStr)
}

// PlayURLContext plays wav content from url. The context bounds the request
// and the streaming; unlike the deprecated PlayURL there is no implicit
// 10s deadline when the caller provides one.
func (p *AudioPlayback) PlayURLContext(ctx context.Context, urlStr string) (int64, error) {
	var written int64
	err := p.playURL(ctx, urlStr, &written)
	if errors.Is(err, io.EOF) {
		return written, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return written, ctxErr
	}
	return written, err
}

func (p *AudioPlayback) playURL(ctx context.Context, urlStr string, written *int64) error {
	// Legacy behavior: without a caller deadline keep a 10s cap for the
	// initial request (chunked range requests stream under the same ctx).
	requestCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(requestCtx, "GET", urlStr, nil)
	if err != nil {
		return err
	}
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Range_requests
	// WAV header size is 44 bytes so we have more than enough
	// This must be correctly round up in case partial reads
	pcmSamples := p.codec.Samples16()
	readSize := (DefaultPlaybackURLRangeSize / pcmSamples) * pcmSamples
	req.Header.Add("Range", "bytes=0-"+strconv.Itoa(readSize-1)) // Try with range request

	res, err := DefaultPlaybackHTTPClient.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("non 200 received. code=%d", res.StatusCode)
	}

	contType := res.Header.Get("Content-Type")
	mimeType, _, err := mime.ParseMediaType(contType)
	if err != nil {
		return err
	}

	switch mimeType {
	case "audio/wav", "audio/x-wav", "audio/wav-x", "audio/vnd.wave":
	default:
		return fmt.Errorf("unsuported content type %q", contType)
	}

	// Check can be streamed
	if res.StatusCode == http.StatusPartialContent {
		// acceptRanges := res.Header.Get("Accept-Ranges")
		// if acceptRanges != "bytes" {
		// 	return fmt.Errorf("header Accept-Ranges != bytes. Value=%q", acceptRanges)
		// }

		contentRange := res.Header.Get("Content-Range")
		ind := strings.LastIndex(contentRange, "/")
		if ind < 0 {
			return fmt.Errorf("full audio size in Content-Range not present")
		}
		maxSize, err := strconv.ParseInt(contentRange[ind+1:], 10, 64)
		if err != nil {
			return err
		}

		if maxSize <= 0 {
			return fmt.Errorf("parsing audio size failed")
		}

		httpPartial := func(res *http.Response, writer io.Writer, size int64) error {
			chunk, err := io.ReadAll(res.Body)
			if err != nil {
				return fmt.Errorf("reading chunk stopped: %w", err)
			}
			res.Body.Close()

			if _, err := writer.Write(chunk); err != nil {
				return err
			}

			var start int64 = size
			var offset int64 = size * 2
			for ; start < maxSize; start += offset {
				end := min(start+offset-1, maxSize)
				// Range is inclusive
				rangeHDR := fmt.Sprintf("bytes=%d-%d", start, end)

				req.Header.Set("Range", rangeHDR) // Try with range request
				res, err = DefaultPlaybackHTTPClient.Do(req)
				if err != nil {
					return fmt.Errorf("failed to request range: %w", err)
				}

				if res.StatusCode == http.StatusRequestedRangeNotSatisfiable && res.ContentLength == 0 {
					break
				}

				if res.StatusCode != http.StatusPartialContent {
					return fmt.Errorf("expected partial content response: code=%d", res.StatusCode)
				}

				chunk, err := io.ReadAll(res.Body)
				if err != nil {
					return fmt.Errorf("reading chunk stopped: %w", err)
				}
				res.Body.Close()

				if _, err := writer.Write(chunk); err != nil {
					return err
				}
			}
			return nil
		}

		httpErr := make(chan error)
		reader, writer := io.Pipe()

		// Buffering allows that there is always one Write ahead
		bufferReader := bufio.NewReaderSize(reader, readSize)
		go func() {
			err := httpPartial(res, writer, int64(readSize))
			writer.Close()
			httpErr <- err
		}()

		n, err := p.streamWav(ctx, bufferReader, p.writer)
		*written += n
		p.totalWritten += n

		// Closing reader to stop writing routine
		reader.Close()
		// There is no reason having http goroutine still running
		// First make sure http goroutine exited and join errors
		err = errors.Join(<-httpErr, err)
		return err
	}

	samples, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	wavBuf := bytes.NewReader(samples)
	n, err := p.streamWav(ctx, wavBuf, p.writer)
	*written += n
	p.totalWritten += n

	return err
}
