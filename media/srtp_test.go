// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"testing"

	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// srtpProfileString renders SDES a=crypto algorithm names; srtpProfileParse
// must accept them back (round trip).
func TestSRTPProfileStringRoundTrip(t *testing.T) {
	profiles := []srtp.ProtectionProfile{
		srtp.ProtectionProfileAes128CmHmacSha1_80,
		srtp.ProtectionProfileAes256CmHmacSha1_80,
		srtp.ProtectionProfileAeadAes128Gcm,
		srtp.ProtectionProfileAeadAes256Gcm,
		srtp.ProtectionProfileNullHmacSha1_80,
	}

	for _, profile := range profiles {
		name := srtpProfileString(profile)
		require.NotEmpty(t, name)
		require.NotContains(t, name, "SRTP_", "SDES name must not carry the pion prefix: %q", name)
		require.Equal(t, profile, srtpProfileParse(name), "parse must round-trip %q", name)
	}
}
