// Package main implements the multimodal-router plugin for CLIProxyAPI (CPA).
//
// The plugin acts as both a ModelRouter and an Executor. It deep-scans the
// full request "messages" history for multimodal content (images, audio,
// documents). When multimodal content is found, it redirects the request to
// this plugin's executor, rewrites the body "model" field to a configured
// multimodal-capable target model, and forwards execution through the host.
// Pure-text requests are left untouched and pass through unchanged.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	pluginabi "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	pluginapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func main() {}

var pluginVersion = "1.0.0"

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config holds the plugin configuration delivered through plugin.reconfigure.
type Config struct {
	Enabled bool `json:"enabled"`
	// MultimodalModel is the target model requests are redirected to when
	// multimodal content is detected. Empty means the plugin detects and logs
	// but never redirects (fail-safe no-op).
	MultimodalModel string `json:"multimodal_model"`
	// TextModelsRaw is the raw text_models configuration value. It may be a
	// string array (YAML block/flow list or JSON array) or a legacy
	// comma-separated string, normalized into textModels. Empty disables
	// inspection: the plugin never takes over a request.
	TextModelsRaw string `json:"text_models"`
	// LogDecision enables a host.log line per routing decision.
	LogDecision bool `json:"log_decision"`
	// textModels is the normalized, lowercased text-only model list. Only
	// models on this list are inspected for multimodal content; unlisted
	// models are assumed to support multimodal input and pass through
	// unchanged. Empty disables inspection entirely.
	textModels []string
}

func defaultConfig() Config {
	return Config{Enabled: true, LogDecision: true}
}

// configPayload captures raw config fields so text_models can be accepted as
// either a string or an array before normalization.
type configPayload struct {
	Enabled         *bool           `json:"enabled"`
	MultimodalModel string          `json:"multimodal_model"`
	TextModels      json.RawMessage `json:"text_models"`
	LogDecision     *bool           `json:"log_decision"`
}

// decodeConfig parses the raw JSON config payload and validates field types.
func decodeConfig(raw json.RawMessage) (Config, error) {
	cfg := defaultConfig()
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
		return cfg, nil
	}
	var payload configPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Config{}, err
	}
	if payload.Enabled != nil {
		cfg.Enabled = *payload.Enabled
	}
	if payload.LogDecision != nil {
		cfg.LogDecision = *payload.LogDecision
	}
	cfg.MultimodalModel = strings.TrimSpace(payload.MultimodalModel)
	cfg.TextModelsRaw = normalizeTextModelsRaw(payload.TextModels)
	cfg.textModels = parseTextModels(cfg.TextModelsRaw)
	return cfg, nil
}

// normalizeTextModelsRaw converts a raw text_models value (JSON array or JSON
// string) into a canonical comma-separated string. A bare scalar outside JSON
// (e.g. from YAML) is treated as the literal comma-separated string.
func normalizeTextModelsRaw(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, ",")
	}
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		return scalar
	}
	return strings.TrimSpace(string(raw))
}

// normalizeTextModelsScalar canonicalizes a single-line text_models value into
// the internal comma-separated form. It accepts a JSON array (["a","b"]), a
// bare YAML flow array ([a, b]), or a legacy comma-separated string.
func normalizeTextModelsScalar(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if normalized := normalizeTextModelsRaw(json.RawMessage(value)); normalized != value {
		return normalized
	}
	// Not valid JSON (e.g. an unquoted YAML flow array [a, b]): split manually.
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		parts := strings.Split(value[1:len(value)-1], ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, ",")
	}
	return value
}

