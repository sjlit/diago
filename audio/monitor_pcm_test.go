package audio

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sjlit/diago/media"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rtpBuffer struct {
	buf []rtp.Packet
}

func (b *rtpBuffer) WriteRTP(p *rtp.Packet) error {
	b.buf = append(b.buf, *p)
	return nil
}

func TestMonitorPCMReaderWriter(t *testing.T) {
	codecR := media.CodecAudioAlaw

	audioAlawBuf := make([]byte, 4*160)
	_, err := EncodeAlawTo(audioAlawBuf, bytes.Repeat([]byte("0123456789"), media.CodecAudioAlaw.Samples16()*4/10))
	require.NoError(t, err)

	t.Run("Reader", func(t *testing.T) {
		rtpBufferReader := bytes.NewBuffer(audioAlawBuf)

		recording := bytes.NewBuffer([]byte{})
		mon := &MonitorPCMReader{}
		mon.Init(recording, codecR, rtpBufferReader)

		mon.Read(make([]byte, 160))
		mon.Read(make([]byte, 160))
		time.Sleep(3 * codecR.SampleDur) // 1 now comming and 2 delayed
		_, err = media.ReadAll(mon, 160)
		require.NoError(t, err)

		mon.Flush()

		// 2 Frames, 2 Silence, 2 Frames
		frameSize := codecR.Samples16()
		assert.Equal(t, 2*frameSize+2*frameSize+2*frameSize, recording.Len())
	})

	t.Run("Writer", func(t *testing.T) {
		// Lets
		recording := bytes.NewBuffer([]byte{})
		mon := &MonitorPCMWriter{}
		mon.Init(recording, codecR, bytes.NewBuffer([]byte{}))

		mon.Write(audioAlawBuf[:160])
		mon.Write(audioAlawBuf[160 : 2*160])
		time.Sleep(3 * codecR.SampleDur) // 1 now comming and 2 delayed
		_, err = media.WriteAll(mon, audioAlawBuf[2*160:], 160)
		require.NoError(t, err)

		mon.Flush()

		// 2 Frames, 2 Silence, 2 Frames
		frameSize := codecR.Samples16()
		assert.Equal(t, 2*frameSize+2*frameSize+2*frameSize, recording.Len())
	})

}

func TestMonitorPCMReaderWriterStopped(t *testing.T) {
	codecR := media.CodecAudioAlaw

	audioAlawBuf := make([]byte, 4*160)
	_, err := EncodeAlawTo(audioAlawBuf, bytes.Repeat([]byte("0123456789"), media.CodecAudioAlaw.Samples16()*4/10))
	require.NoError(t, err)

	t.Run("Reader", func(t *testing.T) {
		rtpBufferReader := bytes.NewBuffer(audioAlawBuf)

		recording := bytes.NewBuffer([]byte{})
		mon := &MonitorPCMReader{}
		mon.Init(recording, codecR, rtpBufferReader)

		n, err := mon.Read(make([]byte, 160))
		require.NoError(t, err)
		assert.Equal(t, 160, n)

		mon.Stop()

		_, err = media.ReadAll(mon, 160)
		require.NoError(t, err)
		require.NoError(t, mon.Flush())

		frameSize := codecR.Samples16()
		assert.Equal(t, frameSize, recording.Len())
	})

	t.Run("Writer", func(t *testing.T) {
		// Lets
		recording := bytes.NewBuffer([]byte{})
		mon := &MonitorPCMWriter{}
		mon.Init(recording, codecR, bytes.NewBuffer([]byte{}))

		n, err := mon.Write(audioAlawBuf[:160])
		require.NoError(t, err)
		assert.Equal(t, 160, n)

		mon.Stop()

		_, err = media.WriteAll(mon, audioAlawBuf[160:], 160)
		require.NoError(t, err)
		require.NoError(t, mon.Flush())

		frameSize := codecR.Samples16()
		assert.Equal(t, frameSize, recording.Len())
	})
}

