package diago

import (
	"errors"
	"io"
	"os"

	"github.com/sjlit/diago/audio"
)

type AudioStereoRecordingWav struct {
	wawWriter *audio.WavWriter
	// mon is a pointer, the monitor embeds mutexes and must never be copied by value
	mon *audio.MonitorPCMStereo
}

func (r *AudioStereoRecordingWav) AudioReader() *audio.MonitorPCMStereo {
	return r.mon
}

func (r *AudioStereoRecordingWav) AudioWriter() *audio.MonitorPCMStereo {
	return r.mon
}

func (r *AudioStereoRecordingWav) Close() error {
	return errors.Join(
		r.mon.Close(),
		r.wawWriter.Close(),
	)
}

func newDialogRecordingWav(wawFile *os.File, ar io.Reader, arProps MediaProps, aw io.Writer, awProps MediaProps) (*AudioStereoRecordingWav, error) {
	// Each direction decodes with its own codec; the two spools only need to
	// share the interleaved timeline timing (rate/duration), enforced by
	// MonitorPCMStereo.Init.
	wavWriter := audio.NewWavWriter(wawFile)
	if awProps.Codec.SampleRate != 0 {
		wavWriter.SampleRate = int(awProps.Codec.SampleRate)
	}

	mon := &audio.MonitorPCMStereo{}
	if err := mon.Init(wavWriter, arProps.Codec, awProps.Codec, ar, aw); err != nil {
		wavWriter.Close()
		return nil, err
	}

	r := &AudioStereoRecordingWav{
		wawWriter: wavWriter,
		mon:       mon,
	}
	return r, nil

}