// parseTextModels splits and normalizes a comma-separated text model list
// into lowercased, trimmed entries.
func parseTextModels(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		m := strings.ToLower(strings.TrimSpace(part))
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

// shouldInspect reports whether the requested model should be inspected for
// multimodal content. Only models on the configured text-only list are
// inspected; unlisted models are assumed to support multimodal input natively
// and pass through unchanged. The empty list disables inspection entirely —
// without an explicit text_models configuration the plugin never takes over a
// request.
func shouldInspect(cfg Config, model string) bool {
	if len(cfg.textModels) == 0 {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, tm := range cfg.textModels {
		if m == tm {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "multimodal-router",
			Version:          pluginVersion,
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/multimodal-router",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable multimodal-aware dynamic model routing."},
				{Name: "multimodal_model", Type: pluginapi.ConfigFieldTypeString, Description: "Target model that multimodal requests are redirected to (e.g. claude-3-5-sonnet-20241022)."},
				{Name: "text_models", Type: pluginapi.ConfigFieldTypeArray, Description: "Text-only model array. Only these models are inspected for multimodal content; unlisted models are passed through unchanged. Accepts a YAML/JSON string array (or a legacy comma-separated string). Empty disables inspection."},
				{Name: "log_decision", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Log every routing decision through host.log."},
			},
		},
		Capabilities: registrationCapabilities{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai", "claude", "openai-response"},
			ExecutorOutputFormats: []string{"openai", "claude", "openai-response"},
		},
	}
}

// ---------------------------------------------------------------------------
// Runtime state
// ---------------------------------------------------------------------------

type hostCallback func(method string, request []byte) ([]byte, error)

var (
	loadedConfigMu sync.RWMutex
	loadedCfg      = defaultConfig()

	hostAPIMu      sync.RWMutex
	hostCallbackFn hostCallback
)

func loadedConfig() Config {
	loadedConfigMu.RLock()
	defer loadedConfigMu.RUnlock()
	return loadedCfg
}

func setLoadedConfigForTest(cfg Config) {
	loadedConfigMu.Lock()
	loadedCfg = cfg
	loadedConfigMu.Unlock()
}

func setHostCallback(cb hostCallback) {
	hostAPIMu.Lock()
	hostCallbackFn = cb
	hostAPIMu.Unlock()
}

// ---------------------------------------------------------------------------
// Multimodal detection
// ---------------------------------------------------------------------------

type multimodalResult struct {
	// Multimodal reports whether any message in the history contains a
	// multimodal content block.
	Multimodal bool
	// Kinds lists the distinct detected multimodal block types.
	Kinds []string
}

// detectMultimodal deep-scans every message in the "messages" history.
// It never fails: unparseable bodies return an empty (non-multimodal) result
// so the request can pass through unchanged.
func detectMultimodal(body []byte) multimodalResult {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return multimodalResult{}
	}
	messages, ok := doc["messages"].([]any)
	if !ok {
		return multimodalResult{}
	}
	seen := make(map[string]bool)
	var kinds []string
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, kind := range detectMessageMultimodal(msg) {
			if !seen[kind] {
				seen[kind] = true
				kinds = append(kinds, kind)
			}
		}
	}
	if len(kinds) == 0 {
		return multimodalResult{}
	}
	return multimodalResult{Multimodal: true, Kinds: kinds}
}

// detectMessageMultimodal deep-scans a single message for multimodal content.
//
// The walk is recursive: multimodal blocks nested inside tool_result.content
// (e.g. an "image" block returned by a screenshot tool) or any other wrapping
// structure are detected, not just blocks at the top level of message.content.
//
// OpenAI format:
//  1. content array contains a block with type "image_url"
//  2. content array contains a block with type "input_audio"
//  3. the message block itself carries an image_url or input_audio property
//
// Claude format:
//  1. content array contains a block with type "image"
//  2. content array contains a block with type "document"
//  3. content array contains a block with type "image_url"
//  4. the message block itself carries a source property object
//  5. a tool_result block whose content array contains an "image"/"document"
//     block (returned by tools such as screenshot capture)
func detectMessageMultimodal(msg map[string]any) []string {
	var kinds []string
	seen := make(map[string]bool)
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	walkMultimodal(msg, add)
	return kinds
}