// TestMonitorPCMFailOpen pins the fail-open write policy: recording is
// best-effort, so a sink failure (disk full, EBADF, ...) must not propagate
// into the media path. The sink here is a read-only *os.File: the bufio layer
// absorbs the first frames, and once the buffer flushes every write fails
// with EBADF - real IO, no mocks.
func TestMonitorPCMFailOpen(t *testing.T) {
	codec := media.CodecAudioAlaw
	alawFrame := make([]byte, 160)
	_, err := EncodeAlawTo(alawFrame, bytes.Repeat([]byte("0123456789"), codec.Samples16()/10))
	require.NoError(t, err)
	// Enough frames to push past RecordingFlushSize so the sink is hit.
	frames := (RecordingFlushSize / 320) + 5
	audioAlawBuf := bytes.Repeat(alawFrame, frames)

	// readOnlyFile returns an *os.File open O_RDONLY: every Write fails.
	readOnlyFile := func(t *testing.T) *os.File {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "sink-*")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		ro, err := os.OpenFile(f.Name(), os.O_RDONLY, 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ro.Close() })
		return ro
	}

	t.Run("ReaderPropagatesWithoutFailOpen", func(t *testing.T) {
		mon := &MonitorPCMReader{}
		require.NoError(t, mon.Init(readOnlyFile(t), codec, bytes.NewBuffer(audioAlawBuf)))

		var gotErr error
		for i := 0; i < frames && gotErr == nil; i++ {
			_, gotErr = mon.Read(make([]byte, 160))
		}
		require.Error(t, gotErr, "without FailOpen the sink error must reach the media path")
	})

	t.Run("WriterPropagatesWithoutFailOpen", func(t *testing.T) {
		mon := &MonitorPCMWriter{}
		require.NoError(t, mon.Init(readOnlyFile(t), codec, io.Discard))

		var gotErr error
		for i := 0; i < frames && gotErr == nil; i++ {
			_, gotErr = mon.Write(alawFrame)
		}
		require.Error(t, gotErr, "without FailOpen the sink error must reach the media path")
	})

	t.Run("ReaderSwallowsWriteError", func(t *testing.T) {
		mon := &MonitorPCMReader{}
		mon.FailOpen = true
		require.NoError(t, mon.Init(readOnlyFile(t), codec, bytes.NewBuffer(audioAlawBuf)))

		for range frames {
			n, err := mon.Read(make([]byte, 160))
			require.NoError(t, err, "FailOpen must keep the media path clean")
			assert.Equal(t, 160, n, "passthrough must keep delivering audio")
		}
		require.Error(t, mon.Err(), "Err must expose the degraded sink")
		// The sink is an O_RDONLY fd, so the recorded error must be the real
		// errno - not a generic or swallowed one - and it must stay
		// errors.Is-traceable through the degradation wrapper.
		require.ErrorIs(t, mon.Err(), syscall.EBADF)
	})

	t.Run("WriterSwallowsWriteError", func(t *testing.T) {
		mon := &MonitorPCMWriter{}
		mon.FailOpen = true
		require.NoError(t, mon.Init(readOnlyFile(t), codec, io.Discard))

		for range frames {
			n, err := mon.Write(alawFrame)
			require.NoError(t, err, "FailOpen must keep the media path clean")
			assert.Equal(t, 160, n, "passthrough must keep delivering audio")
		}
		require.Error(t, mon.Err(), "Err must expose the degraded sink")
		require.ErrorIs(t, mon.Err(), syscall.EBADF)
		assert.ErrorContains(t, mon.Err(), "recording degraded", "Err must carry degradation context")
	})

	t.Run("FlushStillReports", func(t *testing.T) {
		// Fail-open protects the media path only; the finalize path must
		// still surface the degraded sink so callers learn the recording is
		// incomplete.
		mon := &MonitorPCMWriter{}
		mon.FailOpen = true
		require.NoError(t, mon.Init(readOnlyFile(t), codec, io.Discard))

		for range frames {
			_, err := mon.Write(alawFrame)
			require.NoError(t, err)
		}
		require.Error(t, mon.Flush())
	})
}

