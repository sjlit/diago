// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

type Bridger interface {
	AddDialogSession(d DialogSession) error
}

type Bridge struct {
	// Originator is dialog session that created bridge
	Originator DialogSession
	// DTMFpass is also dtmf pipeline and proxy. By default only audio media is proxied
	// NOTE: this may not work if you are already processing DTMF with AudioReaderDTMF
	DTMFpass bool

	log *slog.Logger
	// TODO: RTPpass. RTP pass means that RTP will be proxied.
	// This gives high performance but you can not attach any pipeline in media processing
	// RTPpass bool

	dialogs []DialogSession

	// minDialogs is just helper flag when to start proxy
	WaitDialogsNum int
}

var bridgeReadPool = sync.Pool{
	New: func() any {
		b := make([]byte, media.RTPBufSize)
		return &b
	},
}

// NewBridge creates bridge with default settings.
func NewBridge() Bridge {
	b := Bridge{}
	b.Init(media.DefaultLogger())
	return b
}

func (b *Bridge) Init(log *slog.Logger) {
	b.log = log
	if b.log == nil {
		b.log = media.DefaultLogger()
	}

	if b.WaitDialogsNum == 0 {
		b.WaitDialogsNum = 2
	}
}

func (b *Bridge) GetDialogs() []DialogSession {
	return b.dialogs
}

func (b *Bridge) AddDialogSession(d DialogSession) error {
	// Check can this dialog be added to bridge. NO TRANSCODING
	if b.Originator != nil {
		// This may look ugly but it is safe way of reading
		origM := b.Originator.Media()
		origProps := MediaProps{}
		if _, err := origM.audioWriterProps(&origProps); err != nil {
			return fmt.Errorf("bridge originator dialog has no media: %w", err)
		}

		m := d.Media()
		mprops := MediaProps{}
		if _, err := m.audioWriterProps(&mprops); err != nil {
			return fmt.Errorf("bridge dialog %q has no media: %w", d.Id(), err)
		}

		err := func() error {
			if origProps.Codec != mprops.Codec {
				return fmt.Errorf("no transcoding supported in bridge codec1=%+v codec2=%+v", origProps.Codec, mprops.Codec)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	b.dialogs = append(b.dialogs, d)
	if len(b.dialogs) == 1 {
		b.Originator = d
	}

	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	if len(b.dialogs) > 2 {
		return fmt.Errorf("currently bridge only support 2 party")
	}
	// Check are both answered
	for _, d := range b.dialogs {
		if err := d.Media().checkMediaUsable(); err != nil {
			return fmt.Errorf("dialog session not answered %q: %w", d.Id(), err)
		}
	}

	go func() {
		defer func(start time.Time) {
			b.log.Debug("Proxy media setup", "dur", time.Since(start).String())
		}(time.Now())
		if err := b.proxyMedia(); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}

			b.log.Error("Proxy media stopped", "error", err)
		}
	}()
	return nil
}

// ProxyMedia is explicit starting proxy media.
// In some cases you want to control and be signaled when bridge terminates
//
// NOTE: Should be only called if you want to start manually proxying.
// It is required to set WaitDialogsNum higher than 2.
// Parallel user writers on the bridged dialogs are not supported: the proxy
// writes through the same handle.
//
// Experimental
func (b *Bridge) ProxyMedia() error {
	if len(b.dialogs) < 2 {
		return fmt.Errorf("number of dialogs must equal to 2")
	}

	if b.WaitDialogsNum < 3 {
		return fmt.Errorf("you are already running proxy media. Increase WaitDialogsNum")
	}

	return b.proxyMedia()
}

// ProxyMediaControl starts proxy in background and allows to stop proxy at any time.
// Stop should be called once and it is not needed to be called if call is terminating
//
// Stop interrupts the proxy through the write gate of the bridged dialogs and
// restores the writers afterwards.
//
// Experimental
func (b *Bridge) ProxyMediaControl() (func() error, error) {
	// Same precondition as ProxyMedia, proxyMedia indexes dialogs [0] and [1]
	if len(b.dialogs) < 2 {
		return nil, fmt.Errorf("number of dialogs must equal to 2")
	}

	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- b.proxyMedia()
	}()

	stopF := func() error {
		// Interrupt the proxy through the write gate: the in-flight write
		// completes and the next write surfaces media.ErrWritePaused, which
		// the proxy treats as a clean stop.
		rels := make([]func(), 0, len(b.dialogs))
		var pauseErrs error
		for _, d := range b.dialogs {
			release, err := d.Media().PauseAudioWrite()
			if err != nil {
				pauseErrs = errors.Join(pauseErrs, err)
				continue
			}
			rels = append(rels, release)
		}

		// Wait goroutine termination
		err := <-proxyErr
		for _, release := range rels {
			release()
		}
		return errors.Join(pauseErrs, err)
	}

	return stopF, nil
}

