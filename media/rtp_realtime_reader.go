package media

import (
	"fmt"
	"io"
	"log/slog"
	"time"
)

type RTPRealTimeReader struct {
	r                 io.Reader
	rtpReader         *RTPPacketReader
	firstReadTime     time.Time
	firstRTPTimestamp uint32
	codec             Codec
}

func NewRTPRealTimeReader(reader io.Reader, rtpReader *RTPPacketReader, codec Codec) (*RTPRealTimeReader, error) {
	r := &RTPRealTimeReader{}
	if err := r.Init(reader, rtpReader, codec); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RTPRealTimeReader) Init(reader io.Reader, rtpReader *RTPPacketReader, codec Codec) error {
	if codec.SampleDur == 0 {
		return fmt.Errorf("codec sample dur not defined: %+v", codec)
	}
	if codec.SampleTimestamp() == 0 {
		// Read divides the RTP timestamp delta by this value; a degenerate
		// codec (SampleRate==0 with SampleDur>0) would panic with a division
		// by zero on the second packet.
		return fmt.Errorf("codec sample rate not defined: %+v", codec)
	}

	r.r = reader
	r.rtpReader = rtpReader
	r.codec = codec
	return nil
}

func (r *RTPRealTimeReader) Reset() {
	r.firstReadTime = time.Time{}
}

// logger returns the reader's logger, inherited from the packet reader handle
// that owns it.
func (r *RTPRealTimeReader) logger() *slog.Logger {
	if r.rtpReader != nil && r.rtpReader.log != nil {
		return r.rtpReader.log
	}
	return DefaultLogger()
}

func (r *RTPRealTimeReader) Read(b []byte) (int, error) {
	if r.firstReadTime.IsZero() {
		// If no read yet made
		n, err := r.r.Read(b)
		if err != nil {
			return n, err
		}

		r.firstReadTime = time.Now()
		r.firstRTPTimestamp = r.rtpReader.PacketHeader.Timestamp
		return n, err
	}

	for {
		n, err := r.r.Read(b)
		if err != nil {
			return n, err
		}

		rtpHeader := r.rtpReader.PacketHeader
		tsDiff := rtpHeader.Timestamp - r.firstRTPTimestamp
		dur := time.Duration(tsDiff/r.codec.SampleTimestamp()) * r.codec.SampleDur

		if time.Since(r.firstReadTime) > dur+2*r.codec.SampleDur {
			// Skip it
			r.logger().Debug("Skipping non realtime packet", "ssrc", rtpHeader.SSRC, "seq", rtpHeader.SequenceNumber, "ts", rtpHeader.Timestamp, "ts_diff", tsDiff)
			continue
		}
		return n, nil
	}
}
