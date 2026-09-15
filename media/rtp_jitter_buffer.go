// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

const (
	defaultRTPJitterBufferDelayPackets = 3
	defaultRTPJitterBufferMaxPackets   = 10
)

// ErrInvalidPacketDuration is returned by NewRTPJitterBuffer when the packet
// duration derived from the codec is not greater than zero.
var ErrInvalidPacketDuration = errors.New("media: rtp jitter buffer packetDuration must be greater than zero")

var rtpJitterDebug = envBool("JITTER_DEBUG")

// RTPJitterBufferOptions configures a fixed RTP jitter buffer.
type RTPJitterBufferOptions struct {
	// DelayPackets is the initial fixed playout delay in packets. If unset, 3 is used.
	DelayPackets int
	// MaxPackets caps buffered packets and the forward reordering window. If unset, 10 is used.
	MaxPackets int
}

// RTPJitterBufferStatistics contains simple counters for buffer decisions.
type RTPJitterBufferStatistics struct {
	PacketsRead        uint64
	PacketsReleased    uint64
	PacketsLost        uint64
	PacketsLate        uint64
	PacketsDuplicate   uint64
	PacketsDropped     uint64
	SSRCResets         uint64
	LastSequenceNumber uint16
}

type rtpJitterBufferStats struct {
	packetsRead        atomic.Uint64
	packetsReleased    atomic.Uint64
	packetsLost        atomic.Uint64
	packetsLate        atomic.Uint64
	packetsDuplicate   atomic.Uint64
	packetsDropped     atomic.Uint64
	ssrcResets         atomic.Uint64
	lastSequenceNumber atomic.Uint32
}

type rtpJitterSlot struct {
	raw    []byte
	packet rtp.Packet
	n      int
	ssrc   uint32
	seq    uint16
}

type rtpJitterInput struct {
	slot int
	err  error
}

// RTPJitterBuffer is a fixed-delay, single-consumer RTPReader wrapper.
//
// It sits after RTPSession and before RTPPacketReader, so RTPSession observes
// true network arrival while downstream readers get reordered packets.
type RTPJitterBuffer struct {
	// reader is the upstream network-facing RTP source. Atomic so a media
	// update can retarget it (updateRTPSession) without stopping the pump.
	reader atomic.Pointer[RTPReader]

	// packetDuration controls the interval between playout decisions.
	packetDuration time.Duration
	// delayPackets is the number of queued packets required to start playout early.
	delayPackets int
	// maxPackets is both the queue capacity and accepted forward sequence window.
	maxPackets int

	// input transfers ownership of filled slot indexes from readLoop to ReadRTP.
	input chan rtpJitterInput
	// freeSlots transfers ownership of reusable slot indexes back to readLoop.
	freeSlots chan int
	// done is closed by Close to stop internal channel operations and delivery.
	done chan struct{}
	// closeOnce makes Close idempotent.
	closeOnce sync.Once
	// startOnce ensures that only one upstream reader goroutine is launched.
	startOnce sync.Once
	// pauseC delivers the read pause signal to ReadRTP. nil pointer means not
	// paused. Set through setReadPause (readPauser); the pump goroutine is
	// never interrupted - pause only surfaces at the ReadRTP select.
	pauseC atomic.Pointer[<-chan struct{}]

	// slots contains all reusable packet metadata and packet-sized byte regions.
	slots []rtpJitterSlot
	// sequence maps sequenceNumber % maxPackets to an occupied slot index, or -1.
	sequence []int
	// queued is the number of occupied entries in sequence.
	queued int

	// ssrc identifies the RTP source currently accepted by the buffer.
	ssrc uint32
	// ssrcSet reports whether the buffer has learned an RTP source.
	isSSRCSet bool
	// expectedSeq is the next sequence number scheduled for playout.
	expectedSeq uint16
	// expectedSet reports whether expectedSeq has been initialized.
	isExpectedSet bool

	// initialTimer limits how long startup waits to collect delayPackets.
	initialTimer *time.Timer
	// playoutTimer schedules the next packet release or loss decision.
	playoutTimer *time.Timer
	// playout reports whether the initial buffering phase has completed.
	isPlayout bool
	// releaseNow reports that the next expected sequence may be processed now.
	isReleaseNow bool

	// inputClosed reports that readLoop has stopped producing packet events.
	isInputClosed bool
	// readErr stores the terminal upstream error returned after queued packets drain.
	readErr error
	// lastArrivalTime is used only for JITTER_DEBUG receive-side diagnostics.
	lastArrivalTime time.Time
	// lastArrivalSeq is the previous RTP sequence observed by readLoop.
	lastArrivalSeq uint16
	// lastArrivalSet reports whether receive-side debug arrival state is initialized.
	isLastArrivalSet bool
	// stats uses atomics so Statistics may run concurrently with ReadRTP.
	stats rtpJitterBufferStats
}