// proxyMedia starts routine to proxy media between
// Should be called after having 2 or more participants
func (b *Bridge) proxyMedia() error {
	var err error
	log := b.log

	m1 := b.dialogs[0].Media()
	m2 := b.dialogs[1].Media()

	// Lets for now simplify proxy and later optimize

	if b.DTMFpass {
		errCh := make(chan error, 4)
		go func() {
			errCh <- b.proxyMediaWithDTMF(m1, m2)
		}()

		go func() {
			errCh <- b.proxyMediaWithDTMF(m2, m1)
		}()

		// Wait for all to finish
		for i := 0; i < 2; i++ {
			err = errors.Join(err, <-errCh)
		}
		return err
	}
	errCh := make(chan error, 2)
	startProxy := func(mFrom, mTo *DialogMedia) error {
		p1, p2 := MediaProps{}, MediaProps{}
		r, err := mFrom.audioReaderProps(&p1)
		if err != nil {
			return err
		}
		w, err := mTo.audioWriterProps(&p2)
		if err != nil {
			return err
		}

		log := log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
		log.Debug("Starting proxy media routine")
		go proxyMediaBackground(log, r, w, errCh)
		return nil
	}

	if err := startProxy(m1, m2); err != nil {
		return err
	}
	// Second
	if err := startProxy(m2, m1); err != nil {
		return err
	}

	// Wait for all to finish
	for i := 0; i < 2; i++ {
		err = errors.Join(err, <-errCh)
	}
	return err
}

func proxyMediaBackground(log *slog.Logger, reader io.Reader, writer io.Writer, ch chan error) {
	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	written, err := copyWithBuf(reader, writer, buf.([]byte))
	log.Debug("Proxy media routine finished", "bytes", written)
	if errors.Is(err, media.ErrWritePaused) {
		// Stopped through the write gate
		err = nil
	}
	if err, ok := err.(net.Error); ok && err.Timeout() {
		log.Debug("Proxy media stopped with timeout. RTP Deadline", "error", err)
		err = nil
	}
	ch <- err
}

func (b *Bridge) proxyMediaWithDTMF(m1 *DialogMedia, m2 *DialogMedia) error {
	dtmfReader := DTMFReader{}
	p1, p2 := MediaProps{}, MediaProps{}
	r, err := m1.AudioReader(WithAudioReaderDTMF(&dtmfReader), WithAudioReaderMediaProps(&p1))
	if err != nil {
		return err
	}
	dtmfWriter := DTMFWriter{}
	w, err := m2.AudioWriter(WithAudioWriterDTMF(&dtmfWriter), WithAudioWriterMediaProps(&p2))
	if err != nil {
		return err
	}
	dtmfReader.OnDTMF(func(dtmf rune) error {
		return dtmfWriter.WriteDTMF(dtmf)
	})

	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	log := b.log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
	log.Debug("Starting proxy media routine")
	written, err := copyWithBuf(r, w, buf.([]byte))
	log.Debug("Bridge proxy stream finished", "bytes", written)
	return err
}

// BridgeMix is mixing audio when having 2 or more parties.
//
// Experimental: not fully tested yet
type BridgeMix struct {
	mu      sync.Mutex
	dialogs []DialogSession

	mixWG    sync.WaitGroup
	mixState int
	// pauseReleases holds reader gate releases taken by mixStop, applied by
	// mixStopWait after the mix goroutines rejoin. Guarded by mu.
	pauseReleases []func()

	// WaitDialogsNum is just helper flag when to start proxy
	WaitDialogsNum int
	// RealtimeReader is almost always nesessary if you are delaying audio streaming(mixing) in bridge
	RealtimeReader bool
	Poll           bool
	log            *slog.Logger
}

var (
	// BridgeDebug enables some traces
	BridgeDebug bool

	bridgeTrace = func(args ...any) {
		if BridgeDebug {
			fmt.Fprintln(os.Stderr, args...)
		}
	}
)