// TestMonitorPCMStereoSpool pins the spool-file handling of the stereo
// monitor: the two per-direction raw files must land in SpoolDir (not
// os.TempDir) with owner-only permissions, as they hold undecoded call audio.
func TestMonitorPCMStereoSpool(t *testing.T) {
	spool := t.TempDir()

	mon := &MonitorPCMStereo{SpoolDir: spool}
	recording := bytes.NewBuffer([]byte{})
	require.NoError(t, mon.Init(recording, media.CodecAudioAlaw, media.CodecAudioAlaw, bytes.NewBuffer(nil), bytes.NewBuffer(nil)))

	for _, f := range []*os.File{mon.PCMFileRead, mon.PCMFileWrite} {
		require.Equal(t, spool, filepath.Dir(f.Name()), "spool file must live in SpoolDir")
		st, err := f.Stat()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "call audio must not be world-readable")
	}

	// Close removes the spool files and writes the wav header last.
	require.NoError(t, mon.Close())
	for _, f := range []*os.File{mon.PCMFileRead, mon.PCMFileWrite} {
		_, err := os.Stat(f.Name())
		assert.True(t, os.IsNotExist(err))
	}
}

// TestMonitorPCMStereoErr verifies Err aggregates both directions: a broken
// reader-side spool is visible through the stereo handle. The injected spool
// is a read-only file, so every flush to it fails - real IO.
func TestMonitorPCMStereoErr(t *testing.T) {
	codec := media.CodecAudioAlaw
	alawFrame := make([]byte, 160)
	_, err := EncodeAlawTo(alawFrame, bytes.Repeat([]byte("0123456789"), codec.Samples16()/10))
	require.NoError(t, err)
	frames := (RecordingFlushSize / 320) + 5

	badSpool, err := os.OpenFile(filepath.Join(t.TempDir(), "ro.raw"), os.O_RDONLY|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = badSpool.Close() })

	mon := &MonitorPCMStereo{PCMFileRead: badSpool}
	mon.MonitorPCMReader.FailOpen = true
	require.NoError(t, mon.Init(bytes.NewBuffer([]byte{}), codec, codec, bytes.NewBuffer(bytes.Repeat(alawFrame, frames)), bytes.NewBuffer(nil)))

	for range frames {
		_, err := mon.Read(make([]byte, 160))
		require.NoError(t, err, "fail-open reader must keep media flowing")
	}
	require.Error(t, mon.Err(), "stereo Err must expose the degraded reader side")
	assert.NoError(t, mon.MonitorPCMWriter.Err(), "writer side stays healthy")
}

