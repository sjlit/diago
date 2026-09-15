// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"crypto/tls"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransportSecureRTPValidation(t *testing.T) {
	t.Run("InvalidModeRejected", func(t *testing.T) {
		dm := &DialogMedia{}
		err := dm.initMediaSessionFromConf(MediaConfig{SecureRTP: 99})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid SRTP mode")
	})

	t.Run("ValidModesAccepted", func(t *testing.T) {
		for _, mode := range []int{SecureRTPNone, SecureRTPSDES, SecureRTPDTLS} {
			dm := &DialogMedia{}
			err := dm.initMediaSessionFromConf(MediaConfig{
				SecureRTP: mode,
				Codecs:    []media.Codec{media.CodecAudioUlaw},
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
	dg := NewDiago(ua)

	conf := dg.mediaConfigForTransport(&Transport{Transport: "udp", MediaDTLSConf: dtlsConf})
	assert.Equal(t, SecureRTPDTLS, conf.SecureRTP, "DTLSConf must auto-enable DTLS-SRTP")

	plain := dg.mediaConfigForTransport(&Transport{Transport: "udp"})
	assert.Equal(t, SecureRTPNone, plain.SecureRTP)

	explicitSDES := dg.mediaConfigForTransport(&Transport{
		Transport:     "udp",
		MediaSRTP:     SecureRTPSDES,
		MediaDTLSConf: dtlsConf,
	})
	assert.Equal(t, SecureRTPSDES, explicitSDES.SecureRTP, "explicit mode must win over auto-DTLS")
}

// TestIntegrationDTLSAutoNegotiation drives the auto-enabled DTLS mode through
// a real offer/answer exchange (no handshake): both sides must advertise
// UDP/TLS/RTP/SAVP with fingerprints.
func TestIntegrationDTLSAutoNegotiation(t *testing.T) {
	offerer := &DialogMedia{}
	require.NoError(t, offerer.initMediaSessionFromConf(MediaConfig{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		SecureRTP: SecureRTPDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
	}))
	defer offerer.Close()

	answerer := &DialogMedia{}
	require.NoError(t, answerer.initMediaSessionFromConf(MediaConfig{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		SecureRTP: SecureRTPDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
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