func NewBridgeMix() *BridgeMix {
	b := BridgeMix{
		RealtimeReader: true,
		Poll:           true,
	}
	b.Init()
	return &b
}

// Init initializes bridge struct. Use only if construct bridge with struct
// or use NewBridgeMix
func (b *BridgeMix) Init() {
	b.log = media.DefaultLogger().With("caller", "bridge_mix")
}

func (b *BridgeMix) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	str := fmt.Sprintf("state: %d", b.mixState)
	str += " dialogs:["
	for _, d := range b.dialogs {
		str += " " + d.Id()
	}
	str += "]"
	return str
}

// DialogSessionsList returns list of dialogs in bridge
// It is not safe to use dialogs for media until they are removed from bridge
func (b *BridgeMix) DialogSessionsList() []DialogSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.dialogs)
}

func (b *BridgeMix) AddDialogSession(d DialogSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if state := d.DialogSIP().LoadState(); state != sip.DialogStateConfirmed {
		return fmt.Errorf("dialog must be answered before adding into bridge")
	}

	// Stop any current mixing
	b.log.Debug("Stoping mix", "dialog", d.Id())
	if err := b.mixStopWait(); err != nil {
		return fmt.Errorf("failed to stop current mixing: %w", err)
	}

	b.dialogs = append(b.dialogs, d)
	b.log.Debug("Added dialog", "dialog", d.Id(), "total", len(b.dialogs))
	b.mixStart()
	return nil
}

func (b *BridgeMix) RemoveDialogSession(d DialogSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	dialogID := d.Id()

	var dialog DialogSession
	for _, d := range b.dialogs {
		if d.Id() == dialogID {
			dialog = d
			break
		}
	}
	if dialog == nil {
		return nil
	}

	b.log.Debug("Stoping mix", "dialog", dialog.Id())

	if err := b.mixStopWait(); err != nil {
		return fmt.Errorf("failed to stop current mixing: %w", err)
	}

	// NOTE: mixStopWait unlocks so we can not do any update before
	for i, d := range b.dialogs {
		if d.Id() == dialogID {
			b.dialogs = append(b.dialogs[:i], b.dialogs[i+1:]...)
			break
		}
	}

	b.log.Debug("Removed dialog", "dialog", dialog.Id(), "total", len(b.dialogs))
	return b.mixStart()
}

func (b *BridgeMix) stateWrite(s int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stateWriteUnsafe(s)
}

func (b *BridgeMix) stateWriteUnsafe(s int) {
	b.mixState = s
}

func (b *BridgeMix) stateRead() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mixState
}

func (b *BridgeMix) mixStopWait() error {
	// DO NOT CALL THIS INSIDE LOOP of b.dialogs. This Unlocks
	stopInProgress, err := b.mixStop()
	if err != nil {
		return fmt.Errorf("failed to stop current mixing: %w", err)
	}

	if stopInProgress {
		b.mu.Unlock()
		b.mixWG.Wait()
		b.mu.Lock()
	}
	// Release read pauses (mixStop paused the dialogs to stop the mix)
	var allErros error
	for _, release := range b.pauseReleases {
		release()
	}
	b.pauseReleases = nil
	return allErros
}

func (b *BridgeMix) mixStop() (bool, error) {
	if state := b.mixState; state != 1 {
		// Only if state is running this goroutine can stop it
		return false, nil
	}
	b.mixState = 2 // Set it stoping in progress
	var allErros error
	for _, d := range b.dialogs {
		// Pause reading: interrupts the mix loop reads through the reader
		// gate; releases are held until mixStopWait rejoins the mix.
		release, err := d.Media().PauseAudioRead()
		if err != nil {
			allErros = errors.Join(allErros, err)
			continue
		}
		b.pauseReleases = append(b.pauseReleases, release)
	}
	return true, allErros
}

