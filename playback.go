// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sync"

	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
)

// maxPlayoutPayloadSize is the largest playout payload the library supports:
// 48 kHz stereo 16-bit at 20 ms.
const maxPlayoutPayloadSize = 3840

// Playback errors. PlaybackStopped and PlaybackReplayed also match io.EOF
// for backward compatibility with code treating EOF as successful end.
var (
	// ErrPlaybackStopped is returned by the Play* methods when playback was
	// stopped with Stop() or by DTMF interrupt. It also matches io.EOF.
	ErrPlaybackStopped = errors.New("playback stopped")
	// ErrPlaybackReplayed is returned by the Play* methods when replay was
	// requested with Replay() and playback will restart from the beginning.
	// It also matches io.EOF.
	ErrPlaybackReplayed = errors.New("playback replayed")
	// ErrSourceNotReplayable is returned when replay is requested but playback
	// source can not be restarted (generic reader without io.Seeker support).
	ErrSourceNotReplayable = errors.New("playback source is not replayable")
)

var playBufPool = sync.Pool{
	New: func() any {
		// Increase this size if there will be support for larger pools
		return make([]byte, maxPlayoutPayloadSize)
	},
}

// playbackStream streams PCM/WAV/URL audio into a dialog's audio writer.
type playbackStream struct {
	writer io.Writer
	codec  media.Codec
	onPlay func()

	// httpClient is used by PlayURLContext. Nil falls back to a default
	// client with a 20s timeout. Set by Media.NewPlayback* from the dialog's
	// MediaConfig.HTTPClient.
	httpClient *http.Client

	// Read only values
	// This will influence playout sampling buffer
	bitDepth    int
	numChannels int

	totalWritten int64
}

// newAudioPlayback creates a playback where writer is encoder/streamer to
// media codec. Prefer dialog.NewPlayback() instead of creating playback
// manually.
func newAudioPlayback(writer io.Writer, codec media.Codec) playbackStream {
	return playbackStream{
		writer:      writer,
		codec:       codec,
		bitDepth:    16,
		numChannels: codec.NumChannels,
	}
}

// Codec returns the codec the playback encodes to.
func (p *playbackStream) Codec() media.Codec {
	return p.codec
}

// PlayContext plays supported audio contents and stops streaming with
// ctx.Err() when the context is canceled. An empty mimeType streams reader as
// a raw buffer; make sure bitDepth and numChannels are set correctly.
// Cancellation latency is bounded by one packet interval (the pacing wait
// inside the writer).
func (p *playbackStream) PlayContext(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
	return p.play(ctx, reader, mimeType)
}

func (p *playbackStream) play(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
	var written int64
	var err error

	if p.onPlay != nil {
		// Execute hook on play
		p.onPlay()
	}

	switch mimeType {
	case "":
		written, err = p.stream(ctx, reader, p.writer)
	case "audio/wav", "audio/x-wav", "audio/wav-x", "audio/vnd.wave":
		written, err = p.streamWav(ctx, reader, p.writer)
	case "audio/pcm":
		written, err = p.streamPCM(ctx, reader, p.writer)
	default:
		return 0, fmt.Errorf("unsupported content type %q", mimeType)
	}

	p.totalWritten += written
	switch {
	case errors.Is(err, ErrPlaybackStopped), errors.Is(err, ErrPlaybackReplayed):
		// Do not mask stop/replay with success
		return written, err
	case errors.Is(err, io.EOF):
		return written, nil
	}
	return written, err
}

// PlayFileContext plays a wav file and closes it when finished. Cancellation
// stops streaming and returns ctx.Err(). Call it again to play the file a
// second time.
func (p *playbackStream) PlayFileContext(ctx context.Context, filename string) (int64, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	if ext := path.Ext(file.Name()); ext != ".wav" {
		return 0, fmt.Errorf("only playing wav file is now supported, but detected=%q", ext)
	}

	// Using bufio to improve disk reading
	fileReader := bufio.NewReaderSize(file, 64*1024)
	return p.PlayContext(ctx, fileReader, "audio/wav")
}

func (p *playbackStream) stream(ctx context.Context, body io.Reader, playWriter io.Writer) (int64, error) {
	payloadSize := p.calcPlayoutSize()
	buf := playBufPool.Get()
	defer playBufPool.Put(buf)
	payloadBuf := buf.([]byte)[:payloadSize] // 20 ms

	written, err := media.CopyWithBufContext(ctx, body, playWriter, payloadBuf)
	return written, err
}

func (p *playbackStream) streamPCM(ctx context.Context, body io.Reader, playWriter io.Writer) (int64, error) {
	codec := p.codec
	payloadSize := p.calcPlayoutSize()
	buf := playBufPool.Get()
	defer playBufPool.Put(buf)
	payloadBuf := buf.([]byte)[:payloadSize] // 20 ms

	enc := &audio.PCMEncoderWriter{}
	if err := enc.Init(codec, playWriter); err != nil {
		return 0, fmt.Errorf("failed to create pcm encoder: %w", err)
	}

	written, err := media.CopyWithBufContext(ctx, body, enc, payloadBuf)
	return written, err
}

func (p *playbackStream) streamWav(ctx context.Context, body io.Reader, playWriter io.Writer) (int64, error) {
	codec := p.codec
	wavReader := audio.NewWavReader(body)
	if err := wavReader.ReadHeaders(); err != nil {
		return 0, err
	}
	if wavReader.BitsPerSample != uint16(p.bitDepth) {
		return 0, fmt.Errorf("wav file bitdepth=%d does not match expected=%d", wavReader.BitsPerSample, p.bitDepth)
	}
	if wavReader.SampleRate != codec.SampleRate {
		return 0, fmt.Errorf("wav file samplerate=%d does not match expected=%d", wavReader.SampleRate, codec.SampleRate)
	}
	if wavReader.NumChannels != uint16(codec.NumChannels) {
		return 0, fmt.Errorf("wav file numchannels=%d does not match expected=%d", wavReader.NumChannels, codec.NumChannels)
	}

	// We need to read and packetize to 20 ms
	// sampleDurMS := int(codec.SampleDur.Milliseconds())
	// payloadSize := int(dec.BitsPerSample) / 8 * int(dec.numChannels) * int(dec.SampleRate) / 1000 * sampleDurMS
	payloadSize := p.codec.SamplesPCM(int(wavReader.BitsPerSample))

	buf := playBufPool.Get()
	defer playBufPool.Put(buf)
	payloadBuf := buf.([]byte)[:payloadSize] // 20 ms

	enc := &audio.PCMEncoderWriter{}
	if err := enc.Init(codec, playWriter); err != nil {
		return 0, fmt.Errorf("failed to create pcm encoder: %w", err)
	}

	written, err := media.CopyWithBufContext(ctx, wavReader, enc, payloadBuf)
	// written, err := wavCopy(dec, enc, payloadBuf)
	return written, err
}

func (p *playbackStream) calcPlayoutSize() int {
	codec := &p.codec
	sampleDurMS := int(codec.SampleDur.Milliseconds())

	bitsPerSample := p.bitDepth
	numChannels := p.numChannels
	sampleRate := codec.SampleRate
	return int(bitsPerSample) / 8 * int(numChannels) * int(sampleRate) / 1000 * sampleDurMS
}
