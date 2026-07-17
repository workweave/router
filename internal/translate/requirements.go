package translate

import (
	"bytes"
	"strings"

	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
)

// TranslationRequirements extracts the semantic contract carried by an
// already-parsed request. It does not decide eligibility; proxy combines this
// value with configured provider families and catalog capabilities.
func (e *RequestEnvelope) TranslationRequirements(endpoint router.TranslationEndpoint) router.TranslationRequirements {
	req := router.TranslationRequirements{
		Endpoint:      endpoint,
		FunctionTools: e.HasTools(),
		Images:        e.HasImages(),
	}
	switch e.format {
	case FormatAnthropic:
		req.SourceFormat = router.WireFormatAnthropic
		req.ReasoningReplay = gjson.GetBytes(e.body, "thinking").Exists() || hasContentType(e.body, "thinking")
		req.ReasoningSignature = gjson.GetBytes(e.body, "messages.#(content.#.signature!=\"\")").Exists()
		req.PromptCacheControl = containsKey(e.body, "cache_control")
		req.StructuredOutput = gjson.GetBytes(e.body, "output_config.format").Exists()
		req.Audio, req.Files = anthropicMediaRequirements(e.body)
		req.CitationsOrSearch = containsAnyKey(e.body, "web_search", "web_fetch", "citations")
	case FormatOpenAI:
		req.SourceFormat = router.WireFormatOpenAI
		req.ReasoningReplay = hasContentType(e.body, "reasoning") || gjson.GetBytes(e.body, "reasoning").Exists()
		req.StructuredOutput = gjson.GetBytes(e.body, "response_format").Exists()
		req.UsageDetail = gjson.GetBytes(e.body, "stream_options.include_usage").Bool()
		req.Audio, req.Files = openAIMediaRequirements(e.body)
		req.CitationsOrSearch = containsAnyKey(e.body, "web_search", "web_search_preview", "file_search", "computer_use")
	case FormatGemini:
		req.SourceFormat = router.WireFormatGemini
		req.ReasoningSignature = containsKey(e.body, "thoughtSignature") || containsKey(e.body, "thought_signature")
		req.StructuredOutput = gjson.GetBytes(e.body, "generationConfig.responseSchema").Exists()
		req.CitationsOrSearch = gjson.GetBytes(e.body, "tools.#(googleSearch!=null)").Exists() || gjson.GetBytes(e.body, "tools.#(google_search!=null)").Exists()
		req.Audio, req.Files = geminiMediaRequirements(e.body)
	}
	return req
}

func hasContentType(body []byte, typ string) bool {
	return gjson.GetBytes(body, "messages.#(content.#.type==\""+typ+"\")").Exists() ||
		gjson.GetBytes(body, "input.#(type==\""+typ+"\")").Exists()
}

func containsKey(body []byte, key string) bool {
	return bytes.Contains(body, []byte(`"`+key+`"`))
}

func containsAnyKey(body []byte, keys ...string) bool {
	for _, key := range keys {
		if containsKey(body, key) {
			return true
		}
	}
	return false
}

// anthropicMediaRequirements identifies non-image content blocks. Image
// support is already represented by HasImages; files and audio need their own
// compatibility constraints because sending them through a text translation
// can silently discard provider-specific fields.
func anthropicMediaRequirements(body []byte) (audio, files bool) {
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "audio", "input_audio":
				audio = true
			case "document", "file", "input_file":
				files = true
			}
			return !audio || !files
		})
		return !audio || !files
	})
	return audio, files
}

// openAIMediaRequirements covers both Chat content parts and the Responses
// input union. The latter is scanned recursively because Responses message
// items nest their content under input[].content rather than messages[].
func openAIMediaRequirements(body []byte) (audio, files bool) {
	visitOpenAIMedia(gjson.ParseBytes(body), &audio, &files)
	return audio, files
}

func visitOpenAIMedia(value gjson.Result, audio, files *bool) {
	if value.IsObject() {
		typ := value.Get("type").String()
		switch typ {
		case "input_audio", "audio", "audio_url":
			*audio = true
		case "input_file", "file", "file_url":
			*files = true
		}
		value.ForEach(func(_, nested gjson.Result) bool {
			visitOpenAIMedia(nested, audio, files)
			return !*audio || !*files
		})
		return
	}
	if value.IsArray() {
		value.ForEach(func(_, nested gjson.Result) bool {
			visitOpenAIMedia(nested, audio, files)
			return !*audio || !*files
		})
	}
}

// geminiMediaRequirements categorizes native Gemini media by MIME type. An
// unknown media type is treated as a file, which is conservative: unlike an
// image, it has no portable translation path today.
func geminiMediaRequirements(body []byte) (audio, files bool) {
	gjson.GetBytes(body, "contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			media := part.Get("inlineData")
			if !media.Exists() {
				media = part.Get("inline_data")
			}
			if !media.Exists() {
				media = part.Get("fileData")
			}
			if !media.Exists() {
				media = part.Get("file_data")
			}
			if !media.Exists() {
				return true
			}
			mimeType := strings.ToLower(media.Get("mimeType").String())
			if mimeType == "" {
				mimeType = strings.ToLower(media.Get("mime_type").String())
			}
			switch {
			case strings.HasPrefix(mimeType, "audio/"):
				audio = true
			case !strings.HasPrefix(mimeType, "image/"):
				files = true
			}
			return !audio || !files
		})
		return !audio || !files
	})
	return audio, files
}
