package translate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// collect feeds all deltas then flushes, coalescing adjacent segments
// of the same kind. The splitter's streaming contract is that
// consecutive same-kind segments concatenate into the logical content;
// it deliberately does not buffer whole responses to merge them, so the
// test merges them to assert on the logical result.
func (s *thinkTagSplitter) collect(deltas ...string) []thinkSegment {
	var raw []thinkSegment
	for _, d := range deltas {
		raw = append(raw, s.Feed(d)...)
	}
	raw = append(raw, s.Flush()...)
	return coalesce(raw)
}

func coalesce(in []thinkSegment) []thinkSegment {
	var out []thinkSegment
	for _, seg := range in {
		if n := len(out); n > 0 && out[n-1].kind == seg.kind {
			out[n-1].text += seg.text
			continue
		}
		out = append(out, seg)
	}
	return out
}

func TestThinkTagClean(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("<think>reasoning here</think>answer text")
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "reasoning here"},
		{kind: segText, text: "answer text"},
	}, segs)
}

func TestThinkTagSplitAcrossDeltas(t *testing.T) {
	var s thinkTagSplitter
	// Tag split: <think> + part of close tag
	segs := s.collect("<think>in", "ner</", "think>answer")
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "inner"},
		{kind: segText, text: "answer"},
	}, segs)
}

func TestThinkTagCloseSplit(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("<think>body</", "think>after")
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "body"},
		{kind: segText, text: "after"},
	}, segs)
}

func TestThinkTagLeadingWhitespace(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("  \n\r <think>hello</think>done")
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "hello"},
		{kind: segText, text: "done"},
	}, segs)
}

func TestThinkTagNoTag(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("just plain text, no tags")
	assert.Equal(t, []thinkSegment{
		{kind: segText, text: "just plain text, no tags"},
	}, segs)
}

func TestThinkTagMidProseStaysText(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("answer with <think>mid-prose</think> inside")
	// Opening tag doesn't start content → passthrough.
	assert.Equal(t, []thinkSegment{
		{kind: segText, text: "answer with <think>mid-prose</think> inside"},
	}, segs)
}

func TestThinkTagUnclosedFlushAsThinking(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("<think>all my thoughts")
	segs = append(segs, s.Flush()...)
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "all my thoughts"},
	}, segs)
}

func TestThinkTagTextTrailingCloseTagSameDelta(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("<think>x</think>trailing")
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "x"},
		{kind: segText, text: "trailing"},
	}, segs)
}

func TestThinkTagEmpty(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("")
	assert.Empty(t, segs)
}

func TestThinkTagFlushEmpty(t *testing.T) {
	var s thinkTagSplitter
	s.Feed("plain text")
	segs := s.Flush()
	assert.Empty(t, segs)
}

func TestThinkTagOnlyWhitespacePrefix(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("   ", "   ")
	assert.Empty(t, segs)
}

func TestThinkTagCharByChar(t *testing.T) {
	var s thinkTagSplitter
	// Feed one character at a time.
	input := "<think>a</think>b"
	var segs []thinkSegment
	for i := range input {
		segs = append(segs, s.Feed(string(input[i]))...)
	}
	segs = append(segs, s.Flush()...)
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "a"},
		{kind: segText, text: "b"},
	}, segs)
}

func TestThinkTagPartialOpenThenFull(t *testing.T) {
	var s thinkTagSplitter
	// First attempt fails, second feeds a complete leading think.
	segs := s.collect("<tool>x")
	// First <t doesn't match <tool; entire input is text.
	assert.Equal(t, []thinkSegment{
		{kind: segText, text: "<tool>x"},
	}, segs)
}

func TestThinkTagFlushInsideScanning(t *testing.T) {
	var s thinkTagSplitter
	// Only open partial tag prefix — flush should drop whitespace.
	segs := s.collect("  ")
	segs = append(segs, s.Flush()...)
	assert.Empty(t, segs)
}

func TestThinkTagFlushPendingPartialTag(t *testing.T) {
	var s thinkTagSplitter
	// "<th" is a pending partial open tag. Flush as text.
	segs := s.collect("<th")
	segs = append(segs, s.Flush()...)
	assert.Equal(t, []thinkSegment{
		{kind: segText, text: "<th"},
	}, segs)
}

func TestThinkTagPassthroughAfterMatch(t *testing.T) {
	var s thinkTagSplitter
	segs := s.collect("<think>a</think>b", "c", "d")
	// After close tag, subsequent feeds are verbatim text.
	assert.Equal(t, []thinkSegment{
		{kind: segThinking, text: "a"},
		{kind: segText, text: "bcd"},
	}, segs)
}