// NewRTPJitterBuffer creates a fixed jitter buffer over another RTPReader.
// It returns ErrInvalidPacketDuration if packetDuration is not greater than zero.
func NewRTPJitterBuffer(reader RTPReader, packetDuration time.Duration, opts RTPJitterBufferOptions) (*RTPJitterBuffer, error) {
	if packetDuration <= 0 {
		return nil, ErrInvalidPacketDuration
	}
	if opts.DelayPackets <= 0 {
		opts.DelayPackets = defaultRTPJitterBufferDelayPackets
	}
	if opts.MaxPackets <= 0 {
		opts.MaxPackets = defaultRTPJitterBufferMaxPackets
	}
	if opts.MaxPackets < opts.DelayPackets {
		opts.MaxPackets = opts.DelayPackets
	}
	if opts.DelayPackets > opts.MaxPackets {
		opts.DelayPackets = opts.MaxPackets
	}

	// One extra slot lets the network reader finish one read while the configured
	// jitter window is full. All packet storage is allocated once here.
	slotCount := opts.MaxPackets + 1
	data := make([]byte, slotCount*RTPBufSize)
	slots := make([]rtpJitterSlot, slotCount)
	freeSlots := make(chan int, slotCount)
	for i := range slots {
		slots[i].raw = data[i*RTPBufSize : (i+1)*RTPBufSize]
		freeSlots <- i
	}

	sequence := make([]int, opts.MaxPackets)
	for i := range sequence {
		sequence[i] = -1
	}

	initialTimer := time.NewTimer(time.Hour)
	initialTimer.Stop()
	playoutTimer := time.NewTimer(time.Hour)
	playoutTimer.Stop()

	j := &RTPJitterBuffer{
		packetDuration: packetDuration,
		delayPackets:   opts.DelayPackets,
		maxPackets:     opts.MaxPackets,
		input:          make(chan rtpJitterInput, slotCount),
		freeSlots:      freeSlots,
		done:           make(chan struct{}),
		slots:          slots,
		sequence:       sequence,
		initialTimer:   initialTimer,
		playoutTimer:   playoutTimer,
	}
	j.reader.Store(&reader)
	return j, nil
}

// updateRTPSession retargets the upstream session when the owning dialog
// installs a new media session (re-INVITE/hold). It implements the updater
// consumed by RTPPacketReader.UpdateRTPSession, which keeps the jitter buffer
// in the reader chain instead of replacing it with the bare session.
func (j *RTPJitterBuffer) updateRTPSession(rtpSess *RTPSession) {
	old := j.reader.Load()
	var reader RTPReader = rtpSess
	j.reader.Store(&reader)

	if old == nil {
		return
	}
	// Wake an in-flight read on the abandoned session when the socket
	// changed; a fork sharing the parent conn does not need the poke.
	if s, ok := (*old).(*RTPSession); ok && s.Sess.rtpConn != rtpSess.Sess.rtpConn {
		_ = s.Sess.rtpConn.SetReadDeadline(time.Now())
	}
}

// ReadRTP implements RTPReader. Calls must not overlap.
func (j *RTPJitterBuffer) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	j.start()

	for {
		if j.isPlayout && j.isReleaseNow {
			position := int(j.expectedSeq) % j.maxPackets
			slotIndex := j.sequence[position]
			if slotIndex >= 0 && j.slots[slotIndex].seq == j.expectedSeq {
				slot := &j.slots[slotIndex]
				if len(buf) < slot.n {
					return 0, io.ErrShortBuffer
				}

				j.sequence[position] = -1
				j.queued--
				j.expectedSeq++
				j.isReleaseNow = false
				j.resetPlayoutTimer()

				n := copy(buf, slot.raw[:slot.n])
				err := RTPUnmarshal(buf[:n], p)
				j.recycleSlot(slotIndex)
				if err != nil {
					return 0, err
				}

				j.stats.packetsReleased.Add(1)
				j.stats.lastSequenceNumber.Store(uint32(p.SequenceNumber))
				return n, nil
			}

			j.stats.packetsLost.Add(1)
			j.expectedSeq++
			j.isReleaseNow = false
			j.resetPlayoutTimer()
		}

		if j.isInputClosed && j.queued == 0 {
			j.stopTimers()
			if j.readErr != nil {
				return 0, j.readErr
			}
			return 0, io.EOF
		}

		var initialC <-chan time.Time
		if !j.isPlayout && j.isExpectedSet {
			initialC = j.initialTimer.C
		}

		var playoutC <-chan time.Time
		if j.isPlayout && !j.isReleaseNow {
			playoutC = j.playoutTimer.C
		}

		var pauseC <-chan struct{}
		if p := j.pauseC.Load(); p != nil {
			pauseC = *p
		}

		select {
		case <-j.done:
			j.stopTimers()
			return 0, io.ErrClosedPipe

		case <-pauseC:
			// Transient pause/interrupt: playout state is kept, the next
			// ReadRTP resumes the schedule where it stopped.
			return 0, ErrReadPaused

		case input, ok := <-j.input:
			if !ok {
				j.isInputClosed = true
				continue
			}
			if input.err != nil {
				j.readErr = input.err
				j.isInputClosed = true
				continue
			}

			j.handleSlot(input.slot)
			if !j.isPlayout && j.queued >= j.delayPackets {
				j.debugPlayoutStarted("delay_packets")
				j.startPlayout()
			}

		case <-initialC:
			j.debugPlayoutStarted("initial_timer")
			j.startPlayout()

		case <-playoutC:
			j.isReleaseNow = true
		}
	}
}

