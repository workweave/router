package proxy

// MaxRequestBodyBytes caps inbound request bodies across the Anthropic,
// OpenAI, and Gemini API surfaces. One shared constant so the cap can't
// drift between handler packages that each read it independently.
const MaxRequestBodyBytes = 10 * 1024 * 1024
