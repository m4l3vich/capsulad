package assistant

import (
	"encoding/binary"
	"math"
)

const levelFloorDB = -60.0

// pcmLevel converts 16-bit mono PCM to a 0..1 level for the ring. The mapping
// is logarithmic like the microphone cue: -60 dBFS is dark and 0 dBFS is full.
func pcmLevel(pcm []byte) float64 {
	if len(pcm) < 2 {
		return 0
	}
	var sum float64
	samples := 0
	for offset := 0; offset+1 < len(pcm); offset += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[offset:offset+2]))) / 32768
		sum += value * value
		samples++
	}
	if samples == 0 || sum <= 0 {
		return 0
	}
	db := 20 * math.Log10(math.Sqrt(sum/float64(samples)))
	if db <= levelFloorDB {
		return 0
	}
	if db >= 0 {
		return 1
	}
	return (db - levelFloorDB) / -levelFloorDB
}
