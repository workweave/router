package proxy

// Response-header names the router sets on proxied responses to surface its
// routing decision to clients. These are the single source of truth for the
// header contract; prod write sites and tests reference these constants rather
// than re-spelling the literals.
const (
	// HeaderRouterDecision carries the routing decision reason.
	HeaderRouterDecision = "x-router-decision"
	// HeaderRouterProvider carries the upstream provider that served the turn.
	HeaderRouterProvider = "x-router-provider"
	// HeaderRouterModel carries the model that served the turn.
	HeaderRouterModel = "x-router-model"
	// HeaderRouterCache reports semantic-cache status; value is RouterCacheHit
	// on a cache hit and the header is omitted otherwise.
	HeaderRouterCache = "x-router-cache"
	// HeaderRouterFallbackFrom names the primary provider that was abandoned
	// when runtime provider failover served the turn from a fallback binding.
	HeaderRouterFallbackFrom = "x-router-fallback-from"
	// HeaderRouterFallbackAttempt carries the zero-based fallback attempt index
	// that ultimately served the turn.
	HeaderRouterFallbackAttempt = "x-router-fallback-attempt"
)

// RouterCacheHit is the HeaderRouterCache value set when a response is served
// from the semantic cache.
const RouterCacheHit = "hit"
