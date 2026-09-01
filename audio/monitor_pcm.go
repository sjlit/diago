package audio

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	"github.com/sjlit/diago/media"
	"github.com/google/uuid"
)

var (
	RecordingFlushSize = 4096
)

type pcmBufioWriter struct {
	writer   *bufio.Writer // Lets use Buffered flushing
	mu       sync.Mutex
	stopped  bool
	lastTime time.Time
	codec    media.Codec
	silence  []byte

	// FailOpen makes recording best-effort: the first write error to the
	// PCM sink (disk full, IO degradation) is recorded in brokenErr and
	// swallowed from Read/Write, so the media path keeps running. When false
	// the error propagates to the caller as before. Flush/Close (the
	// finalize path) always report.
	FailOpen bool

	brokenErr error
}

func (m *pcmBufioWriter) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Check do we need to inject silence as a tail
	// Case Reader had no stream running, but time passed
	if err := m.writeSilenceUnsafe(time.Now()); err != nil {
		return err
	}

	if err := m.writer.Flush(); err != nil {
		return err
	}

	return nil
}

func (m *pcmBufioWriter) writeSilenceUnsafe(now time.Time) error {
	diff := uint32(now.Sub(m.lastTime).Seconds() * float64(m.codec.SampleRate))
	srt := m.codec.SampleTimestamp()
	for i := 2 * srt; i < diff; i += srt {
		if _, err := m.writer.Write(m.silence); err != nil {
			return err
		}
	}
	m.lastTime = now
	return nil
}

func (m *pcmBufioWriter) writePCM(now time.Time, lpcm []byte) error {
	// We do not want to write on stopped monitoring
	// We need this, because user can stop monitoring, but still keep underhood stream active
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.brokenErr != nil {
		return nil // paused, or recording already degraded (fail-open)
	}

	// Check do we need to inject first silence
	if err := m.writeSilenceUnsafe(now); err != nil {
		return m.failOpenUnsafe(err)
	}

	if _, err := m.writer.Write(lpcm); err != nil {
		return m.failOpenUnsafe(err)
	}
	return nil
}

// failOpenUnsafe applies the write-error policy under m.mu. With FailOpen set
// the first failure is recorded and swallowed - recording stops taking writes
// (writePCM short-circuits on brokenErr) while the media chain keeps flowing.
func (m *pcmBufioWriter) failOpenUnsafe(err error) error {
	if !m.FailOpen {
		return err
	}
	if m.brokenErr == nil {
		m.brokenErr = err
	}
	return nil
}

// Err returns the first recording write error swallowed by FailOpen, wrapped
// with degradation context ("recording degraded"). The underlying error stays
// errors.Is/As-traceable, so callers can still match on the errno.
func (m *pcmBufioWriter) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.brokenErr == nil {
		return nil
	}
	return fmt.Errorf("recording degraded: %w", m.brokenErr)
}

func (m *pcmBufioWriter) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
}

func (m *pcmBufioWriter) Start() {
	m.mu.Lock()
	m.stopped = false
	m.mu.Unlock()
}

// Monitoring starts with first packet arrived, but you can shift with start time. Ex stream are not continious
func (m *pcmBufioWriter) StartTime(t time.Time) {
	m.mu.Lock()
	m.lastTime = t
	m.mu.Unlock()
}

type MonitorPCMReader struct {
	pcmBufioWriter
	audioReader  io.Reader
	decoder      PCMDecoderBuffer
	FlushOnError bool
}

func (m *MonitorPCMReader) Init(w io.Writer, codec media.Codec, audioReader io.Reader) error {
	bw := bufio.NewWriterSize(w, RecordingFlushSize)
	m.writer = bw
	m.codec = codec
	m.audioReader = audioReader

	decoder := PCMDecoderBuffer{}
	if err := decoder.Init(codec); err != nil {
		return err
	}
	m.decoder = decoder

	samples16 := codec.Samples16()
	silence := bytes.Repeat([]byte{0}, samples16) // This alloc could be avoided
	m.silence = silence
	m.lastTime = time.Now()
	return nil
}

func (m *MonitorPCMReader) Read(b []byte) (int, error) {
	n, err := m.audioReader.Read(b)
	if err != nil {
		if m.FlushOnError {
			return n, errors.Join(err, m.Flush())
		}
		return n, err
	}
	now := time.Now()

	// Decode stream to PCM unless stream is already decoded?
	if _, err := m.decoder.Write(b[:n]); err != nil {
		return 0, err
	}
	lpcm := m.decoder.ReadFull()

	// Write to outer stream. Expecting some buffer with flushing will happen
	err = m.writePCM(now, lpcm)
	return n, err
}

