// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"crypto/tls"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransportSecureRTPValidation(t *testing.T) {
	t.Run("invalid mode rejected", func(t *testing.T) {
		dm := &Media{}
		err := dm.initMediaSessionFromConf(MediaConfig{SRTP: SRTPConfig{Mode: 99}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid srtp mode")
	})

	t.Run("valid modes accepted", func(t *testing.T) {
		for _, mode := range []SecureRTP{SecureRTPNone, SecureRTPSDES, SecureRTPDTLS} {
			dm := &Media{}
			err := dm.initMediaSessionFromConf(MediaConfig{
				SRTP:   SRTPConfig{Mode: mode},
				Codecs: []media.Codec{media.CodecAudioUlaw},
			})
			require.NoError(t, err, "mode %d", mode)
			require.NoError(t, dm.Close())
		}
	})
}

func TestMediaConfigDTLSAutoEnable(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dtlsConf := media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}}
	dg, err := newAgent(withUserAgent(ua),
		withMediaProfile("dtls", MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			SRTP:   SRTPConfig{DTLS: dtlsConf},
		}),
		withMediaProfile("sdes+dtls", MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			SRTP:   SRTPConfig{Mode: SecureRTPSDES, DTLS: dtlsConf},
		}),
		withDefaultTransport(),
	)
	require.NoError(t, err)

	conf := dg.mediaConfigForTransport(&Transport{Network: "udp", Profile: "dtls"})
	assert.Equal(t, SecureRTPDTLS, conf.SRTP.Mode, "DTLS config must auto-enable DTLS-SRTP")

	plain := dg.mediaConfigForTransport(&Transport{Network: "udp"})
	assert.Equal(t, SecureRTPNone, plain.SRTP.Mode)

	explicitSDES := dg.mediaConfigForTransport(&Transport{Network: "udp", Profile: "sdes+dtls"})
	assert.Equal(t, SecureRTPSDES, explicitSDES.SRTP.Mode, "explicit mode must win over auto-DTLS")
}

// TestWithMediaDTLSOptionEnablesDTLS locks the per-call path end to end:
// WithMediaDTLS must not only store the DTLS config, it must also flip the
// session to DTLS-SRTP (the old behavior silently kept plain RTP).
func TestMediaOptionsDTLSEnablesDTLS(t *testing.T) {
	p := &signalParams{}
	require.NoError(t, (MediaOptions{DTLS: &media.DTLSConfig{
		Certificates: []tls.Certificate{testdata.ServerCertificate()},
	}}).apply(p))

	conf := signalMediaConfig(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, p)
	dm := &Media{}
	require.NoError(t, dm.initMediaSessionFromConf(conf))
	defer dm.Close()

	require.NotNil(t, dm.mediaSession)
	assert.Equal(t, int(SecureRTPDTLS), dm.mediaSession.SecureRTP)
}

// TestMediaConfigDTLSAutoEnableOnSession locks that a DTLS configuration with
// Mode left at SecureRTPNone enables DTLS at session init, not only in the
// transport merge. Regression: WithMediaDTLS on Answer stored the DTLS config
// but media stayed plain RTP.
func TestMediaConfigDTLSAutoEnableOnSession(t *testing.T) {
	dm := &Media{}
	require.NoError(t, dm.initMediaSessionFromConf(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		SRTP: SRTPConfig{
			DTLS: media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
		},
	}))
	defer dm.Close()

	require.NotNil(t, dm.mediaSession)
	assert.Equal(t, int(SecureRTPDTLS), dm.mediaSession.SecureRTP)
}

// TestIntegrationDTLSAutoNegotiation drives the auto-enabled DTLS mode through
// a real offer/answer exchange (no handshake): both sides must advertise
// UDP/TLS/RTP/SAVP with fingerprints.
func TestIntegrationDTLSAutoNegotiation(t *testing.T) {
	offerer := &Media{}
	require.NoError(t, offerer.initMediaSessionFromConf(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		SRTP: SRTPConfig{
			Mode: SecureRTPDTLS,
			DTLS: media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
		},
	}))
	defer offerer.Close()

	answerer := &Media{}
	require.NoError(t, answerer.initMediaSessionFromConf(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		SRTP: SRTPConfig{
			Mode: SecureRTPDTLS,
			DTLS: media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
		},
	}))
	defer answerer.Close()

	offerSDP := offerer.MediaSession().LocalSDP()
	require.Contains(t, string(offerSDP), "UDP/TLS/RTP/SAVP")
	require.Contains(t, string(offerSDP), "a=fingerprint:SHA-256")

	require.NoError(t, answerer.MediaSession().RemoteSDP(offerSDP))
	answerSDP := answerer.MediaSession().LocalSDP()
	assert.Contains(t, string(answerSDP), "UDP/TLS/RTP/SAVP")
	assert.Contains(t, string(answerSDP), "a=fingerprint:SHA-256")

	require.NoError(t, offerer.MediaSession().RemoteSDP(answerSDP))
}
