// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

var (
	PlaybackBufferSize = 3840 // For now largest we support. 48000 sample rate with 2 channels
)

// Playback errors. PlaybackStopped and PlaybackReplayed also match io.EOF
// for backward compatibility with code treating EOF as successful end.
var (
	// ErrPlaybackStopped is returned by Play when playback was stopped with
	// Stop() or by DTMF interrupt. It also matches io.EOF.
	ErrPlaybackStopped = errors.New("playback stopped")
	// ErrPlaybackReplayed is returned by Play when replay was requested with
	// Replay() and playback will restart from the beginning. It also matches io.EOF.
	ErrPlaybackReplayed = errors.New("playback replayed")
	// ErrSourceNotReplayable is returned when replay is requested but playback
	// source can not be restarted (generic reader without io.Seeker support).
	ErrSourceNotReplayable = errors.New("playback source is not replayable")
)

var playBufPool = sync.Pool{
	New: func() any {
		// Increase this size if there will be support for larger pools
		return make([]byte, PlaybackBufferSize)
	},
}

type AudioPlayback struct {
	writer io.Writer
	codec  media.Codec
	onPlay func()

	// Read only values
	// This will influence playout sampling buffer
	BitDepth    int
	NumChannels int

	totalWritten int64
}

// NewAudioPlayback creates a playback where writer is encoder/streamer to media codec
// Use dialog.PlaybackCreate() instead creating manually playback
func NewAudioPlayback(writer io.Writer, codec media.Codec) AudioPlayback {
	return AudioPlayback{
		writer:      writer,
		codec:       codec,
		BitDepth:    16,
		NumChannels: codec.NumChannels,
	}
}

func (p *AudioPlayback) Codec() media.Codec {
	return p.codec
}

// Play is generic approach to play supported audio contents
// Empty mimeType will stream reader as buffer. Make sure that bitdepth and numchannels is set correctly
//
// Deprecated: Use PlayContext for cancellable playback.
func (p *AudioPlayback) Play(reader io.Reader, mimeType string) (int64, error) {
	return p.play(context.Background(), reader, mimeType)
}

// PlayContext plays supported audio contents and stops streaming with
// ctx.Err() when the context is canceled. Cancellation latency is bounded by
// one packet interval (the pacing wait inside the writer).
func (p *AudioPlayback) PlayContext(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
	return p.play(ctx, reader, mimeType)
}

func (p *AudioPlayback) play(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
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
		return 0, fmt.Errorf("unsuported content type %q", mimeType)
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

// PlayFile will play file and close file when finished playing
// If you need to play same file multiple times, that use generic Play function
//
// Deprecated: Use PlayFileContext.
func (p *AudioPlayback) PlayFile(filename string) (int64, error) {
	return p.PlayFileContext(context.Background(), filename)
}

// PlayFileContext plays a wav file and closes it when finished. Cancellation
// stops streaming and returns ctx.Err().
func (p *AudioPlayback) PlayFileContext(ctx context.Context, filename string) (int64, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	if ext := path.Ext(file.Name()); ext != ".wav" {
		return 0, fmt.Errorf("only playing wav file is now supported, but detected=%s", ext)
	}

	// Using bufio to improve disk reading
	fileReader := bufio.NewReaderSize(file, 64*1024)
	return p.PlayContext(ctx, fileReader, "audio/wav")
}

func (p *AudioPlayback) stream(ctx context.Context, body io.Reader, playWriter io.Writer) (int64, error) {
	payloadSize := p.calcPlayoutSize()
	buf := playBufPool.Get()
	defer playBufPool.Put(buf)
	payloadBuf := buf.([]byte)[:payloadSize] // 20 ms

	written, err := media.CopyWithBufContext(ctx, body, playWriter, payloadBuf)
	return written, err
}

func (p *AudioPlayback) streamPCM(ctx context.Context, body io.Reader, playWriter io.Writer) (int64, error) {
	codec := p.codec
	payloadSize := p.calcPlayoutSize()
	buf := playBufPool.Get()
	defer playBufPool.Put(buf)
	payloadBuf := buf.([]byte)[:payloadSize] // 20 ms

	enc := &audio.PCMEncoderWriter{}
	if err := enc.Init(codec, playWriter); err != nil {
		return 0, fmt.Errorf("failed to create PCM encoder: %w", err)
	}

	written, err := media.CopyWithBufContext(ctx, body, enc, payloadBuf)
	return written, err
}

func (p *AudioPlayback) streamWav(ctx context.Context, body io.Reader, playWriter io.Writer) (int64, error) {
	codec := p.codec
	wavReader := audio.NewWavReader(body)
	if err := wavReader.ReadHeaders(); err != nil {
		return 0, err
	}
	if wavReader.BitsPerSample != uint16(p.BitDepth) {
		return 0, fmt.Errorf("wav file bitdepth=%d does not match expected=%d", wavReader.BitsPerSample, p.BitDepth)
	}
	if wavReader.SampleRate != codec.SampleRate {
		return 0, fmt.Errorf("wav file samplerate=%d does not match expected=%d", wavReader.SampleRate, codec.SampleRate)
	}
	if wavReader.NumChannels != uint16(codec.NumChannels) {
		return 0, fmt.Errorf("wav file numchannels=%d does not match expected=%d", wavReader.NumChannels, codec.NumChannels)
	}

	// We need to read and packetize to 20 ms
	// sampleDurMS := int(codec.SampleDur.Milliseconds())
	// payloadSize := int(dec.BitsPerSample) / 8 * int(dec.NumChannels) * int(dec.SampleRate) / 1000 * sampleDurMS
	payloadSize := p.codec.SamplesPCM(int(wavReader.BitsPerSample))

	buf := playBufPool.Get()
	defer playBufPool.Put(buf)
	payloadBuf := buf.([]byte)[:payloadSize] // 20 ms

	enc := &audio.PCMEncoderWriter{}
	if err := enc.Init(codec, playWriter); err != nil {
		return 0, fmt.Errorf("failed to create PCM encoder: %w", err)
	}

	written, err := media.CopyWithBufContext(ctx, wavReader, enc, payloadBuf)
	// written, err := wavCopy(dec, enc, payloadBuf)
	return written, err
}

func (p *AudioPlayback) calcPlayoutSize() int {
	codec := &p.codec
	sampleDurMS := int(codec.SampleDur.Milliseconds())

	bitsPerSample := p.BitDepth
	numChannels := p.NumChannels
	sampleRate := codec.SampleRate
	return int(bitsPerSample) / 8 * int(numChannels) * int(sampleRate) / 1000 * sampleDurMS
}

// func wavCopy(dec *audio.WavReader, playWriter io.Writer, payloadBuf []byte) (int64, error) {
// 	var totalWritten int64
// 	for {
// 		ch, err := dec.NextChunk()
// 		if err != nil {
// 			return totalWritten, err
// 		}
// 		fmt.Println("Chunk wav", ch)
// 		if ch.ID != riff.DataFormatID && ch.ID != [4]byte{} {
// 			// Until we reach data chunk we will draining
// 			ch.Drain()
// 			continue
// 		}

// 		fmt.Println("copy buf", len(payloadBuf))
// 		n, err := copyWithBuf(ch, playWriter, payloadBuf)
// 		totalWritten += n
// 		if err != nil {
// 			return totalWritten, err
// 		}
// 	}
// }
