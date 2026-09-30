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
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
)

// Bridger is the bridge contract accepted by Agent.DialBridge. Both *Bridge
// (2-party proxy) and *BridgeMix (N-party mixer) satisfy it.
type Bridger interface {
	Add(d Call) error
}

var (
	_ Bridger = (*Bridge)(nil)
	_ Bridger = (*BridgeMix)(nil)
)

// Bridge is a two-party proxy: it forwards encoded media between the two
// added dialogs without transcoding.
type Bridge struct {
	// Originator is dialog session that created bridge
	Originator Call
	// DTMFpass is also dtmf pipeline and proxy. By default only audio media is proxied
	// NOTE: this may not work if you are already processing DTMF with NewDTMFReader
	DTMFpass bool

	log *slog.Logger
	// TODO: RTPpass. RTP pass means that RTP will be proxied.
	// This gives high performance but you can not attach any pipeline in media processing
	// RTPpass bool

	// dialogs and Originator are guarded by mu: AddCall may be called
	// while a previous proxyMedia goroutine is still reading the list, and
	// ProxyMediaControl's stop iterates it from another goroutine.
	mu      sync.Mutex
	dialogs []Call

	// minDialogs is just helper flag when to start proxy
	WaitDialogsNum int

	// DiscardQueuedOnStart drops RTP already queued on each leg's socket right
	// before the proxy loop starts, so bridging begins from live media instead
	// of replaying a pre-bridge backlog at the line rate - which otherwise
	// turns into permanent one-way latency.
	//
	// Opt-in (default false). Enable it for answer-before-bridge B2BUAs,
	// where the peer starts sending media before the second leg joins and
	// nothing consumes the socket in between.
	DiscardQueuedOnStart bool

	// Lifecycle (#21): closed rejects Add; proxyRunning/proxyDone track the
	// current proxyMedia generation so Remove/Close can interrupt and wait.
	closed         bool
	isProxyRunning bool
	proxyDone      chan struct{}

	// initOnce makes Init idempotent and lets ensureInit prepare the zero
	// value lazily, so a Bridge built as a literal is usable too.
	initOnce sync.Once
}

var (
	// ErrBridgeClosed is returned by Add after Close.
	ErrBridgeClosed = errors.New("bridge closed")
	// ErrBridgeFull is returned by Bridge.Add when the 2-party bridge already
	// holds two dialogs.
	ErrBridgeFull = errors.New("bridge is full")
)

var bridgeReadPool = sync.Pool{
	New: func() any {
		b := make([]byte, media.RTPBufSize)
		return &b
	},
}

// NewBridge creates bridge with default settings.
// The bridge state (dialog list, originator) is guarded internally, so the
// pointer must be used: methods are not safe to call on a copied value.
func NewBridge() *Bridge {
	b := Bridge{}
	b.Init(media.DefaultLogger())
	return &b
}

// Init installs the bridge defaults (logger, WaitDialogsNum, idle proxy
// channel). It is idempotent: subsequent calls are no-ops, so the zero value
// is usable even when the caller never calls Init.
func (b *Bridge) Init(log *slog.Logger) {
	b.initOnce.Do(func() {
		b.log = log
		if b.log == nil {
			b.log = media.DefaultLogger()
		}

		if b.WaitDialogsNum == 0 {
			b.WaitDialogsNum = 2
		}
		if b.proxyDone == nil {
			// Idle state is done (closed chan) so waiters never block.
			b.proxyDone = make(chan struct{})
			close(b.proxyDone)
		}
	})
}

// ensureInit prepares the zero value on first use.
func (b *Bridge) ensureInit() {
	b.Init(media.DefaultLogger())
}

// originator returns the current bridge originator under mu. The field is
// guarded because AddCall updates it while callers (ex. DialBridge)
// may read it concurrently.
func (b *Bridge) originator() Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Originator
}