// TestMonitorPCMStereoPauseResume pins the stop-collection / keep-passthrough
// contract of Pause/Resume: while paused, frames flowing through the monitor
// must still reach the underlying audio sink (the call is never interrupted),
// but their PCM must not be appended to the recording. Resume re-enables
// collection.
//
// During the pause the frames are issued but NOT flushed: while stopped,
// writePCM short-circuits, so the frames never even reach the recorder. This
// makes the "recording did not grow" assertion independent of wall-clock time
// (a Flush during the pause would pad the gap with silence by design - see
// writeSilenceUnsafe - and the assertion would depend on running under 40ms).
func TestMonitorPCMStereoPauseResume(t *testing.T) {
	codec := media.CodecAudioAlaw
	alawFrame := make([]byte, 160)
	_, err := EncodeAlawTo(alawFrame, bytes.Repeat([]byte("0123456789"), codec.Samples16()/10))
	require.NoError(t, err)

	passthrough := &bytes.Buffer{}
	mon := &MonitorPCMStereo{SpoolDir: t.TempDir()}
	require.NoError(t, mon.Init(bytes.NewBuffer([]byte{}), codec, codec, nil, passthrough))
	// Reader side pulls from an endless frame source so Read keeps returning
	// a full frame regardless of what was consumed.
	frameReader := &repeatReader{buf: alawFrame}
	mon.MonitorPCMReader.audioReader = frameReader

	spoolSize := func(f *os.File) int64 {
		t.Helper()
		st, err := f.Stat()
		require.NoError(t, err)
		return st.Size()
	}
	collect := func() {
		t.Helper()
		_, err := mon.Read(make([]byte, 160))
		require.NoError(t, err)
		_, err = mon.Write(alawFrame)
		require.NoError(t, err)
		require.NoError(t, mon.Flush())
	}
	// collectUnflushed is used while paused: no Flush, so the assertion can
	// never race the silence injector's wall-clock guard.
	collectUnflushed := func() {
		t.Helper()
		_, err := mon.Read(make([]byte, 160))
		require.NoError(t, err)
		_, err = mon.Write(alawFrame)
		require.NoError(t, err)
	}

	// Baseline: one frame collected per direction while running.
	collect()
	baseR, baseW := spoolSize(mon.PCMFileRead), spoolSize(mon.PCMFileWrite)
	require.NotZero(t, baseR)
	require.NotZero(t, baseW)
	passthroughBase := passthrough.Len()

	// Pause: passthrough continues on both sides, recording frozen. Frames
	// are issued without a Flush, so a non-growing spool means the frames
	// were skipped at writePCM, independent of how slow the test runs.
	mon.Pause()
	collectUnflushed()
	assert.Equal(t, baseR, spoolSize(mon.PCMFileRead), "paused read frames must not extend the recording")
	assert.Equal(t, baseW, spoolSize(mon.PCMFileWrite), "paused write frames must not extend the recording")
	assert.Equal(t, passthroughBase+160, passthrough.Len(), "write passthrough must still deliver while paused")

	// Resume: collection is live again.
	mon.Resume()
	collect()
	assert.Greater(t, spoolSize(mon.PCMFileRead), baseR, "resumed read frames must be recorded")
	assert.Greater(t, spoolSize(mon.PCMFileWrite), baseW, "resumed write frames must be recorded")

	require.NoError(t, mon.Close())
}

// TestMonitorPCMStereoCallerSpoolOwnership pins the ownership contract: spool
// files pre-set by the caller before Init are used by Init, but remain owned
// by the caller - Close/Init-rollback must leave them open, in place, and
// untouched. Only files Init created itself are closed and removed.
func TestMonitorPCMStereoCallerSpoolOwnership(t *testing.T) {
	codec := media.CodecAudioAlaw

	t.Run("CloseKeepsCallerFiles", func(t *testing.T) {
		dir := t.TempDir()
		callerRead, err := os.CreateTemp(dir, "caller-read-*.raw")
		require.NoError(t, err)
		callerWrite, err := os.CreateTemp(dir, "caller-write-*.raw")
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = callerRead.Close()
			_ = callerWrite.Close()
		})

		mon := &MonitorPCMStereo{PCMFileRead: callerRead, PCMFileWrite: callerWrite}
		require.NoError(t, mon.Init(bytes.NewBuffer(nil), codec, codec, bytes.NewBuffer(nil), bytes.NewBuffer(nil)))

		// Close must not close or remove the caller's files.
		require.NoError(t, mon.Close())
		assert.FileExists(t, callerRead.Name(), "caller-provided read spool must survive Close")
		assert.FileExists(t, callerWrite.Name(), "caller-provided write spool must survive Close")

		// The handle stays usable: a write must still reach the file - this
		// proves the fd was left open by Close.
		n, err := callerRead.Write([]byte("still-open"))
		require.NoError(t, err)
		assert.Equal(t, 10, n)

		// And the caller can close their own files afterwards.
		assert.NoError(t, callerRead.Close(), "caller fd must still be open and closable after Close")
		assert.NoError(t, callerWrite.Close(), "caller fd must still be open and closable after Close")
	})

	t.Run("InitFailureRollbackKeepsCallerFiles", func(t *testing.T) {
		dir := t.TempDir()
		callerRead, err := os.CreateTemp(dir, "caller-read-*.raw")
		require.NoError(t, err)
		t.Cleanup(func() { _ = callerRead.Close() })

		// The pre-set read spool is kept; the writer spool creation fails
		// (SpoolDir does not exist), which drives the rollback path.
		mon := &MonitorPCMStereo{PCMFileRead: callerRead, SpoolDir: filepath.Join(dir, "missing")}
		err = mon.Init(bytes.NewBuffer(nil), codec, codec, bytes.NewBuffer(nil), bytes.NewBuffer(nil))
		require.Error(t, err)
		assert.FileExists(t, callerRead.Name(), "caller-provided spool must survive Init rollback")
		assert.NoError(t, callerRead.Close(), "caller fd must still be open and closable after rollback")
	})
}

