// Package toolcheck validates model-emitted tool_use blocks against the
// tools[].input_schema on the inbound request and deterministically repairs
// safe failure modes, replacing prior per-failure patches (#284, #293,
// #327/#333, #339) with one layered pipeline:
//
//  1. normalize — drop empty-string/null OPTIONAL params (gpt-5.x Responses
//     failure mode; required params untouched)
//  2. parse     — minimal repair for malformed argument JSON
//  3. validate  — Draft-7 validation against the original schema
//  4. repair    — validation-error-driven safe coercions, re-validated
//
// Fail-open throughout: an uncompilable schema, validator panic, or nil
// *Validator forwards the block as-is and reports via the returned Issue.
package toolcheck

import (
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"workweave/router/internal/observability"
)

// localePrinter renders validation-error messages for Issue.Detail.
var localePrinter = message.NewPrinter(language.English)

// Bucket classifies why a tool_use block failed validation.
type Bucket string

const (
	// BucketInvalidJSON means the argument payload was not parseable JSON.
	BucketInvalidJSON Bucket = "invalid_json"
	// BucketUnknownTool means the tool name is not in the request's tool set.
	BucketUnknownTool Bucket = "unknown_tool"
	// BucketSchemaMismatch means the arguments parsed but violate the tool's
	// input_schema.
	BucketSchemaMismatch Bucket = "schema_mismatch"
)

// maxDetailBytes caps Issue.Detail so a pathological validation error can't
// bloat telemetry.
const maxDetailBytes = 300

// maxSchemaBytes is the fail-open guard: schemas larger than this are not
// compiled and the tool is treated as uncheckable.
const maxSchemaBytes = 256 * 1024

// maxArgsBytes bounds how much argument JSON the repair pipeline touches;
// larger payloads (e.g. a huge Write) are validated but not repaired.
const maxArgsBytes = 4 * 1024 * 1024

// Issue describes one offending tool_use block. It is carried on the
// translator response summaries and logged by the proxy.
type Issue struct {
	ToolName string
	Bucket   Bucket
	// Detail is the first validation error (instance path + message),
	// truncated to maxDetailBytes.
	Detail string
	// Repaired is true when repair produced a result that passes validation.
	Repaired bool
	// Actions lists the repair actions applied, including ones applied on a
	// path that ultimately still failed validation.
	Actions []string
}

// Verdict is the outcome of checking one tool_use block.
type Verdict struct {
	// OK is true when the block was valid as emitted; normalize-only
	// cleanups do not clear it (preserves #339 silent-strip semantics).
	OK bool
	// Args is the JSON to forward: repaired on success, else the normalized
	// original. "{}" is the last resort for unparseable JSON only.
	Args string
	// Issue is nil when OK.
	Issue *Issue
}

// toolSchema is the per-tool validation state.
type toolSchema struct {
	// compiled is nil when the schema could not be compiled (fail-open:
	// the tool is then uncheckable and args pass through normalize only).
	compiled *jsonschema.Schema
	// required is the set of required top-level parameter names; the
	// normalize pass only drops params NOT in this set.
	required map[string]struct{}
}

// Validator holds compiled schemas for one request's tool set. A nil
// *Validator is valid and checks nothing. Safe for concurrent use after
// Compile — schemas and sets are read-only.
type Validator struct {
	tools map[string]*toolSchema
}

// Compile parses an Anthropic `tools` array into a Validator. It never
// returns an error: a per-tool compile failure marks that tool uncheckable
// (logged at WARN). Returns nil when toolsRaw carries no usable tools.
func Compile(toolsRaw []byte) *Validator {
	parsed := gjson.ParseBytes(toolsRaw)
	if !parsed.IsArray() {
		return nil
	}
	tools := make(map[string]*toolSchema)
	parsed.ForEach(func(_, tool gjson.Result) bool {
		name := tool.Get("name").String()
		if name == "" {
			return true
		}
		ts := &toolSchema{required: make(map[string]struct{})}
		schema := tool.Get("input_schema")
		schema.Get("required").ForEach(func(_, r gjson.Result) bool {
			ts.required[r.String()] = struct{}{}
			return true
		})
		ts.compiled = compileSchema(name, schema)
		tools[name] = ts
		return true
	})
	if len(tools) == 0 {
		return nil
	}
	return &Validator{tools: tools}
}