// Add adds a dialog to the 2-party bridge; the call starts proxying when the
// joined dialog count reaches WaitDialogsNum.
func (b *Bridge) Add(d Call) error {
	if d == nil {
		return fmt.Errorf("bridge: dialog session is nil")
	}
	b.ensureInit()
	// Check can this dialog be added to bridge. NO TRANSCODING
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBridgeClosed
	}
	// Bridge only supports 2 parties: reject before appending so a failed
	// third Add does not leave a stale dialog behind (#21 atomicity).
	if len(b.dialogs) >= 2 {
		return fmt.Errorf("%w: 2-party bridge already has 2 dialogs", ErrBridgeFull)
	}

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
			return fmt.Errorf("bridge dialog %q has no media: %w", d.ID(), err)
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

	// Remove the just-appended dialog: a failed Add must not leave a stale
	// entry in the bridge (#21 atomicity).
	rollbackLast := func() {
		b.dialogs = b.dialogs[:len(b.dialogs)-1]
		if b.Originator != nil && b.Originator.ID() == d.ID() {
			if len(b.dialogs) > 0 {
				b.Originator = b.dialogs[0]
			} else {
				b.Originator = nil
			}
		}
	}

	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	// Check are both answered. On failure roll back the append so the
	// bridge does not keep a stale half-ready dialog.
	for _, dd := range b.dialogs {
		if err := dd.Media().checkMediaUsable(); err != nil {
			rollbackLast()
			return fmt.Errorf("dialog session not answered %q: %w", dd.ID(), err)
		}
	}

	done, err := b.claimProxyUnsafe()
	if err != nil {
		rollbackLast()
		return err
	}
	go func() {
		defer func(start time.Time) {
			b.log.Debug("Proxy media setup", "dur", time.Since(start).String())
		}(time.Now())
		// done is the generation channel; releaseProxy closes it exactly once.
		defer b.releaseProxy(done)
		if err := b.proxyMedia(); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}

			b.log.Error("Proxy media stopped", "error", err)
		}
	}()
	return nil
}

// claimProxyUnsafe marks a proxy generation as running so Close/Remove can
// interrupt and wait for it. Caller must hold b.mu. Fails when the bridge is
// closed or a proxy is already running, which makes double-start impossible
// from every entry point (auto-start, ProxyMedia, ProxyMediaControl).
func (b *Bridge) claimProxyUnsafe() (chan struct{}, error) {
	if b.closed {
		return nil, ErrBridgeClosed
	}
	if b.isProxyRunning {
		return nil, fmt.Errorf("proxy media already running")
	}
	b.isProxyRunning = true
	b.proxyDone = make(chan struct{})
	return b.proxyDone, nil
}

// releaseProxy ends the claimed generation and wakes Close/Remove waiters.
func (b *Bridge) releaseProxy(done chan struct{}) {
	b.mu.Lock()
	b.isProxyRunning = false
	select {
	case <-done:
	default:
		close(done)
	}
	b.mu.Unlock()
}

