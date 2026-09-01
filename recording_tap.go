// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"errors"
	"io"
	"sync"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

// ErrRecordingClosed is returned by StereoRecording operations issued after
// Close.
var ErrRecordingClosed = errors.New("recording already closed")

// RecordingOption configures DialogMedia.StartStereoRecording.
type RecordingOption func(*recordingConfig)

type recordingConfig struct {
	failOpen bool
	spoolDir string
}

// WithRecordingFailOpen controls what a PCM write failure (disk full, IO
// degradation) does to the call. Default is fail-open: the tap stops taking
// writes, the media pipeline keeps flowing, and the error surfaces through
// StereoRecording.Err and Close. Pass false to propagate the write error into
// the reader/writer chain instead (the pre-existing tap behaviour), which lets
// a full disk interrupt bridged media.
func WithRecordingFailOpen(b bool) RecordingOption {
	return func(c *recordingConfig) { c.failOpen = b }
}

// WithRecordingSpoolDir sets the directory holding the two per-direction raw
// PCM spool files the stereo monitor interleaves at Close. Empty (default)
// uses os.TempDir(). Point it at the same partition as the WAV output to keep
// recording IO local and to size disk headroom against a single spool.
func WithRecordingSpoolDir(dir string) RecordingOption {
	return func(c *recordingConfig) { c.spoolDir = dir }
}

// StereoRecording is an active inline recording tap returned by
// DialogMedia.StartStereoRecording. All methods are safe for concurrent use.
type StereoRecording struct {
	dm  *DialogMedia
	mon *audio.MonitorPCMStereo
	wav *audio.WavWriter

	// innerR/innerW are the pipeline heads wrapped at install time; Close
	// restores them while the tap is still the outermost head.
	innerR io.Reader
	innerW io.Writer

	mu     sync.Mutex
	closed bool
}

// StartStereoRecording installs a stereo WAV recording tap into the dialog's
// audio pipeline and returns a handle to drive it. Both directions are
// decoded and interleaved into w at Close. Unlike the deprecated
// SetAudioReader/SetAudioWriter dance, the tap wraps the current reader and
// writer heads atomically under the media lock, so there is no half-wired
// window and no deprecated setter is touched.
//
// It must be called before the dialog joins a Bridge: BridgeMix resolves each
// leg's reader/writer exactly once at AddDialogSession, so a tap installed
// afterwards is bypassed by bridged traffic. See docs/contracts.md.
//
// The caller keeps ownership of w: StartStereoRecording and Close never close
// it. Close the underlying file (and, for a recording to a fresh file, handle
// its fd) after Close returns.
func (d *DialogMedia) StartStereoRecording(w io.WriteSeeker, opts ...RecordingOption) (*StereoRecording, error) {
	cfg := recordingConfig{failOpen: true}
	for _, o := range opts {
		o(&cfg)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.mediaGuard(); err != nil {
		return nil, err
	}

	innerR := d.getAudioReader()
	innerW := d.getAudioWriter()
	// Mirror AudioReader/AudioWriter's typed-nil guard for the setup window
	// (mediaSession set, stable handles not yet created).
	if innerR == nil && d.RTPPacketReader == nil {
		return nil, ErrDialogNotAnswered
	}
	if innerW == nil && d.RTPPacketWriter == nil {
		return nil, ErrDialogNotAnswered
	}

	codec := media.CodecAudioFromSession(d.mediaSession)

	wavWriter := audio.NewWavWriter(w)
	if codec.SampleRate != 0 {
		wavWriter.SampleRate = int(codec.SampleRate)
	}

	mon := &audio.MonitorPCMStereo{SpoolDir: cfg.spoolDir}
	if err := mon.Init(wavWriter, codec, codec, innerR, innerW); err != nil {
		_ = wavWriter.Close()
		return nil, err
	}
	mon.MonitorPCMReader.FailOpen = cfg.failOpen
	mon.MonitorPCMWriter.FailOpen = cfg.failOpen

	rec := &StereoRecording{
		dm:     d,
		mon:    mon,
		wav:    wavWriter,
		innerR: innerR,
		innerW: innerW,
	}
	d.audioReader = mon
	d.audioWriter = mon
	return rec, nil
}

// Pause stops PCM collection on both directions while bridged media keeps
// flowing untouched. Resume re-enables it; the paused interval is padded with
// silence so both channels stay aligned with the call timeline - a paused
// interval appears as silence in the final WAV. Both return
// ErrRecordingClosed after Close.
func (r *StereoRecording) Pause() error {
	if r.isClosed() {
		return ErrRecordingClosed
	}
	r.mon.Pause()
	return nil
}

// Resume continues PCM collection after Pause. See Pause.
func (r *StereoRecording) Resume() error {
	if r.isClosed() {
		return ErrRecordingClosed
	}
	r.mon.Resume()
	return nil
}

// Err returns the first recording write error swallowed by the fail-open
// policy, or nil. A non-nil Err means the recording is degraded (the call is
// unaffected); Close reports finalization errors too.
func (r *StereoRecording) Err() error {
	return r.mon.Err()
}

// Close stops collection, flushes and interleaves the two PCM spools into the
// WAV, rewrites the WAV header, and uninstalls the tap from the audio
// pipeline. It does not close the caller's writer. Close is idempotent; the
// second call is a no-op returning nil.
//
// Uninstall only removes the tap while it is still the outermost head. If a
// Bridge wrapped the chain on top after Start, the (now stopped, hence
// transparent) tap stays in place rather than surgically breaking the outer
// chain.
func (r *StereoRecording) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	// Stop collection, flush, interleave the spools into the WAV, drop the
	// temp spool files.
	err := r.mon.Close()
	// Rewrite the WAV header (seeks to 0); the caller still owns the fd.
	err = errors.Join(err, r.wav.Close())

	// Uninstall only when the tap is still the outermost head.
	r.dm.mu.Lock()
	if r.dm.audioReader == r.mon {
		r.dm.audioReader = r.innerR
	}
	if r.dm.audioWriter == r.mon {
		r.dm.audioWriter = r.innerW
	}
	r.dm.mu.Unlock()
	return err
}

func (r *StereoRecording) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}
