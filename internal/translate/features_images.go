package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// dataURLBase64Marker precedes an OpenAI data-URL's base64 payload.
const dataURLBase64Marker = ";base64,"

// base64ImageStats returns the total base64 byte length and count of inline
// image payloads in the body, dispatched by inbound format. Both byte-based
// token estimators subtract these bytes and add imageTokenEstimate per image,
// because base64 image transport size bears no relation to the model's
// dimension-based image token cost. Referenced media (http image URLs, Gemini
// fileData URIs) carry no in-body payload and are ignored.
func (e *RequestEnvelope) base64ImageStats() (bytes, count int) {
	switch e.format {
	case FormatAnthropic:
		return anthropicImageBytes(e.body)
	case FormatOpenAI:
		return openAIImageBytes(e.body)
	case FormatGemini:
		return geminiImageBytes(e.body)
	default:
		return 0, 0
	}
}

// jsonStringBytes returns a JSON string value's byte length without
// materializing it. Base64 payloads contain no JSON escapes, so the raw quoted
// length minus the two surrounding quotes is exact.
func jsonStringBytes(v gjson.Result) int {
	if n := len(v.Raw); n >= 2 {
		return n - 2
	}
	return 0
}

// anthropicImageBytes sums base64 image payloads in an Anthropic body, covering
// both top-level image blocks and images nested inside tool_result content —
// the shape the Read tool emits for a rendered PDF (one image block per page).
func anthropicImageBytes(body []byte) (total, count int) {
	addImage := func(block gjson.Result) {
		if data := block.Get("source.data"); data.Exists() {
			total += jsonStringBytes(data)
			count++
		}
	}
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "image":
				addImage(block)
			case "tool_result":
				block.Get("content").ForEach(func(_, inner gjson.Result) bool {
					if inner.Get("type").String() == "image" {
						addImage(inner)
					}
					return true
				})
			}
			return true
		})
		return true
	})
	return total, count
}

// openAIImageBytes sums base64 payloads carried in OpenAI image_url data URLs.
// http(s) image URLs carry no in-body bytes and are skipped.
func openAIImageBytes(body []byte) (total, count int) {
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() != "image_url" {
				return true
			}
			raw := part.Get("image_url.url").Raw
			i := strings.Index(raw, dataURLBase64Marker)
			if i < 0 {
				return true
			}
			// From just after the marker to the closing quote.
			if n := len(raw) - i - len(dataURLBase64Marker) - 1; n > 0 {
				total += n
				count++
			}
			return true
		})
		return true
	})
	return total, count
}

// geminiImageBytes sums base64 payloads in Gemini inlineData parts (camelCase
// and snake_case). fileData URIs carry no in-body payload and are skipped.
func geminiImageBytes(body []byte) (total, count int) {
	gjson.GetBytes(body, "contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			data := part.Get("inlineData.data")
			if !data.Exists() {
				data = part.Get("inline_data.data")
			}
			if data.Exists() {
				total += jsonStringBytes(data)
				count++
			}
			return true
		})
		return true
	})
	return total, count
}