// compileSchema compiles one tool's input_schema, returning nil (uncheckable)
// on failure or panic. Draft-7 is the default dialect since Anthropic
// input_schemas rarely declare $schema.
func compileSchema(name string, schema gjson.Result) (compiled *jsonschema.Schema) {
	if !schema.IsObject() || len(schema.Raw) > maxSchemaBytes {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			observability.Get().Warn("toolcheck: schema compile panic", "tool_name", name, "panic", fmt.Sprint(r))
			compiled = nil
		}
	}()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema.Raw))
	if err != nil {
		observability.Get().Warn("toolcheck: schema unmarshal failed", "tool_name", name, "err", err)
		return nil
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	const url = "inmem://tool/input_schema.json"
	if err := compiler.AddResource(url, doc); err != nil {
		observability.Get().Warn("toolcheck: schema add resource failed", "tool_name", name, "err", err)
		return nil
	}
	compiled, err = compiler.Compile(url)
	if err != nil {
		observability.Get().Warn("toolcheck: schema compile failed", "tool_name", name, "err", err)
		return nil
	}
	return compiled
}

// KnownTool reports whether name is in the request's tool set. A nil
// receiver reports true (fail-open).
func (v *Validator) KnownTool(name string) bool {
	if v == nil || len(v.tools) == 0 {
		return true
	}
	_, ok := v.tools[name]
	return ok
}

// Check validates argsJSON for the named tool, repairing on failure and
// re-validating. A nil receiver still runs the parse tier (malformed JSON
// is invalid regardless of schema) but skips the schema-aware tiers.
func (v *Validator) Check(name, argsJSON string) Verdict {
	// Models commonly emit no argument payload for zero-param tools.
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}

	args := argsJSON
	var actions []string

	// Unparseable JSON gets one minimal-repair attempt; "{}" is the last
	// resort (matches prior translator behavior, now reported not silent).
	if !gjson.Valid(args) {
		fixed, fixActions := repairJSON(args)
		actions = append(actions, fixActions...)
		if fixed == "" || !gjson.Valid(fixed) {
			return Verdict{
				Args: "{}",
				Issue: &Issue{
					ToolName: name,
					Bucket:   BucketInvalidJSON,
					Detail:   truncateDetail("unparseable tool arguments"),
					Actions:  append(actions, "empty_object_fallback"),
				},
			}
		}
		args = fixed
	}
	jsonRepaired := len(actions) > 0

	if v == nil {
		return verdictAfterValidation(name, args, jsonRepaired, actions, nil)
	}

	ts, known := v.tools[name]
	if !known {
		// Streaming already emits the name at content_block_start, so
		// unknown_tool is telemetry-only: forward and report.
		return Verdict{
			Args: args,
			Issue: &Issue{
				ToolName: name,
				Bucket:   BucketUnknownTool,
				Detail:   truncateDetail("tool not present in request tool set"),
				Repaired: false,
				Actions:  actions,
			},
		}
	}

	// Runs even when schema-valid: the client's own tool validation is
	// stricter than JSON Schema (e.g. Read rejects pages:"" that {type:string} accepts).
	args, normActions := normalizeArgs(args, ts.required)
	actions = append(actions, normActions...)

	if ts.compiled == nil {
		// Uncheckable schema: normalize-only pass-through, but a JSON repair
		// is still worth reporting.
		return verdictAfterValidation(name, args, jsonRepaired, actions, nil)
	}

	verr := validate(ts.compiled, args)
	if verr == nil {
		return verdictAfterValidation(name, args, jsonRepaired, actions, nil)
	}

	// Drive safe coercions off the validation errors, then re-validate. On
	// failure forward the normalized original — half-repaired is worse.
	repaired, repairActions := repairArgs(ts.compiled, args, verr)
	if len(repairActions) > 0 {
		if rerr := validate(ts.compiled, repaired); rerr == nil {
			return Verdict{
				Args: repaired,
				Issue: &Issue{
					ToolName: name,
					Bucket:   BucketSchemaMismatch,
					Detail:   detailFromError(verr),
					Repaired: true,
					Actions:  append(actions, repairActions...),
				},
			}
		}
	}
	return Verdict{
		Args: args,
		Issue: &Issue{
			ToolName: name,
			Bucket:   BucketSchemaMismatch,
			Detail:   detailFromError(verr),
			Repaired: false,
			Actions:  actions,
		},
	}
}