// walkMultimodal recursively walks the value tree and reports any multimodal
// block or property it encounters. JSON unmarshals to a tree of map / []any /
// scalar, so there is no risk of cycles; depth is bounded by the request size.
func walkMultimodal(v any, add func(string)) {
	switch x := v.(type) {
	case map[string]any:
		// A block whose type is a known multimodal content type.
		if typ, _ := x["type"].(string); typ != "" {
			switch typ {
			case "image_url", "input_audio", "image", "document":
				add(typ)
			}
		}
		// A message or block carrying a multimodal payload property directly.
		if _, ok := x["image_url"]; ok {
			add("image_url")
		}
		if _, ok := x["input_audio"]; ok {
			add("input_audio")
		}
		// Claude image/document blocks (and a bare top-level source) carry a
		// "source" object. Skip when the node is already classified by its
		// type so the kind list does not grow with redundant "source" entries.
		if _, ok := x["source"]; ok {
			typ, _ := x["type"].(string)
			if typ != "image" && typ != "document" && typ != "image_url" && typ != "input_audio" {
				add("source")
			}
		}
		for _, child := range x {
			walkMultimodal(child, add)
		}
	case []any:
		for _, item := range x {
			walkMultimodal(item, add)
		}
	}
}

// ---------------------------------------------------------------------------
// Request rewriting
// ---------------------------------------------------------------------------

// rewriteTopLevelModel rewrites the top-level "model" string field to the
// given target. It is idempotent: when the field already equals the target,
// or the body is not parseable JSON, the original body is returned unchanged
// with changed=false.
func rewriteTopLevelModel(body []byte, model string) ([]byte, bool, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return append([]byte(nil), body...), false, nil
	}
	changed := rewriteStringField(doc, "model", model)
	if !changed {
		return append([]byte(nil), body...), false, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func rewriteStringField(doc map[string]any, key, model string) bool {
	v, ok := doc[key].(string)
	if !ok || v == model {
		return false
	}
	doc[key] = model
	return true
}

// ---------------------------------------------------------------------------
// Model routing (model.route)
// ---------------------------------------------------------------------------

type routeDecision struct {
	Handled       bool
	OriginalModel string
	UpstreamModel string
	Kinds         []string
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// routeModel decides whether this plugin should take over execution. It only
// handles requests that (a) are enabled, (b) contain multimodal content, and
// (c) are not already targeting the configured multimodal model.
func routeModel(cfg Config, format, model string, body []byte, callbackID string, call hostCaller) routeDecision {
	target := strings.TrimSpace(cfg.MultimodalModel)
	detected := detectMultimodal(body)
	final := model
	var decision routeDecision

	switch {
	case !cfg.Enabled:
		// Plugin disabled: never handle.
	case !shouldInspect(cfg, model):
		// Model is not on the text-only list: it is assumed to support
		// multimodal input natively, so pass through unchanged.
	case !detected.Multimodal:
		// Pure text: keep the client model untouched.
	case target == "":
		// Multimodal but no target configured: fail-safe no-op.
	case model == target:
		// Already the multimodal target: no redirect needed.
	default:
		final = target
		decision = routeDecision{Handled: true, OriginalModel: model, UpstreamModel: target, Kinds: detected.Kinds}
	}
	logRoute(call, cfg, callbackID, format, model, final, detected.Multimodal, detected.Kinds)
	return decision
}

func protocolLabel(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude":
		return "Claude"
	case "openai", "openai-response":
		return "OpenAI"
	default:
		return strings.ToUpper(strings.TrimSpace(format))
	}
}

// logRoute records one routing decision through host.log, e.g.
//
//	[OpenAI] 多模态: 是 | 原始模型: deepseek-v3 -> 最终模型: claude-3-5-sonnet-20241022
//
// It reads cfg.LogDecision from the same config snapshot the caller used, so
// the decision and its logging cannot diverge.
func logRoute(call hostCaller, cfg Config, callbackID, format, original, final string, multimodal bool, kinds []string) {
	if call == nil {
		return
	}
	if !cfg.LogDecision {
		return
	}
	yesNo := "否"
	if multimodal {
		yesNo = "是"
	}
	message := fmt.Sprintf("[%s] 多模态: %s | 原始模型: %s -> 最终模型: %s", protocolLabel(format), yesNo, original, final)
	fields := map[string]any{}
	if len(kinds) > 0 {
		fields["modalities"] = kinds
	}
	_, _ = call(pluginabi.MethodHostLog, struct {
		HostCallbackID string         `json:"host_callback_id,omitempty"`
		Level          string         `json:"level"`
		Message        string         `json:"message"`
		Fields         map[string]any `json:"fields,omitempty"`
	}{HostCallbackID: callbackID, Level: "info", Message: message, Fields: fields})
}

func handleModelRoute(raw []byte, call hostCaller) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	decision := routeModel(loadedConfig(), req.SourceFormat, req.RequestedModel, req.Body, req.HostCallbackID, call)
	if !decision.Handled {
		return json.Marshal(pluginapi.ModelRouteResponse{Handled: false})
	}
	return json.Marshal(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "multimodal content detected",
	})
}

