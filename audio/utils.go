// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package audio

import "encoding/binary"

func PCMMix(dstBuf []byte, mixedBuf []byte, readBuf []byte) int {
	// Keep 16-bit sample alignment across all buffers, otherwise an odd-sized
	// or short read would panic on the last 2-byte sample
	n := min(len(dstBuf), len(mixedBuf), len(readBuf))
	n &^= 1
	for i := 0; i < n; i += 2 {
		current := int16(binary.LittleEndian.Uint16(mixedBuf[i:]))
		frame := int16(binary.LittleEndian.Uint16(readBuf[i:]))

		mixed32 := int32(current) + int32(frame)
		var mixed int16
		switch {
		case mixed32 > 32767: //int16 max
			mixed = 32767
		case mixed32 < -32768:
			mixed = -32768
		default:
			mixed = int16(mixed32)
		}

		binary.LittleEndian.PutUint16(dstBuf[i:], uint16(mixed))
	}
	return n
}

func PCMUnmix(dstBuf []byte, mixedBuf []byte, readBuf []byte) {
	// Keep 16-bit sample alignment across all buffers, otherwise an odd-sized
	// or short read would panic on the last 2-byte sample
	n := min(len(dstBuf), len(mixedBuf), len(readBuf))
	n &^= 1
	for i := 0; i < n; i += 2 {
		current := int16(binary.LittleEndian.Uint16(mixedBuf[i:]))
		frame := int16(binary.LittleEndian.Uint16(readBuf[i:]))

		mixed32 := int32(current) - int32(frame)
		var mixed int16
		switch {
		case mixed32 > 32767: //int16 max
			mixed = 32767
		case mixed32 < -32768:
			mixed = -32768
		default:
			mixed = int16(mixed32)
		}

		binary.LittleEndian.PutUint16(dstBuf[i:], uint16(mixed))
	}
}