// verdictAfterValidation builds the verdict for args that pass (or skip)
// validation: clean unless an earlier JSON repair made it report-worthy.
func verdictAfterValidation(name, args string, jsonRepaired bool, actions []string, _ error) Verdict {
	if !jsonRepaired {
		return Verdict{OK: true, Args: args}
	}
	return Verdict{
		Args: args,
		Issue: &Issue{
			ToolName: name,
			Bucket:   BucketInvalidJSON,
			Detail:   truncateDetail("malformed tool argument JSON (repaired)"),
			Repaired: true,
			Actions:  actions,
		},
	}
}

// validate runs the compiled schema over args, recovering from validator
// panics (fail-open). Decoded via jsonschema.UnmarshalJSON so large
// integers don't false-fail float comparisons.
func validate(schema *jsonschema.Schema, args string) (verr error) {
	defer func() {
		if r := recover(); r != nil {
			observability.Get().Warn("toolcheck: validate panic", "panic", fmt.Sprint(r))
			verr = nil
		}
	}()
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(args))
	if err != nil {
		// gjson accepted it but the strict decoder didn't; treat as
		// uncheckable rather than invalid.
		return nil
	}
	return schema.Validate(instance)
}

// normalizeArgs drops top-level OPTIONAL params whose value is "" or null.
// Empty-string optionals are the gpt-5.x /chat-era failure mode (#339);
// null optionals come from strictified schemas that force every param
// present. Required params are never touched, so a genuinely-missing one
// still surfaces downstream.
func normalizeArgs(args string, required map[string]struct{}) (out string, actions []string) {
	parsed := gjson.Parse(args)
	if !parsed.IsObject() {
		return args, nil
	}
	out = args
	parsed.ForEach(func(key, val gjson.Result) bool {
		isEmptyString := val.Type == gjson.String && val.Str == ""
		isNull := val.Type == gjson.Null
		if !isEmptyString && !isNull {
			return true
		}
		if _, req := required[key.String()]; req {
			return true
		}
		next, err := sjson.Delete(out, escapeJSONPathToken(key.String()))
		if err != nil {
			return true
		}
		out = next
		if isEmptyString {
			actions = append(actions, "drop_empty_optional")
		} else {
			actions = append(actions, "drop_null_optional")
		}
		return true
	})
	return out, actions
}

// detailFromError renders the first leaf validation error as
// "/instance/path: message", truncated.
func detailFromError(err error) string {
	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return truncateDetail(err.Error())
	}
	leaf := firstLeaf(verr)
	path := "/" + strings.Join(leaf.InstanceLocation, "/")
	return truncateDetail(path + ": " + leaf.ErrorKind.LocalizedString(localePrinter))
}

func truncateDetail(s string) string {
	if len(s) <= maxDetailBytes {
		return s
	}
	return s[:maxDetailBytes]
}

// firstLeaf walks Causes to the first leaf error.
func firstLeaf(verr *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(verr.Causes) > 0 {
		verr = verr.Causes[0]
	}
	return verr
}

// escapeJSONPathToken escapes a raw object key for use in a gjson/sjson path.
func escapeJSONPathToken(token string) string {
	var b strings.Builder
	for _, r := range token {
		switch r {
		case '.', '*', '?', '\\', '|', '#', '@':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