// Remove removes a dialog by ID. Missing IDs are a no-op nil.
// If a proxy is running it is interrupted through the write gate; removal
// still succeeds on pause errors (e.g. already hung up). Originator falls
// back to the first remaining dialog.
func (b *Bridge) Remove(d Call) error {
	if d == nil {
		return fmt.Errorf("bridge: dialog session is nil")
	}
	b.ensureInit()
	id := d.ID()
	b.mu.Lock()
	idx := -1
	for i, dd := range b.dialogs {
		if dd.ID() == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		b.mu.Unlock()
		return nil
	}
	running := b.isProxyRunning
	done := b.proxyDone
	snapshot := slices.Clone(b.dialogs)
	b.mu.Unlock()

	var releases []StopFunc
	var releaseErr error
	if running {
		releases = interruptBridgeProxy(snapshot)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		for _, release := range releases {
			releaseErr = errors.Join(releaseErr, release())
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for i, dd := range b.dialogs {
		if dd.ID() == id {
			b.dialogs = slices.Delete(b.dialogs, i, i+1)
			break
		}
	}
	if b.Originator != nil && b.Originator.ID() == id {
		if len(b.dialogs) > 0 {
			b.Originator = b.dialogs[0]
		} else {
			b.Originator = nil
		}
	}
	return releaseErr
}

// Close stops any running proxy and clears the bridge. Idempotent; Add after
// Close returns ErrBridgeClosed.
func (b *Bridge) Close() error {
	b.ensureInit()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	running := b.isProxyRunning
	done := b.proxyDone
	snapshot := slices.Clone(b.dialogs)
	b.mu.Unlock()

	var releaseErr error
	if running {
		releases := interruptBridgeProxy(snapshot)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		for _, release := range releases {
			releaseErr = errors.Join(releaseErr, release())
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.dialogs = nil
	b.Originator = nil
	return releaseErr
}

// interruptBridgeProxy pauses writers to surface media.ErrWritePaused in the
// proxy copy loop. Callers must call the returned releases after the proxy
// exits (or times out) to restore writers.
func interruptBridgeProxy(dialogs []Call) []StopFunc {
	var releases []StopFunc
	for _, d := range dialogs {
		release, err := d.Media().PauseAudioWrite()
		if err != nil {
			continue
		}
		releases = append(releases, release)
	}
	return releases
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
	b.ensureInit()
	b.mu.Lock()
	n := len(b.dialogs)
	if n < 2 {
		b.mu.Unlock()
		return fmt.Errorf("number of dialogs must equal to 2")
	}

	if b.WaitDialogsNum < 3 {
		b.mu.Unlock()
		return fmt.Errorf("you are already running proxy media. Increase WaitDialogsNum")
	}

	done, err := b.claimProxyUnsafe()
	b.mu.Unlock()
	if err != nil {
		return err
	}
	// Claimed like the background paths so Close/Remove can interrupt and
	// wait for this proxy too.
	defer b.releaseProxy(done)

	return b.proxyMedia()
}

// ProxyMediaControl starts proxy in background and allows to stop proxy at any time.
// Stop should be called once and it is not needed to be called if call is terminating
//
// Stop interrupts the proxy through the write gate of the bridged dialogs and
// restores the writers afterwards.
//
// Experimental
func (b *Bridge) ProxyMediaControl() (StopFunc, error) {
	b.ensureInit()
	// Same precondition as ProxyMedia, proxyMedia indexes dialogs [0] and [1]
	b.mu.Lock()
	if len(b.dialogs) < 2 {
		b.mu.Unlock()
		return nil, fmt.Errorf("number of dialogs must equal to 2")
	}
	done, err := b.claimProxyUnsafe()
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}

	proxyErr := make(chan error, 1)
	go func() {
		err := b.proxyMedia()
		// Release before signaling: a stopF caller can immediately start a new
		// proxy generation without racing the flag.
		b.releaseProxy(done)
		proxyErr <- err
	}()

	// A second stop call must not pause the writers again nor block on the
	// already-consumed proxyErr channel: repeated calls share the first
	// result.
	var stopOnce sync.Once
	var stopErr error
	stopF := func() error {
		stopOnce.Do(func() {
			// Interrupt the proxy through the write gate: the in-flight write
			// completes and the next write surfaces media.ErrWritePaused, which
			// the proxy treats as a clean stop.
			b.mu.Lock()
			dialogs := slices.Clone(b.dialogs)
			b.mu.Unlock()

			rels := make([]StopFunc, 0, len(dialogs))
			var pauseErrs error
			for _, d := range dialogs {
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
				pauseErrs = errors.Join(pauseErrs, release())
			}
			stopErr = errors.Join(pauseErrs, err)
		})
		return stopErr
	}

	return stopF, nil
}

// proxyMedia starts routine to proxy media between
// Should be called after having 2 or more participants
func (b *Bridge) proxyMedia() error {
	var err error
	log := b.log

	// Snapshot the dialog list: AddCall may append (and fail with
	// "only support 2 party") while proxying is already running.
	b.mu.Lock()
	dialogs := slices.Clone(b.dialogs)
	discardQueued := b.DiscardQueuedOnStart
	b.mu.Unlock()
	if len(dialogs) < 2 {
		return fmt.Errorf("number of dialogs must equal to 2")
	}

	// Start from live media: anything queued before this point is stale by
	// definition (the proxy is the first consumer) and replaying it at the
	// line rate would add permanent delay. Fail-open on errors.
	if discardQueued {
		b.discardQueuedRTP(dialogs)
	}

	m1 := dialogs[0].Media()
	m2 := dialogs[1].Media()

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
	startProxy := func(mFrom, mTo *Media) error {
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

// discardQueuedRTP drops RTP that queued on each leg's socket before the proxy
// loop could read it. Errors are fail-open: the bridge proceeds and forwards
// whatever remains queued, which is the pre-existing behavior.
func (b *Bridge) discardQueuedRTP(dialogs []Call) {
	for _, d := range dialogs {
		n, err := d.Media().DiscardQueuedRTP()
		if err != nil {
			b.log.Warn("discarding queued RTP before proxy failed",
				"dialog", d.ID(), "packets", n, "error", err)
			continue
		}
		if n > 0 {
			b.log.Debug("discarded queued RTP before proxy",
				"dialog", d.ID(), "packets", n)
		}
	}
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

func (b *Bridge) proxyMediaWithDTMF(m1 *Media, m2 *Media) error {
	dtmfReader := DTMFReader{}
	r, p1, err := m1.ReaderWithProps(WithReaderDTMF(&dtmfReader))
	if err != nil {
		return err
	}
	dtmfWriter := DTMFWriter{}
	w, p2, err := m2.WriterWithProps(WithWriterDTMF(&dtmfWriter))
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
	if errors.Is(err, media.ErrWritePaused) {
		// Stopped through the write gate (Remove/Close/stopF).
		return nil
	}
	return err
}

// BridgeMix is mixing audio when having 2 or more parties.
//
// Experimental: not fully tested yet
type BridgeMix struct {
	mu      sync.Mutex
	dialogs []Call

	// mixMutateMu serializes Add/Remove across the stop-wait-restart cycle.
	// mixStopWait releases b.mu while waiting for the mix to drain, so without
	// it two mutators would wait on the same generation and both restart the
	// mix (double loop over the same streams). Never held by the mix loop.
	mixMutateMu sync.Mutex

	// mixWG and mixDone track the CURRENT mix generation. A fresh WaitGroup
	// is created per mix (re)start, so a stopper waiting on a drained
	// generation can never be wedged by a later mix reusing the same
	// WaitGroup (the Add-after-Wait misuse that stranded teardowns).
	mixWG    *sync.WaitGroup
	mixDone  chan struct{}
	mixState int
	// pauseReleases holds reader gate releases taken by mixStop, applied by
	// mixStopWait after the mix goroutines rejoin. Guarded by mu.
	pauseReleases []StopFunc

	// WaitDialogsNum is just helper flag when to start proxy
	WaitDialogsNum int
	// RealtimeReader is almost always nesessary if you are delaying audio streaming(mixing) in bridge
	RealtimeReader bool
	Poll           bool
	log            *slog.Logger

	// initOnce makes Init idempotent and lets the zero value be used directly.
	initOnce sync.Once
}

// NewBridgeMix creates an N-party mixer with realtime reading and polling enabled.
func NewBridgeMix() *BridgeMix {
	b := BridgeMix{
		RealtimeReader: true,
		Poll:           true,
	}
	b.Init()
	return &b
}

// Init initializes bridge struct. Use only if construct bridge with struct
// or use NewBridgeMix. It is idempotent and the zero value is usable without
// calling it.
func (b *BridgeMix) Init() {
	b.initOnce.Do(func() {
		b.log = media.DefaultLogger().With("caller", "bridge_mix")
	})
}

// ensureInit prepares the zero value on first use.
func (b *BridgeMix) ensureInit() {
	b.Init()
}

func (b *BridgeMix) String() string {
	b.ensureInit()
	b.mu.Lock()
	defer b.mu.Unlock()

	var str strings.Builder
	fmt.Fprintf(&str, "state: %d", b.mixState)
	str.WriteString(" dialogs:[")
	for _, d := range b.dialogs {
		str.WriteString(" ")
		str.WriteString(d.ID())
	}
	str.WriteString("]")
	return str.String()
}

// Add adds an answered dialog to the mixer; mixing restarts with the new set.
func (b *BridgeMix) Add(d Call) error {
	if d == nil {
		return fmt.Errorf("bridge mix: dialog session is nil")
	}
	b.ensureInit()
	b.mixMutateMu.Lock()
	defer b.mixMutateMu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()

	switch state := d.SIP().LoadState(); state {
	case sip.DialogStateEstablished, sip.DialogStateConfirmed:
		// Answered: Established means the 2xx was sent and the ACK may still
		// be in flight; media is already negotiated at that point.
	default:
		return fmt.Errorf("dialog must be answered before adding into bridge")
	}

	// Stop any current mixing
	b.log.Debug("Stoping mix", "dialog", d.ID())
	if err := b.mixStopWait(); err != nil {
		return fmt.Errorf("failed to stop current mixing: %w", err)
	}

	b.dialogs = append(b.dialogs, d)
	b.log.Debug("Added dialog", "dialog", d.ID(), "total", len(b.dialogs))
	if err := b.mixStart(); err != nil {
		// Add must be atomic: if the mix could not be (re)started (e.g. media
		// torn down by a concurrent BYE), roll the dialog back out of the
		// bridge instead of leaving a stuck entry no one will remove.
		for i, dd := range b.dialogs {
			if dd.ID() == d.ID() {
				b.dialogs = append(b.dialogs[:i], b.dialogs[i+1:]...)
				break
			}
		}
		b.log.Debug("Added dialog rolled back", "dialog", d.ID(), "total", len(b.dialogs))
		return fmt.Errorf("failed to start mixing: %w", err)
	}
	return nil
}

// Remove removes a dialog from the mixer and restarts mixing with the rest.
func (b *BridgeMix) Remove(d Call) error {
	if d == nil {
		return fmt.Errorf("bridge mix: dialog session is nil")
	}
	b.ensureInit()
	b.mixMutateMu.Lock()
	defer b.mixMutateMu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	dialogID := d.ID()

	var dialog Call
	for _, d := range b.dialogs {
		if d.ID() == dialogID {
			dialog = d
			break
		}
	}
	if dialog == nil {
		return nil
	}

	b.log.Debug("Stoping mix", "dialog", dialog.ID())

	// Remove the dialog even if stopping the mix reports errors: a failed
	// pause (e.g. media already closed by a concurrent BYE) must never leave
	// the entry stuck in the bridge. The mix drains on its own via closed
	// media in that case, and mixStopWait still waits for it below.
	mixStopErr := b.mixStopWait()

	// NOTE: mixStopWait unlocks so we can not do any update before
	for i, d := range b.dialogs {
		if d.ID() == dialogID {
			b.dialogs = append(b.dialogs[:i], b.dialogs[i+1:]...)
			break
		}
	}

	b.log.Debug("Removed dialog", "dialog", dialog.ID(), "total", len(b.dialogs))
	if err := b.mixStart(); err != nil {
		return errors.Join(mixStopErr, err)
	}
	return mixStopErr
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
	// NOTE: pause errors are reported but must not skip the wait: even with a
	// failed pause the mix drains on its own (closed media errors the reads).
	stopInProgress, stopErr := b.mixStop()

	if stopInProgress {
		// Wait on the captured generation: it is closed exactly once when
		// every goroutine of that mix has exited. Later mixes run on their
		// own generation, so they can not wedge this wait.
		done := b.mixDone
		b.mu.Unlock()
		if done != nil {
			<-done
		}
		b.mu.Lock()
	}
	// Release read pauses (mixStop paused the dialogs to stop the mix)
	var allErros error
	for _, release := range b.pauseReleases {
		allErros = errors.Join(allErros, release())
	}
	b.pauseReleases = nil
	if stopErr != nil {
		allErros = errors.Join(allErros, fmt.Errorf("failed to stop current mixing: %w", stopErr))
	}
	return allErros
}

func (b *BridgeMix) mixStop() (bool, error) {
	switch b.mixState {
	case 1:
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
	case 2:
		// Another goroutine already initiated the stop and is waiting in
		// mixStopWait for the mix goroutine to drain. We must wait for it as
		// well: proceeding now would mutate b.dialogs while the stop is still
		// in flight and skip the deferred mixStart, stranding state=2 with
		// dialogs left in the bridge.
		return true, nil
	}
	// state == 0: no mix running
	return false, nil
}

func (b *BridgeMix) mixStart() error {
	if b.mixState != 0 {
		// 1: a mix is already running (another mutator restarted it while
		// this one was waiting) - starting a second loop would have two
		// generations reading the same streams. 2: a stop is in progress.
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
	wg := &sync.WaitGroup{}
	rwStreams, err := func() ([]*bridgePCMStream, error) {
		rwStreams := make([]*bridgePCMStream, len(b.dialogs))
		firstDialogCodec := media.Codec{}

		for i, d := range b.dialogs {
			rwStreams[i] = &bridgePCMStream{}
			if err := b.addDialogStream(ctx, d, rwStreams[i], wg, &firstDialogCodec, poll); err != nil {
				return nil, err
			}
		}
		return rwStreams, nil
	}()
	if err != nil {
		cancelPoll()
		return err
	}

	// Start new mix on a fresh generation. The reaper closes mixDone once the
	// mix loop and all poll stream readers have exited; mixStopWait blocks on
	// it instead of a shared WaitGroup (Add-after-Wait reuse race).
	done := make(chan struct{})
	b.mixWG = wg
	b.mixDone = done
	b.stateWriteUnsafe(1)
	// Add for the main loop must precede the reaper so the counter can never
	// be observed as drained before the loop is even started.
	wg.Add(1)
	go func(wg *sync.WaitGroup, done chan struct{}) {
		wg.Wait()
		close(done)
	}(wg, done)
	go func(rwStreams []*bridgePCMStream) {
		defer cancelPoll()
		defer wg.Done()
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
		// Just keep streaming: drain and discard so the RTP flow (and RTCP
		// stats) stays alive until the read fails. Accumulating the payload
		// would grow unboundedly over a long call.
		r := rwStreams[0]
		if !poll {
			buf := make([]byte, media.RTPBufSize)
			for {
				if _, err := r.r.Read(buf); err != nil {
					return err
				}
			}
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
			b.log.Debug("Nothing read, delaying read")
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
			b.log.Debug("Writing stream", "i", i, "stream", w.id, "n", n, "err", err)
			if err != nil {
				// Detect is this Deadline or EOF error caused by stream exiting
				if errors.Is(err, os.ErrDeadlineExceeded) {
					state := b.stateRead()
					if state != 1 {
						// We are stopped
						return err
					}

					// Mixing has been stopped or network problem
					w.isMarkGone = true
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
	// control survives media updates (re-INVITE)
	dm *Media
	// read buf
	buf []byte
	n   int

	pipeRead   chan int
	pipeWrite  chan []byte
	isMarkGone bool
}

func (b *BridgeMix) addDialogStream(ctx context.Context, d Call, stream *bridgePCMStream, wg *sync.WaitGroup, firstDialogCodec *media.Codec, poll bool) error {
	m := d.Media()

	r, p, err := m.ReaderWithProps()
	if err != nil {
		return err
	}

	if firstDialogCodec.SampleRate == 0 {
		*firstDialogCodec = p.Codec
	}

	if firstDialogCodec.SampleRate != p.Codec.SampleRate || firstDialogCodec.SampleDur != p.Codec.SampleDur {
		return fmt.Errorf("codec mismatch: resampling or transcoding is not supported")
	}

	rtr, err := func() (io.Reader, error) {
		if !b.RealtimeReader {
			return r, nil
		}

		if rtr, ok := r.(*media.RTPRealTimeReader); ok {
			return rtr, nil
		}

		rtr, err := media.NewRTPRealTimeReader(r, m.rtpPacketReader, p.Codec)
		if err != nil {
			return nil, err
		}
		m.setAudioReader(rtr)
		return rtr, nil
	}()
	if err != nil {
		return err
	}

	// Attach PCM decoder
	pcmReader := audio.PCMDecoderReader{}
	if err := pcmReader.Init(p.Codec, rtr); err != nil {
		return err
	}

	// Now do write stream
	w, wprops, err := m.WriterWithProps()
	if err != nil {
		return err
	}

	pcmWriter := audio.PCMEncoderWriter{}
	if err := pcmWriter.Init(wprops.Codec, w); err != nil {
		return err
	}

	*stream = bridgePCMStream{
		r:         &pcmReader,
		w:         &pcmWriter,
		dm:        m,
		id:        m.rtpPacketWriter.SSRC,
		buf:       make([]byte, media.RTPBufSize),
		pipeRead:  make(chan int),
		pipeWrite: make(chan []byte),
	}

	if poll {
		// We do buffering because initial packet can be read oner than actual mixing has started
		wg.Add(1)
		b.log.Debug("poll: starting stream", "stream.id", stream.id)
		go func(s *bridgePCMStream) {
			defer wg.Done()

			bufPtr := bridgeReadPool.Get().(*[]byte)
			defer bridgeReadPool.Put(bufPtr)

			defer close(s.pipeWrite)

			buf := *bufPtr
			for {
				n, err := s.r.Read(buf)
				if err != nil {
					b.log.Debug("poll: stopped with error", "error", err, "stream.id", stream.id)
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
					b.log.Debug("poll: stream context canceled", "stream.id", stream.id)
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
	clear(mixedBuf)

	if !poll {
		// If are not polling data then we need todo direct read
		err := func() error {
			for i, r := range rwStreams {
				ms := r.dm.currentMediaSession()
				if ms == nil {
					continue
				}
				// Best effort: the direct read below tolerates an already
				// stopped reader.
				_ = ms.StopRTP(1, 1*time.Millisecond)

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
			if r.isMarkGone {
				handledStreams--
				continue
			}
			r.n = 0 // Make sure it is zero

			select {
			case bw, more := <-r.pipeWrite:
				if !more {
					r.isMarkGone = true
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
