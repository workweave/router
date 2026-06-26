package translate

import "strings"

// thinkKind classifies a segment produced by thinkTagSplitter.
type thinkKind int

const (
	segThinking thinkKind = iota // content inside <think>…</think>
	segText                      // content outside the tags
)

// thinkSegment is one piece of content produced by the splitter.
type thinkSegment struct {
	kind thinkKind
	text string
}

// thinkState tracks the splitter's scan progress.
type thinkState int

const (
	thScanningOpen thinkState = iota // scanning for a leading <think>
	thInsideThink                    // inside <think>…</think>
	thPassthrough                    // definitive non-match; emit verbatim
)

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// thinkTagSplitter is a stateful scanner that reroutes a leading
// <think>…</think> block from an OpenAI-compat content stream into
// separate thinking/text segments. A <think> that does not open the
// content (after leading whitespace) is passed through verbatim, so
// mid-prose mentions of the tag never trip it.
//
// Designed for streaming: Feed is called per-delta and never buffers
// whole responses — at most len(thinkOpenTag) bytes while matching the
// open tag, and at most len(thinkCloseTag)-1 bytes of the in-flight
// close-tag prefix.
type thinkTagSplitter struct {
	state thinkState
	// accum holds bytes consumed while matching the leading open tag.
	// On a full match it is dropped; on a mismatch it is emitted as text.
	accum strings.Builder
	// tail holds trailing thinking bytes that could still be a prefix of
	// the close tag, carried across deltas so a split </think> is caught.
	tail strings.Builder
}

// Feed processes a content delta and returns zero or more segments in
// emission order. Call Flush after the final Feed to drain any buffered
// content.
func (s *thinkTagSplitter) Feed(content string) []thinkSegment {
	if content == "" {
		return nil
	}
	if s.state == thPassthrough {
		return []thinkSegment{{kind: segText, text: content}}
	}

	var segments []thinkSegment
	for i := 0; i < len(content); i++ {
		if s.state == thScanningOpen {
			if isSpace(content[i]) && s.accum.Len() == 0 {
				continue // skip leading whitespace before the open tag
			}
			s.accum.WriteByte(content[i])
			cur := s.accum.String()
			if cur == thinkOpenTag {
				// Full open tag matched — everything after is thinking.
				s.state = thInsideThink
				s.accum.Reset()
				continue
			}
			if strings.HasPrefix(thinkOpenTag, cur) {
				continue // still a viable prefix; keep buffering
			}
			// Definitive non-match: emit the buffered prefix plus the
			// rest of this delta as text, then pass everything through.
			s.state = thPassthrough
			s.accum.Reset()
			segments = append(segments, thinkSegment{kind: segText, text: cur + content[i+1:]})
			return segments
		}

		// thInsideThink: consume the rest of the delta in one pass.
		combined := s.tail.String() + content[i:]
		s.tail.Reset()
		if idx := strings.Index(combined, thinkCloseTag); idx >= 0 {
			if idx > 0 {
				segments = append(segments, thinkSegment{kind: segThinking, text: combined[:idx]})
			}
			s.state = thPassthrough
			if rem := combined[idx+len(thinkCloseTag):]; rem != "" {
				segments = append(segments, thinkSegment{kind: segText, text: rem})
			}
			return segments
		}
		// No close tag yet. Everything except a possible trailing
		// close-tag prefix is safe to emit as thinking; keep the tail.
		keep := len(thinkCloseTag) - 1
		if len(combined) > keep {
			segments = append(segments, thinkSegment{kind: segThinking, text: combined[:len(combined)-keep]})
			s.tail.WriteString(combined[len(combined)-keep:])
		} else {
			s.tail.WriteString(combined)
		}
		return segments
	}
	return segments
}

// Flush drains buffered content at stream end. An unclosed <think>
// surfaces as thinking (the model was truncated or malformed); a
// buffered partial open-tag surfaces as text.
func (s *thinkTagSplitter) Flush() []thinkSegment {
	var segments []thinkSegment
	switch s.state {
	case thScanningOpen:
		if s.accum.Len() > 0 {
			segments = append(segments, thinkSegment{kind: segText, text: s.accum.String()})
		}
	case thInsideThink:
		if s.tail.Len() > 0 {
			segments = append(segments, thinkSegment{kind: segThinking, text: s.tail.String()})
		}
	}
	s.accum.Reset()
	s.tail.Reset()
	s.state = thPassthrough
	if len(segments) == 0 {
		return nil
	}
	return segments
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