// ---------------------------------------------------------------------------
// Executor (executor.execute / executor.execute_stream)
// ---------------------------------------------------------------------------

type executorRPCRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
}

type hostModelExecutePayload struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// prepareUpstream computes the effective upstream model and body for a request
// routed to this plugin. Rewriting is idempotent: if the body model already
// equals the target, the body is passed through byte-for-byte. The config is
// passed in so the caller controls when it is snapshotted — the stream path
// snapshots before spawning its forwarding goroutine.
func prepareUpstream(req executorRPCRequest, cfg Config) (model string, body []byte, multimodal bool) {
	model = req.Model
	body = req.OriginalRequest
	if !cfg.Enabled {
		return model, body, false
	}
	target := strings.TrimSpace(cfg.MultimodalModel)
	if target == "" {
		return model, body, false
	}
	if !shouldInspect(cfg, req.Model) {
		return model, body, false
	}
	detected := detectMultimodal(req.OriginalRequest)
	if !detected.Multimodal {
		return model, body, false
	}
	rewritten, changed, err := rewriteTopLevelModel(req.OriginalRequest, target)
	if err != nil {
		// Rewrite failed: degrade to passing the body through unchanged.
		return model, body, true
	}
	if changed {
		body = rewritten
	}
	model = target
	return model, body, true
}

func handleExecutorExecute(raw []byte, call hostCaller) ([]byte, error) {
	var req executorRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	model, body, _ := prepareUpstream(req, loadedConfig())
	hostRaw, err := call(pluginabi.MethodHostModelExecute, hostModelExecutePayload{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: req.SourceFormat,
			ExitProtocol:  req.Format,
			Model:         model,
			Stream:        false,
			Body:          body,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return nil, err
	}
	var hostResp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(hostRaw, &hostResp); err != nil {
		return nil, err
	}
	if hostResp.StatusCode >= 400 {
		return nil, fmt.Errorf("host.model.execute status %d: %s", hostResp.StatusCode, string(hostResp.Body))
	}
	return json.Marshal(pluginapi.ExecutorResponse{Payload: hostResp.Body, Headers: hostResp.Headers})
}

func handleExecutorExecuteStream(raw []byte, call hostCaller) ([]byte, error) {
	var req executorRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("missing plugin stream id")
	}
	return startExecutorStream(req, call, func(streamID, errText string) error {
		_, err := call(pluginabi.MethodHostStreamClose, struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error,omitempty"`
		}{StreamID: streamID, Error: errText})
		return err
	})
}