func (b *BridgeMix) mixStart() error {
	if b.mixState == 2 {
		// A stop is in progress (another goroutine is in mixStopWait).
		// Don't start a new mix to avoid WaitGroup Add/Wait race.
		return nil
	}
	if len(b.dialogs) < 1 {
		return nil
	}
	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	ctx, cancelPoll := context.WithCancel(context.Background())
	// We could decide and optimize here, poll vs deadlines
	poll := b.Poll
	rwStreams, err := func() ([]*bridgePCMStream, error) {
		rwStreams := make([]*bridgePCMStream, len(b.dialogs))
		firstDialogCodec := media.Codec{}

		for i, d := range b.dialogs {
			rwStreams[i] = &bridgePCMStream{}
			if err := b.addDialogStream(ctx, d, rwStreams[i], &firstDialogCodec, poll); err != nil {
				return nil, err
			}
		}
		return rwStreams, nil
	}()
	if err != nil {
		cancelPoll()
		return err
	}

	// Start new mix
	b.mixWG.Add(1)
	b.stateWriteUnsafe(1)
	go func(rwStreams []*bridgePCMStream) {
		defer cancelPoll()
		defer b.mixWG.Done()
		defer b.stateWrite(0)
		b.log.Debug("Starting mix loop", "streams.len", len(rwStreams))
		if err := b.mixLoop(rwStreams, poll); err != nil {
			b.log.Info("Mix stopped with error", "error", err)
		}
	}(rwStreams)
	return nil
}

