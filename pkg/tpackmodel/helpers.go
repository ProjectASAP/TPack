package tpackmodel

import (
	"encoding/hex"
	"math/rand"
)

// IndicesToMetadata converts metadata indices back to a string map using the
// per-column vocabularies stored on the model state.
func IndicesToMetadata(indices []int, columns []string, vocabs map[string][]string) map[string]string {
	if len(indices) == 0 || len(columns) == 0 {
		return nil
	}
	meta := make(map[string]string, len(columns))
	for i, col := range columns {
		if i < len(indices) {
			if vocab, ok := vocabs[col]; ok && indices[i] >= 0 && indices[i] < len(vocab) {
				meta[col] = vocab[indices[i]]
			}
		}
	}
	return meta
}

// RandomHex returns a length-N lowercase hex string drawn from rng.
// Used for trace and span IDs at generation time.
func RandomHex(rng *rand.Rand, length int) string {
	b := make([]byte, (length+1)/2)
	_, _ = rng.Read(b)
	return hex.EncodeToString(b)[:length]
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
