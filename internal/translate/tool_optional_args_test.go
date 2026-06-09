package translate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseToolRequiredParams(t *testing.T) {
	body := []byte(`{"tools":[
		{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"pages":{"type":"string"}},"required":["file_path"]}},
		{"name":"Ping","input_schema":{"type":"object","properties":{}}}
	]}`)
	got := parseToolRequiredParams(body)

	read, ok := got["Read"]
	assert.True(t, ok, "Read tool must be present")
	_, fileReq := read["file_path"]
	_, pagesReq := read["pages"]
	assert.True(t, fileReq, "file_path is required")
	assert.False(t, pagesReq, "pages is optional")

	ping, ok := got["Ping"]
	assert.True(t, ok, "tool with no required array still maps to an empty set")
	assert.Empty(t, ping)
}

func TestParseToolRequiredParams_NoTools(t *testing.T) {
	assert.Nil(t, parseToolRequiredParams([]byte(`{"messages":[]}`)))
}

func TestStripEmptyOptionalArgs_DropsEmptyOptional(t *testing.T) {
	// The gpt-5.x failure mode: Read called with an empty optional pages="".
	req := map[string]struct{}{"file_path": {}}
	got := stripEmptyOptionalArgs(`{"file_path":"/a.go","limit":2000,"offset":0,"pages":""}`, req)
	assert.JSONEq(t, `{"file_path":"/a.go","limit":2000,"offset":0}`, got,
		"empty optional pages must be dropped so the client tool doesn't error")
}

func TestStripEmptyOptionalArgs_KeepsEmptyRequired(t *testing.T) {
	// A genuinely-missing required arg must still surface its error downstream,
	// so an empty required value is never stripped.
	req := map[string]struct{}{"file_path": {}}
	got := stripEmptyOptionalArgs(`{"file_path":""}`, req)
	assert.JSONEq(t, `{"file_path":""}`, got)
}

func TestStripEmptyOptionalArgs_KeepsNonEmptyAndNonString(t *testing.T) {
	req := map[string]struct{}{}
	in := `{"pages":"1-5","limit":0,"flag":false,"name":""}`
	got := stripEmptyOptionalArgs(in, req)
	// Only the empty-string optional "name" is dropped; "1-5", 0, false survive.
	assert.JSONEq(t, `{"pages":"1-5","limit":0,"flag":false}`, got)
}

func TestStripEmptyOptionalArgs_NonObjectUnchanged(t *testing.T) {
	assert.Equal(t, `{}`, stripEmptyOptionalArgs(`{}`, nil))
	assert.Equal(t, `[]`, stripEmptyOptionalArgs(`[]`, nil))
}
