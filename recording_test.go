package diago

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewDialogRecordingWavCodecPolicy pins the relaxed codec gate: each
// direction decodes independently, so PCMA (reader) + PCMU (writer) - same
// 8k/20ms timing, different names - must build. Only a timing mismatch
// (sample rate / frame duration) may reject, because the two spools share one
// interleaved WAV timeline.
func TestNewDialogRecordingWavCodecPolicy(t *testing.T) {
	t.Run("DifferentCodecSameTimingAccepted", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
		require.NoError(t, err)
		defer f.Close()

		rec, err := newDialogRecordingWav(f, bytes.NewBuffer(nil), MediaProps{Codec: media.CodecAudioAlaw},
			io.Discard, MediaProps{Codec: media.CodecAudioUlaw})
		require.NoError(t, err)
		require.NoError(t, rec.Close())
	})

	t.Run("TimingMismatchRejected", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
		require.NoError(t, err)
		defer f.Close()

		_, err = newDialogRecordingWav(f, bytes.NewBuffer(nil), MediaProps{Codec: media.CodecAudioAlaw},
			io.Discard, MediaProps{Codec: media.CodecAudioOpus})
		require.Error(t, err)
	})
}

func TestIntegrationRecordingStereoWav(t *testing.T) {
	fakePCMFrame := bytes.Repeat([]byte("0123456789"), 32)
	alawFrame := make([]byte, 160)
	_, err := audio.EncodeAlawTo(alawFrame, fakePCMFrame)
	require.NoError(t, err)
	encodedAudio := bytes.Repeat(alawFrame, 4)

	dialog := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession: &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
			// audioReader:  bytes.NewBuffer(make([]byte, 9999)),
			audioReader:     bytes.NewBuffer(encodedAudio),
			audioWriter:     bytes.NewBuffer([]byte{}),
			RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioUlaw),
		},
	}

	recordFile, err := os.OpenFile("/tmp/diago_test_record_stereo.wav", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0755)
	require.NoError(t, err)
	defer recordFile.Close()

	rec, err := dialog.AudioStereoRecordingCreate(recordFile)
	require.NoError(t, err)

	media.ReadAll(rec.AudioReader(), 160)
	media.WriteAll(rec.AudioWriter(), encodedAudio, 160)
	err = rec.Close()

	recordFile.Seek(0, 0)
	wav := audio.NewWavReader(recordFile)
	wav.ReadHeaders()
	// 2 channels, 4 frames Read, 4 frames Write
	assert.Equal(t, 2*4*320, wav.DataSize)
}