// repeatReader yields buf indefinitely so Read always returns a full frame,
// independent of how much the monitor consumes.
type repeatReader struct {
	buf  []byte
	last []byte
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := copy(p, r.buf)
	r.last = append(r.last[:0], p[:n]...)
	return n, nil
}

// TestMonitorPCMStereoInitCodec pins the codec policy of the stereo monitor:
// each direction decodes with its own codec, so reader PCMA + writer PCMU is
// a valid combination. What must be rejected is a timing mismatch - the two
// spools share one interleaved timeline, so sample rate and frame duration
// have to agree.
func TestMonitorPCMStereoInitCodec(t *testing.T) {
	recording := bytes.NewBuffer([]byte{})

	mon := &MonitorPCMStereo{SpoolDir: t.TempDir()}
	err := mon.Init(recording, media.CodecAudioAlaw, media.CodecAudioUlaw, bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	require.NoError(t, err, "same timing, different codec names must be accepted")
	require.NoError(t, mon.Close())

	bad := &MonitorPCMStereo{SpoolDir: t.TempDir()}
	err = bad.Init(recording, media.CodecAudioAlaw, media.CodecAudioOpus, bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	require.Error(t, err, "8k vs 48k cannot share one interleaved timeline")
}

// TestMonitorPCMStereoInitCodecNumChannels pins the channel-count policy: the
// interleave pass treats each direction as a mono 16-bit sample stream and
// WavWriter hardcodes NumChans=2, so a multichannel codec (opus) would produce
// a wrongly laid out WAV. Only mono codecs are accepted.
//
// The test bypasses decoder init (which needs the with_opus_c build tag) by
// pre-creating the monitor spools, so the NumChannels gate is exercised on its
// own.
func TestMonitorPCMStereoInitCodecNumChannels(t *testing.T) {
	if media.CodecAudioOpus.NumChannels == 1 {
		t.Skip("test expects a multichannel codec constant")
	}
	recording := bytes.NewBuffer([]byte{})

	mon := &MonitorPCMStereo{SpoolDir: t.TempDir()}
	err := mon.Init(recording, media.CodecAudioOpus, media.CodecAudioOpus, bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	require.Error(t, err, "multichannel codecs cannot be interleaved as mono")
	assert.ErrorContains(t, err, "mono")
}

// TestMonitorPCMStereoCloseCleansUpOnDegrade pins that Close releases both
// spool files even when finalization fails. Under fail-open the motivating
// scenario (disk full) makes Flush error, and Close must not short-circuit
// past cleanup - otherwise every degraded call leaks two raw PCM files and
// their fds.
func TestMonitorPCMStereoCloseCleansUpOnDegrade(t *testing.T) {
	codec := media.CodecAudioAlaw
	alawFrame := make([]byte, 160)
	_, err := EncodeAlawTo(alawFrame, bytes.Repeat([]byte("0123456789"), codec.Samples16()/10))
	require.NoError(t, err)
	frames := (RecordingFlushSize / 320) + 5

	// Reader spool is a read-only file (every flush fails); writer spool is a
	// real file created under SpoolDir that the OLD code would leak.
	badSpool, err := os.OpenFile(filepath.Join(t.TempDir(), "ro.raw"), os.O_RDONLY|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer badSpool.Close()

	mon := &MonitorPCMStereo{SpoolDir: t.TempDir(), PCMFileRead: badSpool}
	mon.MonitorPCMReader.FailOpen = true
	require.NoError(t, mon.Init(bytes.NewBuffer([]byte{}), codec, codec, bytes.NewBuffer(bytes.Repeat(alawFrame, frames)), bytes.NewBuffer(nil)))

	for range frames {
		_, err := mon.Read(make([]byte, 160))
		require.NoError(t, err, "fail-open keeps media flowing")
	}

	writerSpool := mon.PCMFileWrite.Name()
	require.NoError(t, mon.MonitorPCMWriter.Flush()) // writer spool exists on disk

	err = mon.Close()
	require.Error(t, err, "Close still reports the degraded finalize")
	_, statErr := os.Stat(writerSpool)
	assert.True(t, os.IsNotExist(statErr), "writer spool must be removed despite the flush error (no /tmp leak)")
}

func TestMonitorPCMStereo(t *testing.T) {
	audioAlawBuf := make([]byte, 4*160)
	_, err := EncodeAlawTo(audioAlawBuf, bytes.Repeat([]byte("0123456789"), media.CodecAudioAlaw.Samples16()*4/10))
	require.NoError(t, err)
	audioPCMBuf := make([]byte, 4*320)
	DecodeAlawTo(audioPCMBuf, audioAlawBuf)

	t.Run("SmallData", func(t *testing.T) {
		mon := &MonitorPCMStereo{}
		recording := bytes.NewBuffer([]byte{})
		err = mon.Init(recording, media.CodecAudioAlaw, media.CodecAudioAlaw, bytes.NewBuffer(audioAlawBuf), bytes.NewBuffer([]byte{}))
		require.NoError(t, err)

		errWrite := make(chan error)
		go func() {
			// Do not share outer err with the test goroutine, it is a data race
			_, werr := media.WriteAll(mon, audioAlawBuf, 160)
			errWrite <- werr
		}()

		_, err = media.ReadAll(mon, 160)
		require.NoError(t, err)

		err = <-errWrite
		require.NoError(t, err)

		err = mon.Close()
		require.NoError(t, err)

		// Do files get removed
		_, err = os.Stat(mon.PCMFileRead.Name())
		assert.True(t, os.IsNotExist(err))
		_, err = os.Stat(mon.PCMFileWrite.Name())
		assert.True(t, os.IsNotExist(err))

		frameSize := media.CodecAudioAlaw.Samples16()
		assert.Equal(t, 8*frameSize, recording.Len())
		// Check does data alternate
		stereo := recording.Bytes()
		assert.Equal(t, audioPCMBuf[:2], stereo[:2])
		assert.Equal(t, audioPCMBuf[:2], stereo[2:4])
	})

	t.Run("BigData", func(t *testing.T) {
		audioAlawBufBig := bytes.Repeat(audioAlawBuf, 20)

		mon := &MonitorPCMStereo{}
		recording := bytes.NewBuffer([]byte{})
		err = mon.Init(recording, media.CodecAudioAlaw, media.CodecAudioAlaw, bytes.NewBuffer(audioAlawBufBig), bytes.NewBuffer([]byte{}))
		require.NoError(t, err)

		errWrite := make(chan error)
		go func() {
			// Do not share outer err with the test goroutine, it is a data race
			_, werr := media.WriteAll(mon, audioAlawBufBig, 160)
			errWrite <- werr
		}()

		_, err = media.ReadAll(mon, 160)
		require.NoError(t, err)

		err = <-errWrite
		require.NoError(t, err)

		err = mon.Close()
		require.NoError(t, err)

		frameSize := media.CodecAudioAlaw.Samples16()
		// 80 frames * 2 channels
		assert.Equal(t, 80*2*frameSize, recording.Len())
	})

}