func (j *RTPJitterBuffer) start() {
	j.startOnce.Do(func() {
		go j.readLoop()
	})
}

func (j *RTPJitterBuffer) readLoop() {
	defer close(j.input)

	for {
		var slotIndex int
		select {
		case <-j.done:
			return
		case slotIndex = <-j.freeSlots:
		}

		slot := &j.slots[slotIndex]
		reader := *j.reader.Load()
		n, err := reader.ReadRTP(slot.raw, &slot.packet)
		if err != nil {
			// A media update swaps the upstream session and pokes the old
			// conn to unblock this read; the timeout (or a key change that
			// fails decryption on the old session) must retry on the new
			// reader instead of terminating the pump.
			if current := *j.reader.Load(); current != reader {
				j.recycleSlot(slotIndex)
				continue
			}
			j.sendInput(rtpJitterInput{err: err})
			return
		}
		if n == 0 {
			j.recycleSlot(slotIndex)
			continue
		}

		slot.n = n
		slot.ssrc = slot.packet.SSRC
		slot.seq = slot.packet.SequenceNumber
		j.debugArrival(slot)
		if !j.sendInput(rtpJitterInput{slot: slotIndex}) {
			return
		}
	}
}

func (j *RTPJitterBuffer) sendInput(input rtpJitterInput) bool {
	select {
	case <-j.done:
		return false
	case j.input <- input:
		return true
	}
}

func (j *RTPJitterBuffer) handleSlot(slotIndex int) {
	slot := &j.slots[slotIndex]
	j.stats.packetsRead.Add(1)

	if !j.isSSRCSet || j.ssrc != slot.ssrc {
		if j.isSSRCSet {
			j.stats.ssrcResets.Add(1)
		}
		j.resetStream(slot.ssrc, slot.seq)
	}

	distance := int16(slot.seq - j.expectedSeq)
	if distance < 0 {
		if j.isPlayout {
			j.stats.packetsLate.Add(1)
			j.debugPacketDecision("late", slot, "behind_playout")
			j.recycleSlot(slotIndex)
			return
		}
		if !j.canMoveExpectedBack(slot.seq) {
			j.stats.packetsDropped.Add(1)
			j.debugPacketDecision("dropped", slot, "before_window")
			j.recycleSlot(slotIndex)
			return
		}
		j.expectedSeq = slot.seq
		distance = 0
	}

	if int(distance) >= j.maxPackets {
		j.stats.packetsDropped.Add(1)
		j.debugPacketDecision("dropped", slot, "beyond_window")
		j.recycleSlot(slotIndex)
		return
	}

	position := int(slot.seq) % j.maxPackets
	existing := j.sequence[position]
	if existing >= 0 {
		if j.slots[existing].seq == slot.seq {
			j.stats.packetsDuplicate.Add(1)
			j.debugPacketDecision("duplicate", slot, "same_sequence")
		} else {
			j.stats.packetsDropped.Add(1)
			j.debugPacketDecision("dropped", slot, "slot_collision")
		}
		j.recycleSlot(slotIndex)
		return
	}

	j.sequence[position] = slotIndex
	j.queued++
}

func (j *RTPJitterBuffer) canMoveExpectedBack(seq uint16) bool {
	for _, slotIndex := range j.sequence {
		if slotIndex < 0 {
			continue
		}
		distance := int16(j.slots[slotIndex].seq - seq)
		if distance < 0 || int(distance) >= j.maxPackets {
			return false
		}
	}
	return true
}

func (j *RTPJitterBuffer) resetStream(ssrc uint32, seq uint16) {
	j.clearSequence()
	j.ssrc = ssrc
	j.isSSRCSet = true
	j.expectedSeq = seq
	j.isExpectedSet = true
	j.isPlayout = false
	j.isReleaseNow = false
	j.stopAndDrainTimer(j.playoutTimer)
	j.resetTimer(j.initialTimer, time.Duration(j.delayPackets)*j.packetDuration)
}

