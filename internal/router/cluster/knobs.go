package cluster

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// ComputeKnobsHash calculates a canonical 64-bit hash over the effective knobs.
func ComputeKnobsHash(alpha []float64, speedWeight, outputCostRatio float64, expectedOutputTokens int, perModelVerbosity bool) uint64 {
	// 4 bytes for len(alpha)
	// len(alpha)*8 bytes for alpha values
	// 8 bytes for speedWeight
	// 8 bytes for outputCostRatio
	// 4 bytes for expectedOutputTokens
	// 1 byte for perModelVerbosity
	buf := make([]byte, 4+len(alpha)*8+8+8+4+1)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(alpha)))
	off := 4
	for _, a := range alpha {
		binary.LittleEndian.PutUint64(buf[off:off+8], math.Float64bits(a))
		off += 8
	}
	binary.LittleEndian.PutUint64(buf[off:off+8], math.Float64bits(speedWeight))
	off += 8
	binary.LittleEndian.PutUint64(buf[off:off+8], math.Float64bits(outputCostRatio))
	off += 8
	binary.LittleEndian.PutUint32(buf[off:off+4], uint32(expectedOutputTokens))
	off += 4
	if perModelVerbosity {
		buf[off] = 1
	} else {
		buf[off] = 0
	}

	sum := sha256.Sum256(buf)
	return binary.LittleEndian.Uint64(sum[:8])
}