type MonitorPCMWriter struct {
	pcmBufioWriter
	audioWriter  io.Writer
	decoder      PCMDecoderBuffer
	FlushOnError bool
}

func (m *MonitorPCMWriter) Init(w io.Writer, codec media.Codec, audioWriter io.Writer) error {
	bw := bufio.NewWriterSize(w, RecordingFlushSize)
	m.writer = bw
	m.codec = codec
	m.audioWriter = audioWriter

	decoder := PCMDecoderBuffer{}
	if err := decoder.Init(codec); err != nil {
		return err
	}
	m.decoder = decoder

	samples16 := codec.Samples16()
	silence := bytes.Repeat([]byte{0}, samples16) // This alloc could be avoided
	m.silence = silence
	m.lastTime = time.Now()
	return nil
}

func (m *MonitorPCMWriter) Write(b []byte) (int, error) {
	n, err := m.audioWriter.Write(b)
	if err != nil {
		if m.FlushOnError {
			return n, errors.Join(err, m.Flush())
		}
		return n, err
	}

	now := time.Now()
	// Decode stream to PCM unless stream is already decoded?
	if _, err := m.decoder.Write(b[:n]); err != nil {
		return 0, err
	}
	lpcm := m.decoder.ReadFull()

	// Write to outer stream. Expecting some buffer with flushing will happen
	err = m.writePCM(now, lpcm)
	return n, err
}

type MonitorPCMStereo struct {
	MonitorPCMReader
	MonitorPCMWriter

	// PCMFileRead/PCMFileWrite are the per-direction raw spool files. They
	// are normally created by Init under SpoolDir, but may be pre-set by the
	// caller before Init (Init then keeps them). Only files Init created
	// itself are closed and removed by Close; caller-provided files remain
	// owned by the caller.
	PCMFileRead  *os.File
	PCMFileWrite *os.File

	// SpoolDir is the directory for the two per-direction raw spool files.
	// Empty falls back to os.TempDir(). The spool holds undecoded call
	// audio, so files are created 0600 regardless of the directory.
	SpoolDir string

	// ownedRead/ownedWrite mark the spool files Init created itself; only
	// those are closed and removed by Close / Init rollback.
	ownedRead  bool
	ownedWrite bool

	recording io.Writer
}

// Err returns the first recording write error swallowed by FailOpen across
// both directions, or nil. It is a degradation signal for the media path;
// Close reports finalization errors independently.
func (m *MonitorPCMStereo) Err() error {
	return errors.Join(m.MonitorPCMReader.Err(), m.MonitorPCMWriter.Err())
}

// Pause stops PCM collection on both directions. Media keeps flowing through
// the monitor untouched; the wall-clock gap is padded with silence by the
// next write or Flush (see writeSilenceUnsafe), so both channels stay aligned
// with the call timeline - a paused interval appears as silence in the final
// WAV. Resume re-enables collection.
func (m *MonitorPCMStereo) Pause() {
	m.MonitorPCMReader.Stop()
	m.MonitorPCMWriter.Stop()
}

// Resume continues PCM collection after Pause.
func (m *MonitorPCMStereo) Resume() {
	m.MonitorPCMReader.Start()
	m.MonitorPCMWriter.Start()
}