func startExecutorStream(req executorRPCRequest, call hostCaller, closeStream func(string, string) error) ([]byte, error) {
	// Snapshot the config before the goroutine so the forwarding loop never
	// reads the mutable global config concurrently with a reconfigure.
	cfg := loadedConfig()
	go func() {
		if err := runStreamForward(req, cfg, call); err != nil {
			_ = closeStream(req.StreamID, err.Error())
		}
	}()
	return json.Marshal(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

// runStreamForward bridges the host model stream to the plugin stream. It is a
// transparent pipe: each host chunk is emitted unchanged, with no
// buffering or truncation, preserving streaming latency.
func runStreamForward(req executorRPCRequest, cfg Config, call hostCaller) error {
	model, body, _ := prepareUpstream(req, cfg)
	hostRaw, err := call(pluginabi.MethodHostModelExecuteStream, hostModelExecutePayload{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: req.SourceFormat,
			ExitProtocol:  req.Format,
			Model:         model,
			Stream:        true,
			Body:          body,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return fmt.Errorf("execute stream: %w", err)
	}
	var hostResp struct {
		pluginapi.HostModelStreamResponse
		Body []byte `json:"body"`
	}
	if err := json.Unmarshal(hostRaw, &hostResp); err != nil {
		return fmt.Errorf("decode host stream response: %w", err)
	}
	if hostResp.StatusCode >= 400 {
		return fmt.Errorf("execute stream status %d: %s", hostResp.StatusCode, string(hostResp.Body))
	}
	if hostResp.StreamID == "" {
		return fmt.Errorf("missing host stream id")
	}
	hostStreamID := hostResp.StreamID

	closeHost := func() error {
		_, err := call(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: hostStreamID})
		return err
	}
	closePlugin := func(errText string) error {
		_, err := call(pluginabi.MethodHostStreamClose, struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error,omitempty"`
		}{StreamID: req.StreamID, Error: errText})
		return err
	}
	emit := func(payload []byte) error {
		_, err := call(pluginabi.MethodHostStreamEmit, struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
		}{StreamID: req.StreamID, Payload: payload})
		return err
	}

	for {
		readRaw, err := call(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: hostStreamID})
		if err != nil {
			_ = closeHost()
			return fmt.Errorf("read host stream: %w", err)
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(readRaw, &chunk); err != nil {
			_ = closeHost()
			return fmt.Errorf("decode host stream chunk: %w", err)
		}
		if chunk.Error != "" {
			_ = closeHost()
			if err := closePlugin(chunk.Error); err != nil {
				return fmt.Errorf("close plugin stream: %w", err)
			}
			return nil
		}
		if chunk.Done {
			break
		}
		if err := emit(chunk.Payload); err != nil {
			_ = closeHost()
			return fmt.Errorf("emit stream chunk: %w", err)
		}
	}
	if err := closeHost(); err != nil {
		return fmt.Errorf("close host stream: %w", err)
	}
	if err := closePlugin(""); err != nil {
		return fmt.Errorf("close plugin stream: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Host callback plumbing
// ---------------------------------------------------------------------------

type hostCaller func(method string, payload any) (json.RawMessage, error)

func callHost(method string, payload any) (json.RawMessage, error) {
	hostAPIMu.RLock()
	cb := hostCallbackFn
	hostAPIMu.RUnlock()
	if cb == nil {
		return nil, fmt.Errorf("host API not initialized")
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	responseBytes, err := cb(method, rawPayload)
	if err != nil {
		return nil, err
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(responseBytes, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		if env.Error == nil {
			return nil, fmt.Errorf("host callback %s failed", method)
		}
		return nil, fmt.Errorf("host callback %s failed: %s", method, env.Error.Message)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// ---------------------------------------------------------------------------
// Lifecycle + method dispatch
// ---------------------------------------------------------------------------

func handlePluginRegister(raw []byte) ([]byte, error) {
	return json.Marshal(pluginRegistration())
}

func handlePluginReconfigure(raw []byte) ([]byte, error) {
	cfgRaw, _, err := decodeLifecycleConfig(raw)
	if err != nil {
		return nil, err
	}
	cfg, err := decodeConfig(cfgRaw)
	if err != nil {
		return nil, err
	}
	setLoadedConfigForTest(cfg)
	return json.Marshal(pluginRegistration())
}

func handleExecutorIdentifier() ([]byte, error) {
	return json.Marshal(struct {
		Identifier string `json:"identifier"`
	}{Identifier: "multimodal-router"})
}

func okEnvelope(v any) ([]byte, error) {
	if v == nil {
		v = map[string]any{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func wrapEnvelope(payload []byte, err error) ([]byte, error) {
	if err != nil {
		return errorEnvelope("plugin_error", err.Error()), nil
	}
	return okEnvelope(json.RawMessage(payload))
}

func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(pluginabi.Envelope{
		OK:    false,
		Error: &pluginabi.Error{Code: code, Message: message},
	})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"failed to encode error envelope"}}`)
	}
	return raw
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return wrapEnvelope(handlePluginRegister(request))
	case pluginabi.MethodPluginReconfigure:
		return wrapEnvelope(handlePluginReconfigure(request))
	case pluginabi.MethodModelRoute:
		return wrapEnvelope(handleModelRoute(request, callHost))
	case pluginabi.MethodExecutorIdentifier:
		return wrapEnvelope(handleExecutorIdentifier())
	case pluginabi.MethodExecutorExecute:
		return wrapEnvelope(handleExecutorExecute(request, callHost))
	case pluginabi.MethodExecutorExecuteStream:
		return wrapEnvelope(handleExecutorExecuteStream(request, callHost))
	case pluginabi.MethodExecutorCountTokens:
		return errorEnvelope("unsupported", "executor.count_tokens is not supported by multimodal-router"), nil
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---------------------------------------------------------------------------
// Lifecycle config parsing (YAML delivered by plugin.reconfigure)
// ---------------------------------------------------------------------------

// decodeLifecycleConfig accepts either a raw JSON config or a base64-encoded
// YAML config wrapped as {"config_yaml": "..."}. The latter is parsed line by
// line into the plugin Config, mirroring how the CPA host delivers YAML
// plugin configuration over the RPC boundary.
func decodeLifecycleConfig(raw []byte) (json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	var lifecycle struct {
		ConfigYAML string `json:"config_yaml"`
	}
	if err := json.Unmarshal(trimmed, &lifecycle); err == nil && lifecycle.ConfigYAML != "" {
		decoded, err := base64.StdEncoding.DecodeString(lifecycle.ConfigYAML)
		if err != nil {
			return nil, true, err
		}
		cfg := defaultConfig()
		var textModelsList []string
		inTextModelsList := false
		scanner := bufio.NewScanner(bytes.NewReader(decoded))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if inTextModelsList {
				// Collect the "- item" lines of a YAML block list until the
				// next top-level key (a non-list line) begins.
				if strings.HasPrefix(line, "-") {
					item := unquoteYAMLScalar(strings.TrimSpace(strings.TrimPrefix(line, "-")))
					if item != "" {
						textModelsList = append(textModelsList, item)
					}
					continue
				}
				inTextModelsList = false
			}
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = unquoteYAMLScalar(strings.TrimSpace(value))
			if key == "text_models" && value == "" {
				// A bare "text_models:" key starts a block list below.
				inTextModelsList = true
				continue
			}
			switch key {
			case "enabled":
				cfg.Enabled = strings.EqualFold(value, "true")
			case "multimodal_model":
				cfg.MultimodalModel = value
			case "text_models":
				cfg.TextModelsRaw = normalizeTextModelsScalar(value)
			case "log_decision":
				cfg.LogDecision = strings.EqualFold(value, "true")
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, true, err
		}
		if len(textModelsList) > 0 {
			cfg.TextModelsRaw = strings.Join(textModelsList, ",")
		}
		cfg.textModels = parseTextModels(cfg.TextModelsRaw)
		cfgRaw, err := json.Marshal(cfg)
		if err != nil {
			return nil, true, err
		}
		return cfgRaw, true, nil
	}
	return append(json.RawMessage(nil), trimmed...), false, nil
}

func unquoteYAMLScalar(value string) string {
	if len(value) < 2 {
		return value
	}
	quote := value[0]
	if (quote != '"' && quote != '\'') || value[len(value)-1] != quote {
		return value
	}
	return value[1 : len(value)-1]
}