func (j *RTPJitterBuffer) clearSequence() {
	for i, slotIndex := range j.sequence {
		if slotIndex < 0 {
			continue
		}
		j.sequence[i] = -1
		j.recycleSlot(slotIndex)
	}
	j.queued = 0
}

func (j *RTPJitterBuffer) startPlayout() {
	j.stopAndDrainTimer(j.initialTimer)
	j.isPlayout = true
	j.isReleaseNow = true
}

func (j *RTPJitterBuffer) resetPlayoutTimer() {
	j.resetTimer(j.playoutTimer, j.packetDuration)
}

func (j *RTPJitterBuffer) resetTimer(timer *time.Timer, duration time.Duration) {
	j.stopAndDrainTimer(timer)
	timer.Reset(duration)
}

func (j *RTPJitterBuffer) stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (j *RTPJitterBuffer) stopTimers() {
	j.stopAndDrainTimer(j.initialTimer)
	j.stopAndDrainTimer(j.playoutTimer)
}

func (j *RTPJitterBuffer) recycleSlot(slotIndex int) {
	select {
	case j.freeSlots <- slotIndex:
	case <-j.done:
	}
}

// setReadPause implements readPauser. Passing a closed channel surfaces
// ErrReadPaused from ReadRTP; nil restores normal operation. The upstream
// pump is never interrupted by a pause.
func (j *RTPJitterBuffer) setReadPause(paused <-chan struct{}) {
	j.pauseC.Store(&paused)
}

// Close stops jitter-buffer delivery. It does not close the injected reader.
func (j *RTPJitterBuffer) Close() error {
	j.closeOnce.Do(func() {
		close(j.done)
		j.stopTimers()
	})
	return nil
}

// Statistics returns a race-safe snapshot of jitter buffer counters.
func (j *RTPJitterBuffer) Statistics() RTPJitterBufferStatistics {
	return RTPJitterBufferStatistics{
		PacketsRead:        j.stats.packetsRead.Load(),
		PacketsReleased:    j.stats.packetsReleased.Load(),
		PacketsLost:        j.stats.packetsLost.Load(),
		PacketsLate:        j.stats.packetsLate.Load(),
		PacketsDuplicate:   j.stats.packetsDuplicate.Load(),
		PacketsDropped:     j.stats.packetsDropped.Load(),
		SSRCResets:         j.stats.ssrcResets.Load(),
		LastSequenceNumber: uint16(j.stats.lastSequenceNumber.Load()),
	}
}

func (j *RTPJitterBuffer) debugArrival(slot *rtpJitterSlot) {
	if !rtpJitterDebug {
		return
	}

	now := time.Now()
	if j.isLastArrivalSet {
		arrivalDelta := now.Sub(j.lastArrivalTime)
		if arrivalDelta > j.packetDuration+j.packetDuration/2 {
			jitterDebugf("event=delayed_arrival seq=%d prev_seq=%d arrival_delta=%s packet_duration=%s",
				slot.seq,
				j.lastArrivalSeq,
				arrivalDelta,
				j.packetDuration,
			)
		}

		expectedNext := j.lastArrivalSeq + 1
		if slot.seq != expectedNext {
			jitterDebugf("event=out_of_sequence seq=%d prev_seq=%d expected_next=%d arrival_delta=%s",
				slot.seq,
				j.lastArrivalSeq,
				expectedNext,
				arrivalDelta,
			)
		}
	}

	j.lastArrivalTime = now
	j.lastArrivalSeq = slot.seq
	j.isLastArrivalSet = true
}

func (j *RTPJitterBuffer) debugPacketDecision(event string, slot *rtpJitterSlot, reason string) {
	if !rtpJitterDebug {
		return
	}

	jitterDebugf("event=%s seq=%d expected_seq=%d queued=%d delay_packets=%d max_packets=%d reason=%s",
		event,
		slot.seq,
		j.expectedSeq,
		j.queued,
		j.delayPackets,
		j.maxPackets,
		reason,
	)
}

func (j *RTPJitterBuffer) debugPlayoutStarted(reason string) {
	if !rtpJitterDebug {
		return
	}

	jitterDebugf("event=playout_started queued=%d delay_packets=%d max_packets=%d reason=%s",
		j.queued,
		j.delayPackets,
		j.maxPackets,
		reason,
	)
}

func jitterDebugf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "JITTER_DEBUG "+format+"\n", args...)
}

func envBool(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

var _ RTPReader = (*RTPJitterBuffer)(nil)
var _ io.Closer = (*RTPJitterBuffer)(nil)
