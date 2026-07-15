package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateID_FormatAndCharset(t *testing.T) {
	id := GenerateID("rk")
	require.True(t, strings.HasPrefix(id, "rk_"))
	suffix := strings.TrimPrefix(id, "rk_")
	assert.Len(t, suffix, idLength)
	for _, ch := range suffix {
		assert.Contains(t, alphaNumericCharacters, string(ch))
	}
}

func TestGenerateID_UniformDistribution(t *testing.T) {
	// With rejection sampling every alphabet character is equally likely.
	// A plain byte%62 skews the first 8 characters ('0'..'7') to 5/1024
	// vs 4/1024 for the rest — a 25% relative excess this sample size
	// detects reliably while staying far outside random noise.
	const samples = 4000
	counts := make(map[rune]int, len(alphaNumericCharacters))
	for i := 0; i < samples; i++ {
		for _, ch := range strings.TrimPrefix(GenerateID("t"), "t_") {
			counts[ch]++
		}
	}
	total := samples * idLength
	expected := float64(total) / float64(len(alphaNumericCharacters))
	biased := 0
	for _, ch := range alphaNumericCharacters[:8] {
		if float64(counts[ch]) > expected*1.15 {
			biased++
		}
	}
	assert.Zero(t, biased, "first 8 alphabet characters must not be over-represented")
}
