// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"testing"

	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// srtpProfileString renders SDES a=crypto algorithm names (RFC 4568/6188);
// srtpProfileParse must accept them back (round trip).
func TestSRTPProfileStringRoundTrip(t *testing.T) {
	profiles := []srtp.ProtectionProfile{
		srtp.ProtectionProfileAes128CmHmacSha1_80,
		srtp.ProtectionProfileAes128CmHmacSha1_32,
		srtp.ProtectionProfileAes256CmHmacSha1_80,
		srtp.ProtectionProfileAes256CmHmacSha1_32,
		srtp.ProtectionProfileAeadAes128Gcm,
		srtp.ProtectionProfileAeadAes256Gcm,
		srtp.ProtectionProfileNullHmacSha1_80,
		srtp.ProtectionProfileNullHmacSha1_32,
	}

	for _, profile := range profiles {
		name := srtpProfileString(profile)
		require.NotEmpty(t, name)
		require.NotContains(t, name, "SRTP_", "SDES name must not carry the pion prefix: %q", name)
		require.Equal(t, profile, srtpProfileParse(name), "parse must round-trip %q", name)
	}

	// RFC 4568/6188 spellings.
	require.Equal(t, "AES_CM_128_HMAC_SHA1_80", srtpProfileString(srtp.ProtectionProfileAes128CmHmacSha1_80))
	require.Equal(t, "AES_CM_128_HMAC_SHA1_32", srtpProfileString(srtp.ProtectionProfileAes128CmHmacSha1_32))
	require.Equal(t, "AES_256_CM_HMAC_SHA1_80", srtpProfileString(srtp.ProtectionProfileAes256CmHmacSha1_80))
	require.Equal(t, "AES_256_CM_HMAC_SHA1_32", srtpProfileString(srtp.ProtectionProfileAes256CmHmacSha1_32))

	// The pre-RFC spelling emitted by older diago versions still parses.
	require.Equal(t,
		srtp.ProtectionProfileAes256CmHmacSha1_80,
		srtpProfileParse("AES_CM_256_HMAC_SHA1_80"))
}
