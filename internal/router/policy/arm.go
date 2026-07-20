package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"workweave/router/internal/router"
)

// ArmIdentity is the complete immutable identity of one dispatchable temporal-Q
// action. It intentionally mirrors temporal_q.ids.make_arm_id.
type ArmIdentity struct {
	CanonicalModel               string `json:"canonical_model"`
	Endpoint                     string `json:"endpoint"`
	ModelRevision                string `json:"model_revision"`
	Provider                     string `json:"provider"`
	ReasoningConfigurationSHA256 string `json:"reasoning_configuration_sha256"`
	ToolConfigurationSHA256      string `json:"tool_configuration_sha256"`
	UpstreamID                   string `json:"upstream_id"`
}

// ArmContext identifies request-scoped compatibility settings that affect a
// dispatched model action.
type ArmContext struct {
	Endpoint                     string
	ReasoningConfigurationSHA256 string
	ToolConfigurationSHA256      string
}

// MakeArmID returns a deterministic cross-language identity for an arm.
func MakeArmID(identity ArmIdentity) string {
	payload, err := json.Marshal(identity)
	if err != nil {
		panic("marshal temporal-Q arm identity: " + err.Error())
	}
	sum := sha256.Sum256(payload)
	return "tq_arm_" + hex.EncodeToString(sum[:])
}

// DeriveArmContext combines privacy-safe ingress and routing-level configuration.
func DeriveArmContext(req router.Request) ArmContext {
	context := ArmContext{
		Endpoint:                     string(req.TranslationRequirements.Endpoint),
		ReasoningConfigurationSHA256: combineReasoningConfigurationHash(req),
		ToolConfigurationSHA256:      req.ToolConfigurationSHA256,
	}
	if context.Endpoint == "" {
		context.Endpoint = "unknown"
	}
	if context.ToolConfigurationSHA256 == "" {
		context.ToolConfigurationSHA256 = hashToolConfiguration(req)
	}
	return context
}

func combineReasoningConfigurationHash(req router.Request) string {
	routingHash := hashReasoningConfiguration(req)
	if req.ReasoningConfigurationSHA256 == "" {
		return routingHash
	}
	payload := struct {
		IngressHash string `json:"ingress_hash"`
		RoutingHash string `json:"routing_hash"`
	}{
		IngressHash: req.ReasoningConfigurationSHA256,
		RoutingHash: routingHash,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("marshal combined temporal-Q reasoning configuration: " + err.Error())
	}
	return sha256Bytes(encoded)
}

func hashReasoningConfiguration(req router.Request) string {
	forceEffort := ""
	if req.RoutingKnobs != nil {
		forceEffort = strings.ToLower(strings.TrimSpace(req.RoutingKnobs.ForceEffort))
	}
	payload := struct {
		ForceEffort        string `json:"force_effort"`
		ReasoningReplay    bool   `json:"reasoning_replay"`
		ReasoningSignature bool   `json:"reasoning_signature"`
	}{
		ForceEffort:        forceEffort,
		ReasoningReplay:    req.TranslationRequirements.ReasoningReplay,
		ReasoningSignature: req.TranslationRequirements.ReasoningSignature,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("marshal temporal-Q reasoning configuration: " + err.Error())
	}
	return sha256Bytes(encoded)
}

func hashToolConfiguration(req router.Request) string {
	tools := append([]string(nil), req.AvailableTools...)
	sort.Strings(tools)
	payload := struct {
		HasTools bool     `json:"has_tools"`
		Tools    []string `json:"tools"`
	}{
		HasTools: req.HasTools,
		Tools:    tools,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("marshal temporal-Q tool configuration: " + err.Error())
	}
	return sha256Bytes(encoded)
}

func sha256Bytes(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
