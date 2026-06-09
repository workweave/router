package toolcheck

import (
	"crypto/sha256"
	"encoding/hex"

	lru "github.com/hashicorp/golang-lru/v2"
)

// compiledCacheSize is sized for the distinct tool sets in flight at once:
// agent sessions resend a byte-identical tools block every turn, so the
// working set is roughly one entry per distinct client configuration.
const compiledCacheSize = 64

// compiledCache amortizes schema compilation across turns. Keyed by the
// sha256 of the raw tools JSON; *Validator values are immutable after
// Compile so sharing across requests is safe.
var compiledCache, _ = lru.New[string, *Validator](compiledCacheSize)

// CompileCached fronts Compile with the package-level LRU. Note: a nil
// Validator result (no usable tools) is also cached.
func CompileCached(toolsRaw []byte) *Validator {
	sum := sha256.Sum256(toolsRaw)
	key := hex.EncodeToString(sum[:])
	if v, ok := compiledCache.Get(key); ok {
		return v
	}
	v := Compile(toolsRaw)
	compiledCache.Add(key, v)
	return v
}