func (b *BridgeMix) mixLoop(rwStreams []*bridgePCMStream, poll bool) error {
	mixBuf := make([]byte, media.RTPBufSize)

	if len(rwStreams) == 1 {
		b.log.Debug("Only single stream in bridge, reading bufffers...")
		// Just keep streaming
		r := rwStreams[0]
		if !poll {
			_, err := media.ReadAll(r.r, media.RTPBufSize)
			return err
		}

		for {
			bw, more := <-r.pipeWrite
			if !more {
				break
			}
			n := copy(r.buf, bw)
			r.pipeRead <- n
		}
		return nil
	}

	// Currently we consider that sample clock is done by Audio Writers
	// The slowest will cause jitter.
	// TODO fix this with single ticker
	for {
		n, err := b.mixAllStreams(rwStreams, mixBuf, poll)
		if err != nil {
			return err
		}
		if n == 0 {
			bridgeTrace("Nothing read, delaying read")
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// broadcast to all
		for i, w := range rwStreams {
			streamBuf := mixBuf[:n]
			if w.n > 0 {
				readBuf := w.buf
				streamBuf = unmixStream(readBuf[:w.n], mixBuf[:n])
			}

			n, err := w.w.Write(streamBuf)
			bridgeTrace("Writing stream", "i", i, "stream", w.id, "n", n, "err", err)
			if err != nil {
				// Detect is this Deadline or EOF error caused by stream exiting
				if errors.Is(err, os.ErrDeadlineExceeded) {
					state := b.stateRead()
					if state != 1 {
						// We are stopped
						return err
					}

					// Mixing has been stopped or network problem
					w.markGone = true
					continue

				}
				return err
			}
		}
	}
}

type bridgePCMStream struct {
	id uint32
	r  io.Reader
	w  io.Writer
	// dm resolves the CURRENT media session at use time so mix-loop deadline
	// control survives media updates (re-INVITE) - docs/contracts.md §4
	dm *DialogMedia
	// read buf
	buf []byte
	n   int

	pipeRead  chan int
	pipeWrite chan []byte
	markGone  bool
}

func (b *BridgeMix) addDialogStream(ctx context.Context, d DialogSession, stream *bridgePCMStream, firstDialogCodec *media.Codec, poll bool) error {
	m := d.Media()

	p := MediaProps{}
	r, err := m.AudioReader(WithAudioReaderMediaProps(&p))
	if err != nil {
		return err
	}

	if firstDialogCodec.SampleRate == 0 {
		firstDialogCodec = &p.Codec
	}

	if firstDialogCodec.SampleRate != p.Codec.SampleRate && firstDialogCodec.SampleDur != p.Codec.SampleDur {
		return fmt.Errorf("Codec missmatch. Resampling or transcoding is not supported")
	}

	rtr := func() io.Reader {
		if !b.RealtimeReader {
			return r
		}

		if rtr, ok := r.(*media.RTPRealTimeReader); ok {
			return rtr
		}

		rtr := media.NewRTPRealTimeReader(r, m.RTPPacketReader, p.Codec)
		m.SetAudioReader(rtr)
		return rtr
	}()

	// Attach PCM decoder
	pcmReader := audio.PCMDecoderReader{}
	if err := pcmReader.Init(p.Codec, rtr); err != nil {
		return err
	}

	// Now do write stream
	p = MediaProps{}
	w, err := m.AudioWriter(WithAudioWriterMediaProps(&p))
	if err != nil {
		return err
	}

	pcmWriter := audio.PCMEncoderWriter{}
	if err := pcmWriter.Init(p.Codec, w); err != nil {
		return err
	}

	*stream = bridgePCMStream{
		r:         &pcmReader,
		w:         &pcmWriter,
		dm:        m,
		id:        m.RTPPacketWriter.SSRC,
		buf:       make([]byte, media.RTPBufSize),
		pipeRead:  make(chan int),
		pipeWrite: make(chan []byte),
	}

	if poll {
		// We do buffering because initial packet can be read oner than actual mixing has started
		b.mixWG.Add(1)
		bridgeTrace("poll: starting stream", "stream.id", stream.id)
		go func(s *bridgePCMStream) {
			defer b.mixWG.Done()

			bufPtr := bridgeReadPool.Get().(*[]byte)
			defer bridgeReadPool.Put(bufPtr)

			defer close(s.pipeWrite)

			buf := *bufPtr
			for {
				n, err := s.r.Read(buf)
				if err != nil {
					bridgeTrace("poll: stopped with error", "error", err, "stream.id", stream.id)
					return
				}

				select {
				case s.pipeWrite <- buf[:n]:
					nw := <-s.pipeRead
					if nw != n {
						// Consumer did not accept the full chunk. Log and keep streaming,
						// this must not take down the media goroutine
						b.log.Error("Reading from pipe was not full", "stream.id", s.id, "n", n, "written", nw)
					}
				case <-ctx.Done():
					bridgeTrace("poll: stream context canceled", "stream.id", stream.id)
					return
				}
			}
		}(stream)
		return nil
	}
	return nil
}

func (b *BridgeMix) mixAllStreams(rwStreams []*bridgePCMStream, mixedBuf []byte, poll bool) (int, error) {
	maxN := 0
	// zero mixed buf
	for i := 0; i < len(mixedBuf); i++ {
		mixedBuf[i] = 0
		// binary.LittleEndian.PutUint16(mixedBuf[i:], uint16(0))
	}

	if !poll {
		// If are not polling data then we need todo direct read
		err := func() error {
			for i, r := range rwStreams {
				ms := r.dm.currentMediaSession()
				if ms == nil {
					continue
				}
				ms.StopRTP(1, 1*time.Millisecond)

				// Mostly PCM sample size should be same or less our sampling
				// but we should keep same sampling or deal this per writer?
				n, err := r.r.Read(r.buf)
				rwStreams[i].n = n
				if err != nil {
					if errors.Is(err, media.ErrReadPaused) {
						state := b.stateRead()
						if state != 1 {
							// We are stopped
							return err
						}
						continue
					}
					if errors.Is(err, os.ErrDeadlineExceeded) {
						state := b.stateRead()
						if state != 1 {
							// We are stopped
							return err
						}
						continue
					}
					return err
				}
				maxN = max(maxN, n)
			}
			return nil
		}()
		return maxN, err
	}

	err := func() error {
		handledStreams := len(rwStreams)
		for _, r := range rwStreams {
			if r.markGone {
				handledStreams--
				continue
			}
			r.n = 0 // Make sure it is zero

			select {
			case bw, more := <-r.pipeWrite:
				if !more {
					r.markGone = true
					continue
				}
				n := copy(r.buf, bw)
				r.n = n
				r.pipeRead <- n

				readBuf := r.buf[:n]
				mixN := audio.PCMMix(mixedBuf, mixedBuf, readBuf)
				maxN = max(maxN, mixN)

			default:
				// Do not block
				b.log.Debug("poll: no packet on stream", "stream.id", r.id)
			}
		}

		if handledStreams == 0 {
			return fmt.Errorf("all streams are gones")
		}

		if handledStreams < len(rwStreams) || maxN == 0 {
			state := b.stateRead()
			if state != 1 {
				// We are stopped
				return fmt.Errorf("reading is stopped")
			}
		}

		return nil
	}()

	b.log.Debug("Mixing done", "streams.len", len(rwStreams), "maxN", maxN)
	return maxN, err
}

func unmixStream(buf []byte, mixedBuf []byte) []byte {
	// Process only bytes we actually received. Keep 16-bit sample alignment,
	// otherwise stale bytes beyond the read would be unmixed and written to the wire
	n := min(len(buf), len(mixedBuf))
	n &^= 1

	readBuf := buf[:n]
	audio.PCMUnmix(readBuf, mixedBuf, readBuf)
	return readBuf
}