// Each direction decodes with its own codec (readerCodec for audioReader,
// writerCodec for audioWriter). The two spools share one interleaved
// timeline, so both codecs must agree on sample rate and frame duration.
// Only mono codecs are accepted: the interleave pass treats each direction as
// a mono 16-bit sample stream (the stereo pairing happens there), so a
// multichannel codec would produce a wrongly laid out WAV.
func (m *MonitorPCMStereo) Init(record io.Writer, readerCodec, writerCodec media.Codec, audioReader io.Reader, audioWriter io.Writer) error {
	if readerCodec.SampleRate != writerCodec.SampleRate || readerCodec.SampleDur != writerCodec.SampleDur {
		return fmt.Errorf("stereo timeline mismatch: reader codec %s vs writer codec %s (rate/duration must match)", readerCodec.Name, writerCodec.Name)
	}
	if readerCodec.NumChannels != 1 || writerCodec.NumChannels != 1 {
		return fmt.Errorf("stereo monitor requires mono codecs: reader codec %s has %d channels, writer codec %s has %d channels", readerCodec.Name, readerCodec.NumChannels, writerCodec.Name, writerCodec.NumChannels)
	}
	m.recording = record

	spoolDir := m.SpoolDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	uuid := uuid.New().String()
	var err error
	err = func() error {
		if m.PCMFileRead == nil {
			filepath := path.Join(spoolDir, uuid+"_monitor_reader.raw")
			m.PCMFileRead, err = os.OpenFile(filepath, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			m.ownedRead = true
		}

		if m.PCMFileWrite == nil {
			filepath := path.Join(spoolDir, uuid+"_monitor_writer.raw")
			m.PCMFileWrite, err = os.OpenFile(filepath, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			m.ownedWrite = true
		}

		if err := m.MonitorPCMReader.Init(m.PCMFileRead, readerCodec, audioReader); err != nil {
			return err
		}

		if err := m.MonitorPCMWriter.Init(m.PCMFileWrite, writerCodec, audioWriter); err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		return errors.Join(err, m.removeTmpFiles())
	}

	return nil
}

// removeTmpFiles closes and removes the spool files Init created itself
// (ownedRead/ownedWrite). Caller-provided files are left open and in place.
func (m *MonitorPCMStereo) removeTmpFiles() (err error) {
	if m.ownedRead && m.PCMFileRead != nil {
		e1 := m.PCMFileRead.Close()
		e2 := os.Remove(m.PCMFileRead.Name())
		err = errors.Join(err, e1, e2)
		m.ownedRead = false
	}

	if m.ownedWrite && m.PCMFileWrite != nil {
		e1 := m.PCMFileWrite.Close()
		e2 := os.Remove(m.PCMFileWrite.Name())
		err = errors.Join(err, e1, e2)
		m.ownedWrite = false
	}
	return err
}

func (m *MonitorPCMStereo) Close() error {
	// Stop any current PCM writing
	m.MonitorPCMReader.Stop()
	m.MonitorPCMWriter.Stop()

	// Every stage is attempted and its error joined, never short-circuited:
	// a failed flush (the common fail-open / disk-full case) must not skip
	// the interleave pass or the spool cleanup, or each degraded call leaks
	// the two raw PCM files and their fds. removeTmpFiles is the last,
	// unconditional step so fds and spools are always released.
	err := m.Flush()
	err = errors.Join(err, m.interleave())
	return errors.Join(err, m.removeTmpFiles())
}

func (m *MonitorPCMStereo) Flush() error {
	if err := m.MonitorPCMReader.Flush(); err != nil {
		return err
	}
	if err := m.MonitorPCMWriter.Flush(); err != nil {
		return err
	}
	return nil
}

func (m *MonitorPCMStereo) interleave() error {
	fr := m.PCMFileRead
	fw := m.PCMFileWrite
	recording := m.recording
	if _, err := fr.Seek(0, 0); err != nil {
		return err
	}
	if _, err := fw.Seek(0, 0); err != nil {
		return err
	}

	// Read frames from both files and interleave
	readBuf1 := make([]byte, RecordingFlushSize/2)
	readBuf2 := make([]byte, RecordingFlushSize/2)
	stereoBuf := make([]byte, (RecordingFlushSize/2)*2)
	size := 2 // 16 bit
	for {
		n1, err1 := io.ReadFull(fr, readBuf1)
		n2, err2 := io.ReadFull(fw, readBuf2)

		n := max(n1, n2)

		if (err1 != nil || err2 != nil) && n == 0 {
			if !errors.Is(err1, io.EOF) {
				return err1
			}

			if !errors.Is(err2, io.EOF) {
				return err2
			}
			break
		}
		// Shorter file ended, then pad its missing tail with silence
		clear(readBuf1[n1:n])
		clear(readBuf2[n2:n])

		// Keep 16-bit sample alignment on a final partial chunk
		n &^= 1

		// interleave
		copyN := 0
		for i, j := 0, 0; i < n; i += size {
			copyN += copy(stereoBuf[j:j+size], readBuf1[i:i+size])
			copyN += copy(stereoBuf[j+size:j+2*size], readBuf2[i:i+size])
			j += 2 * size // 2 channels * size
		}

		if _, err := recording.Write(stereoBuf[:copyN]); err != nil {
			return err
		}

	}

	return nil
}
