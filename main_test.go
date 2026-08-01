package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	pluginabi "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	pluginapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---------------------------------------------------------------------------
// Registration metadata
// ---------------------------------------------------------------------------

func TestPluginRegistrationMetadataAndConfigFields(t *testing.T) {
	reg := pluginRegistration()
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version=%d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != "multimodal-router" {
		t.Fatalf("plugin name=%q", reg.Metadata.Name)
	}
	if reg.Metadata.Version != pluginVersion || reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata missing CPA-required management fields: %#v", reg.Metadata)
	}
	if !reg.Capabilities.ModelRouter || !reg.Capabilities.Executor {
		t.Fatalf("capabilities=%#v, want model router and executor", reg.Capabilities)
	}
	if reg.Capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeStatic) {
		t.Fatalf("executor scope=%q", reg.Capabilities.ExecutorModelScope)
	}
	if !reflect.DeepEqual(reg.Capabilities.ExecutorInputFormats, []string{"openai", "claude", "openai-response"}) {
		t.Fatalf("executor input formats=%v", reg.Capabilities.ExecutorInputFormats)
	}
	if !reflect.DeepEqual(reg.Capabilities.ExecutorOutputFormats, []string{"openai", "claude", "openai-response"}) {
		t.Fatalf("executor output formats=%v", reg.Capabilities.ExecutorOutputFormats)
	}
	wantFields := []string{"enabled", "multimodal_model", "text_models", "log_decision"}
	got := make([]string, 0, len(reg.Metadata.ConfigFields))
	for _, field := range reg.Metadata.ConfigFields {
		got = append(got, field.Name)
		if field.Description == "" {
			t.Fatalf("config field %q has empty description", field.Name)
		}
	}
	if !reflect.DeepEqual(got, wantFields) {
		t.Fatalf("config fields=%v, want %v", got, wantFields)
	}
}

// ---------------------------------------------------------------------------
// Config decoding
// ---------------------------------------------------------------------------

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, err := decodeConfig(nil)
	if err != nil {
		t.Fatalf("decodeConfig nil error = %v", err)
	}
	if !cfg.Enabled || !cfg.LogDecision {
		t.Fatalf("decoded defaults=%#v, want enabled+log_decision on", cfg)
	}
	if cfg.MultimodalModel != "" {
		t.Fatalf("default multimodal_model=%q, want empty", cfg.MultimodalModel)
	}
}

func TestDecodeConfigStringAndArrayTextModels(t *testing.T) {
	raw := json.RawMessage(`{"enabled":true,"multimodal_model":"claude-3-5-sonnet-20241022","text_models":"deepseek-v3,gpt-3.5-turbo","log_decision":true}`)
	cfg, err := decodeConfig(raw)
	if err != nil {
		t.Fatalf("decodeConfig error = %v", err)
	}
	if cfg.MultimodalModel != "claude-3-5-sonnet-20241022" {
		t.Fatalf("multimodal_model=%q", cfg.MultimodalModel)
	}
	if cfg.TextModelsRaw != "deepseek-v3,gpt-3.5-turbo" {
		t.Fatalf("text_models raw=%q", cfg.TextModelsRaw)
	}
	if !reflect.DeepEqual(cfg.textModels, []string{"deepseek-v3", "gpt-3.5-turbo"}) {
		t.Fatalf("text_models parsed=%v", cfg.textModels)
	}

	rawArr := json.RawMessage(`{"text_models":["gpt-4o-mini","claude-haiku-4.5"]}`)
	cfg, err = decodeConfig(rawArr)
	if err != nil {
		t.Fatalf("decodeConfig array error = %v", err)
	}
	if cfg.TextModelsRaw != "gpt-4o-mini,claude-haiku-4.5" {
		t.Fatalf("array text_models raw=%q", cfg.TextModelsRaw)
	}
	if !reflect.DeepEqual(cfg.textModels, []string{"gpt-4o-mini", "claude-haiku-4.5"}) {
		t.Fatalf("array text_models parsed=%v", cfg.textModels)
	}
}

func TestShouldInspect(t *testing.T) {
	empty := Config{}
	if shouldInspect(empty, "anything") {
		t.Fatal("empty list disables inspection entirely")
	}
	cfg := Config{textModels: []string{"deepseek-v3", "gpt-3.5-turbo"}}
	if !shouldInspect(cfg, "DEEPSEEK-v3") {
		t.Fatal("list match should be case-insensitive")
	}
	if shouldInspect(cfg, "gpt-4o") {
		t.Fatal("unlisted model should not be inspected")
	}
}

// ---------------------------------------------------------------------------
// Multimodal detection matrix
// ---------------------------------------------------------------------------

func TestDetectMultimodal(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "openai image_url in content array",
			body: `{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			want: true,
		},
		{
			name: "openai input_audio in content array",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`,
			want: true,
		},
		{
			name: "direct image_url property on message",
			body: `{"model":"gpt-4o","messages":[{"role":"user","image_url":{"url":"https://example.com/a.png"}}]}`,
			want: true,
		},
		{
			name: "direct input_audio property on message",
			body: `{"model":"gpt-4o","messages":[{"role":"user","input_audio":{"data":"AAAA","format":"wav"}}]}`,
			want: true,
		},
		{
			name: "claude image block",
			body: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`,
			want: true,
		},
		{
			name: "claude document block",
			body: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`,
			want: true,
		},
		{
			name: "claude image_url block",
			body: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"image_url","url":"https://example.com/a.png"}]}]}`,
			want: true,
		},
		{
			name: "claude source property on message",
			body: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}]}`,
			want: true,
		},
		{
			name: "historical image followed by text-only turn",
			body: `{"model":"deepseek-v3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://e.com/a.png"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"what was the image?"}]}`,
			want: true,
		},
		{
			name: "pure text",
			body: `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`,
			want: false,
		},
		{
			name: "content with only text blocks",
			body: `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			want: false,
		},
		{
			name: "malformed json",
			body: `{"model":`,
			want: false,
		},
		{
			name: "no messages array",
			body: `{"model":"gpt-4o"}`,
			want: false,
		},
		{
			name: "empty messages",
			body: `{"model":"gpt-4o","messages":[]}`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectMultimodal([]byte(tt.body))
			if got.Multimodal != tt.want {
				t.Fatalf("detectMultimodal=%v, want %v (kinds=%v)", got.Multimodal, tt.want, got.Kinds)
			}
		})
	}
}

func TestDetectMultimodalKinds(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]},
		{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"y"}}]}
	]}`
	got := detectMultimodal([]byte(body))
	if !got.Multimodal {
		t.Fatal("expected multimodal")
	}
	if !reflect.DeepEqual(got.Kinds, []string{"image_url", "input_audio"}) {
		t.Fatalf("kinds=%v", got.Kinds)
	}
}

// ---------------------------------------------------------------------------
// Request rewriting
// ---------------------------------------------------------------------------

func TestRewriteTopLevelModel(t *testing.T) {
	body := []byte(`{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":"hi"}]}`)
	out, changed, err := rewriteTopLevelModel(body, "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatalf("rewrite error = %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body not valid JSON: %v", err)
	}
	if doc["model"] != "claude-3-5-sonnet-20241022" {
		t.Fatalf("rewritten model=%v", doc["model"])
	}
	// Non-model fields must survive untouched.
	if _, ok := doc["messages"]; !ok {
		t.Fatal("messages lost during rewrite")
	}
}

func TestRewriteTopLevelModelIdempotent(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[]}`)
	out, changed, err := rewriteTopLevelModel(body, "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatalf("rewrite error = %v", err)
	}
	if changed {
		t.Fatal("idempotent rewrite should not report changed")
	}
	if string(out) != string(body) {
		t.Fatal("idempotent rewrite must return the body byte-for-byte")
	}
}

func TestRewriteTopLevelModelNonJSON(t *testing.T) {
	body := []byte(`not json`)
	out, changed, err := rewriteTopLevelModel(body, "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("rewrite error = %v", err)
	}
	if changed {
		t.Fatal("non-JSON should not be changed")
	}
	if string(out) != string(body) {
		t.Fatal("non-JSON must pass through byte-for-byte")
	}
}

func TestRewriteTopLevelModelMissingModelField(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	_, changed, err := rewriteTopLevelModel(body, "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("rewrite error = %v", err)
	}
	if changed {
		t.Fatal("body without model field must not be changed")
	}
}

// ---------------------------------------------------------------------------
// Model route decisions
// ---------------------------------------------------------------------------

func testBody(payload string) []byte { return []byte(payload) }

func TestRouteModelDecisions(t *testing.T) {
	cfg := Config{Enabled: true, MultimodalModel: "claude-3-5-sonnet-20241022", LogDecision: false}
	cfg.textModels = []string{"gpt-3.5-turbo", "gpt-4o-mini", "claude-3-5-sonnet-20241022"}
	multimodal := `{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`
	text := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

	t.Run("multimodal redirects", func(t *testing.T) {
		d := routeModel(cfg, "openai", "gpt-3.5-turbo", testBody(multimodal), "", nil)
		if !d.Handled || d.UpstreamModel != "claude-3-5-sonnet-20241022" || d.OriginalModel != "gpt-3.5-turbo" {
			t.Fatalf("decision=%#v", d)
		}
	})

	t.Run("pure text passes through", func(t *testing.T) {
		d := routeModel(cfg, "openai", "gpt-4o-mini", testBody(text), "", nil)
		if d.Handled {
			t.Fatalf("text request must not be handled: %#v", d)
		}
	})

	t.Run("already target model passes through", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"x"}}]}]}`
		d := routeModel(cfg, "claude", "claude-3-5-sonnet-20241022", testBody(body), "", nil)
		if d.Handled {
			t.Fatalf("already-target request must not be handled: %#v", d)
		}
	})

	t.Run("disabled plugin never handles", func(t *testing.T) {
		disabled := Config{Enabled: false, MultimodalModel: "claude-3-5-sonnet"}
		d := routeModel(disabled, "openai", "gpt-3.5-turbo", testBody(multimodal), "", nil)
		if d.Handled {
			t.Fatalf("disabled plugin must not handle: %#v", d)
		}
	})

	t.Run("empty target is a no-op", func(t *testing.T) {
		noTarget := Config{Enabled: true, MultimodalModel: "", LogDecision: false}
		noTarget.textModels = []string{"gpt-3.5-turbo"}
		d := routeModel(noTarget, "openai", "gpt-3.5-turbo", testBody(multimodal), "", nil)
		if d.Handled {
			t.Fatalf("empty target must not handle: %#v", d)
		}
	})

	t.Run("empty text_models list disables plugin", func(t *testing.T) {
		emptyList := Config{Enabled: true, MultimodalModel: "claude-3-5-sonnet-20241022", LogDecision: false}
		d := routeModel(emptyList, "openai", "gpt-3.5-turbo", testBody(multimodal), "", nil)
		if d.Handled {
			t.Fatalf("empty text_models list must disable routing: %#v", d)
		}
	})

	t.Run("malformed body passes through", func(t *testing.T) {
		d := routeModel(cfg, "openai", "gpt-4o-mini", testBody(`{"model":`), "", nil)
		if d.Handled {
			t.Fatalf("malformed body must not be handled: %#v", d)
		}
	})
}

func TestRouteModelTextModelsList(t *testing.T) {
	multimodal := `{"model":"deepseek-v3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`

	cfg := Config{Enabled: true, MultimodalModel: "claude-3-5-sonnet-20241022", LogDecision: false}
	cfg.textModels = []string{"deepseek-v3"}

	t.Run("listed model inspected and redirected", func(t *testing.T) {
		d := routeModel(cfg, "openai", "deepseek-v3", testBody(multimodal), "", nil)
		if !d.Handled || d.UpstreamModel != "claude-3-5-sonnet-20241022" {
			t.Fatalf("listed text model should redirect: %#v", d)
		}
	})

	t.Run("unlisted model passes through even with multimodal", func(t *testing.T) {
		d := routeModel(cfg, "openai", "gpt-4o", testBody(multimodal), "", nil)
		if d.Handled {
			t.Fatalf("unlisted multimodal-capable model must pass through: %#v", d)
		}
	})

	t.Run("listed model with pure text still passes through", func(t *testing.T) {
		text := `{"model":"deepseek-v3","messages":[{"role":"user","content":"hi"}]}`
		d := routeModel(cfg, "openai", "deepseek-v3", testBody(text), "", nil)
		if d.Handled {
			t.Fatalf("listed model with text must not redirect: %#v", d)
		}
	})
}

// ---------------------------------------------------------------------------
// model.route RPC envelope
// ---------------------------------------------------------------------------

func TestHandleModelRouteRPC(t *testing.T) {
	setLoadedConfigForTest(testConfig())
	defer setLoadedConfigForTest(defaultConfig())

	multimodal := `{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`
	rawReq, err := json.Marshal(rpcModelRouteRequest{
		ModelRouteRequest: pluginapi.ModelRouteRequest{
			SourceFormat:   "openai",
			RequestedModel: "gpt-3.5-turbo",
			Body:           testBody(multimodal),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := handleModelRoute(rawReq, nil)
	if err != nil {
		t.Fatalf("handleModelRoute error = %v", err)
	}
	var route pluginapi.ModelRouteResponse
	if err := json.Unmarshal(resp, &route); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !route.Handled || route.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("route=%#v", route)
	}

	// Pure text must yield Handled=false.
	textRawReq, _ := json.Marshal(rpcModelRouteRequest{
		ModelRouteRequest: pluginapi.ModelRouteRequest{
			SourceFormat:   "openai",
			RequestedModel: "gpt-4o-mini",
			Body:           testBody(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`),
		},
	})
	resp, err = handleModelRoute(textRawReq, nil)
	if err != nil {
		t.Fatalf("handleModelRoute text error = %v", err)
	}
	if err := json.Unmarshal(resp, &route); err != nil {
		t.Fatalf("unmarshal text response: %v", err)
	}
	if route.Handled {
		t.Fatalf("text route must not be handled: %#v", route)
	}
}

// ---------------------------------------------------------------------------
// host.log decision logging
// ---------------------------------------------------------------------------

func TestLogRouteEmitsHostLog(t *testing.T) {
	cfg := testConfig()
	cfg.LogDecision = true
	setLoadedConfigForTest(cfg)
	defer setLoadedConfigForTest(defaultConfig())

	var calls []struct {
		method string
		body   string
	}
	call := func(method string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		calls = append(calls, struct {
			method string
			body   string
		}{method, string(raw)})
		return nil, nil
	}
	logRoute(call, cfg, "cb-1", "openai", "gpt-3.5-turbo", "claude-3-5-sonnet-20241022", true, []string{"image_url"})
	if len(calls) != 1 || calls[0].method != pluginabi.MethodHostLog {
		t.Fatalf("expected one host.log call, got %#v", calls)
	}
	var payload struct {
		HostCallbackID string         `json:"host_callback_id"`
		Level          string         `json:"level"`
		Message        string         `json:"message"`
		Fields         map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &payload); err != nil {
		t.Fatalf("unmarshal log payload: %v", err)
	}
	if payload.HostCallbackID != "cb-1" || payload.Level != "info" {
		t.Fatalf("log payload=%#v", payload)
	}
	if !strings.Contains(payload.Message, "gpt-3.5-turbo") || !strings.Contains(payload.Message, "claude-3-5-sonnet-20241022") {
		t.Fatalf("log message=%q", payload.Message)
	}
	if !strings.Contains(payload.Message, "多模态: 是") {
		t.Fatalf("log message missing multimodal yes marker: %q", payload.Message)
	}
}

func TestLogRouteDisabled(t *testing.T) {
	cfg := Config{Enabled: true, LogDecision: false}
	setLoadedConfigForTest(cfg)
	defer setLoadedConfigForTest(defaultConfig())

	called := false
	call := func(string, any) (json.RawMessage, error) {
		called = true
		return nil, nil
	}
	logRoute(call, cfg, "", "openai", "a", "b", true, nil)
	if called {
		t.Fatal("log_decision=false must suppress host.log")
	}
}

// ---------------------------------------------------------------------------
// Executor: full RPC flow with a mock host
// ---------------------------------------------------------------------------

type mockHost struct {
	mu sync.Mutex
	// nonStreamRequest and streamRequest are written by the mock serving
	// goroutine and read by the test goroutine; guard them for -race.
	nonStreamRequest pluginapi.HostModelExecutionRequest
	streamRequest    pluginapi.HostModelExecutionRequest
	streamChunks     []string
	emitted          []string
	hostStreamID     string
	statusCode       int
}

func (m *mockHost) serve(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostModelExecute:
		var req struct {
			pluginapi.HostModelExecutionRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.nonStreamRequest = req.HostModelExecutionRequest
		statusCode := m.statusCode
		m.mu.Unlock()
		return json.Marshal(pluginapi.HostModelExecutionResponse{
			StatusCode: statusCode,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"id":"x","model":"` + req.Model + `","choices":[]}`),
		})
	case pluginabi.MethodHostModelExecuteStream:
		var req struct {
			pluginapi.HostModelExecutionRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.streamRequest = req.HostModelExecutionRequest
		m.mu.Unlock()
		return json.Marshal(struct {
			pluginapi.HostModelStreamResponse
			Body []byte `json:"body"`
		}{
			HostModelStreamResponse: pluginapi.HostModelStreamResponse{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "host-stream-1",
			},
		})
	case pluginabi.MethodHostModelStreamRead:
		m.mu.Lock()
		if len(m.streamChunks) == 0 {
			m.mu.Unlock()
			return json.Marshal(pluginapi.HostModelStreamReadResponse{Done: true})
		}
		chunk := m.streamChunks[0]
		m.streamChunks = m.streamChunks[1:]
		m.mu.Unlock()
		return json.Marshal(pluginapi.HostModelStreamReadResponse{Payload: []byte(chunk)})
	case pluginabi.MethodHostModelStreamClose:
		return json.Marshal(struct{}{})
	case pluginabi.MethodHostStreamEmit:
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.emitted = append(m.emitted, string(req.Payload))
		m.mu.Unlock()
		return json.Marshal(struct{}{})
	case pluginabi.MethodHostStreamClose:
		return json.Marshal(struct{}{})
	default:
		return nil, fmt.Errorf("unexpected host method %s", method)
	}
}

// snapNonStream returns a copy of the captured non-stream request.
func (m *mockHost) snapNonStream() pluginapi.HostModelExecutionRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneHostReq(m.nonStreamRequest)
}

// snapStream returns a copy of the captured stream request.
func (m *mockHost) snapStream() pluginapi.HostModelExecutionRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneHostReq(m.streamRequest)
}

// snapEmitted returns a copy of the emitted chunks.
func (m *mockHost) snapEmitted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.emitted...)
}

func cloneHostReq(r pluginapi.HostModelExecutionRequest) pluginapi.HostModelExecutionRequest {
	out := r
	out.Body = append([]byte(nil), r.Body...)
	if r.Headers != nil {
		out.Headers = r.Headers.Clone()
	}
	if r.Query != nil {
		clone := make(url.Values, len(r.Query))
		for k, v := range r.Query {
			clone[k] = append([]string(nil), v...)
		}
		out.Query = clone
	}
	return out
}

func newMockCaller(m *mockHost) hostCaller {
	return func(method string, payload any) (json.RawMessage, error) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		out, err := m.serve(method, raw)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(out), nil
	}
}

func testConfig() Config {
	cfg := Config{Enabled: true, MultimodalModel: "claude-3-5-sonnet-20241022", LogDecision: false}
	cfg.textModels = []string{"gpt-3.5-turbo", "deepseek-v3", "gpt-4o-mini", "claude-3-5-haiku", "claude-3-5-sonnet-20241022"}
	return cfg
}

// TC-01 OpenAI 纯文本: 透传原始 model
// TC-02 OpenAI 新发图片: 改写 model
// TC-03 OpenAI 历史带图追问: 改写 model
// TC-04 Claude 文档: 保持 URL 参数, 改写 model
// TC-05 非对话 API / 非 JSON: 原样透传

func TestExecutorNonStreamTC(t *testing.T) {
	setLoadedConfigForTest(testConfig())
	defer setLoadedConfigForTest(defaultConfig())

	multimodalBody := `{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`

	t.Run("TC-02 multimodal body rewritten to target model", func(t *testing.T) {
		m := &mockHost{}
		call := newMockCaller(m)
		req := executorRPCRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				SourceFormat:    "openai",
				Format:          "openai",
				Model:           "gpt-3.5-turbo",
				OriginalRequest: []byte(multimodalBody),
				Headers:         http.Header{"Authorization": []string{"Bearer x"}},
			},
			HostCallbackID: "cb-1",
		}
		rawReq, _ := json.Marshal(req)
		out, err := handleExecutorExecute(rawReq, call)
		if err != nil {
			t.Fatalf("execute error = %v", err)
		}
		var resp pluginapi.ExecutorResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ns := m.snapNonStream()
		if ns.Model != "claude-3-5-sonnet-20241022" {
			t.Fatalf("upstream model=%q", ns.Model)
		}
		var bodyDoc map[string]any
		if err := json.Unmarshal(ns.Body, &bodyDoc); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		if bodyDoc["model"] != "claude-3-5-sonnet-20241022" {
			t.Fatalf("upstream body model=%v", bodyDoc["model"])
		}
		if ns.EntryProtocol != "openai" || ns.ExitProtocol != "openai" {
			t.Fatalf("protocols=%q -> %q", ns.EntryProtocol, ns.ExitProtocol)
		}
		if ns.Stream {
			t.Fatal("non-stream request must have stream=false")
		}
		if !strings.Contains(string(resp.Payload), "claude-3-5-sonnet-20241022") {
			t.Fatalf("response payload=%s", resp.Payload)
		}
	})

	t.Run("TC-01 pure text keeps original model", func(t *testing.T) {
		m := &mockHost{}
		call := newMockCaller(m)
		req := executorRPCRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				SourceFormat:    "openai",
				Format:          "openai",
				Model:           "gpt-4o-mini",
				OriginalRequest: []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`),
			},
			HostCallbackID: "cb-1",
		}
		rawReq, _ := json.Marshal(req)
		if _, err := handleExecutorExecute(rawReq, call); err != nil {
			t.Fatalf("execute error = %v", err)
		}
		if ns := m.snapNonStream(); ns.Model != "gpt-4o-mini" {
			t.Fatalf("pure text upstream model=%q, want original", ns.Model)
		}
	})

	t.Run("TC-03 historical image in round 1 triggers rewrite", func(t *testing.T) {
		history := `{"model":"deepseek-v3","messages":[
			{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]},
			{"role":"assistant","content":"ok"},
			{"role":"user","content":"what was the image?"}
		]}`
		m := &mockHost{}
		call := newMockCaller(m)
		req := executorRPCRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				SourceFormat:    "openai",
				Format:          "openai",
				Model:           "deepseek-v3",
				OriginalRequest: []byte(history),
			},
			HostCallbackID: "cb-1",
		}
		rawReq, _ := json.Marshal(req)
		if _, err := handleExecutorExecute(rawReq, call); err != nil {
			t.Fatalf("execute error = %v", err)
		}
		if ns := m.snapNonStream(); ns.Model != "claude-3-5-sonnet-20241022" {
			t.Fatalf("historical image upstream model=%q", ns.Model)
		}
	})

	t.Run("TC-05 non-JSON body passes through", func(t *testing.T) {
		m := &mockHost{}
		call := newMockCaller(m)
		req := executorRPCRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				SourceFormat:    "openai",
				Format:          "openai",
				Model:           "gpt-4o-mini",
				OriginalRequest: []byte(`not json`),
			},
			HostCallbackID: "cb-1",
		}
		rawReq, _ := json.Marshal(req)
		if _, err := handleExecutorExecute(rawReq, call); err != nil {
			t.Fatalf("execute error = %v", err)
		}
		ns := m.snapNonStream()
		if ns.Model != "gpt-4o-mini" {
			t.Fatalf("non-JSON upstream model=%q, want original", ns.Model)
		}
		if string(ns.Body) != "not json" {
			t.Fatalf("non-JSON body=%q, want byte-for-byte pass-through", ns.Body)
		}
	})
}

func TestExecutorStreamTransparentPipe(t *testing.T) {
	setLoadedConfigForTest(testConfig())
	defer setLoadedConfigForTest(defaultConfig())

	multimodalBody := `{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}],"stream":true}`
	chunks := []string{
		"data: {\"id\":\"x\",\"model\":\"claude-3-5-sonnet-20241022\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	m := &mockHost{streamChunks: chunks}
	call := newMockCaller(m)
	req := executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			SourceFormat:    "openai",
			Format:          "openai",
			Model:           "gpt-3.5-turbo",
			OriginalRequest: []byte(multimodalBody),
		},
		StreamID:       "plugin-stream-1",
		HostCallbackID: "cb-1",
	}
	// runStreamForward is the forwarding core; call it synchronously so the
	// assertions observe deterministic state. The goroutine wrapping is
	// exercised separately below.
	if err := runStreamForward(req, testConfig(), call); err != nil {
		t.Fatalf("runStreamForward error = %v", err)
	}
	ss := m.snapStream()
	if ss.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("stream upstream model=%q", ss.Model)
	}
	emitted := m.snapEmitted()
	if len(emitted) != len(chunks) {
		t.Fatalf("emitted %d chunks, want %d: %#v", len(emitted), len(chunks), emitted)
	}
	for i, want := range chunks {
		if emitted[i] != want {
			t.Fatalf("chunk %d changed:\n got %q\nwant %q", i, emitted[i], want)
		}
	}
}

func TestExecutorExecuteStreamSmoke(t *testing.T) {
	setLoadedConfigForTest(testConfig())
	defer setLoadedConfigForTest(defaultConfig())

	multimodalBody := `{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}],"stream":true}`
	m := &mockHost{streamChunks: []string{"data: [DONE]\n\n"}}
	call := newMockCaller(m)
	req := executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			SourceFormat:    "openai",
			Format:          "openai",
			Model:           "gpt-3.5-turbo",
			OriginalRequest: []byte(multimodalBody),
		},
		StreamID:       "plugin-stream-1",
		HostCallbackID: "cb-1",
	}
	rawReq, _ := json.Marshal(req)
	out, err := handleExecutorExecuteStream(rawReq, call)
	if err != nil {
		t.Fatalf("execute_stream error = %v", err)
	}
	var init struct {
		Headers map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(out, &init); err != nil {
		t.Fatalf("unmarshal init: %v", err)
	}
	if init.Headers["Content-Type"][0] != "text/event-stream" {
		t.Fatalf("init headers=%v", init.Headers)
	}
	// The forwarding goroutine runs asynchronously; give it a bounded wait and
	// assert the stream reached [DONE] and the upstream model was rewritten.
	emitted := m.snapEmitted()
	for i := 0; i < 100000 && len(emitted) < 1; i++ {
		runtime.Gosched()
		emitted = m.snapEmitted()
	}
	ss := m.snapStream()
	if ss.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("stream upstream model=%q", ss.Model)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted %d chunks, want 1", len(emitted))
	}
}

// ---------------------------------------------------------------------------
// Lifecycle config parsing
// ---------------------------------------------------------------------------

func TestDecodeLifecycleConfigYAML(t *testing.T) {
	rawYAML := []byte("enabled: true\nmultimodal_model: claude-3-5-sonnet-20241022\ntext_models: deepseek-v3,gpt-3.5-turbo\nlog_decision: true\n")
	rawReq, err := json.Marshal(map[string]string{"config_yaml": base64.StdEncoding.EncodeToString(rawYAML)})
	if err != nil {
		t.Fatalf("marshal lifecycle: %v", err)
	}
	cfgRaw, isYAML, err := decodeLifecycleConfig(rawReq)
	if err != nil {
		t.Fatalf("decodeLifecycleConfig error = %v", err)
	}
	if !isYAML {
		t.Fatal("expected yaml path")
	}
	cfg, err := decodeConfig(cfgRaw)
	if err != nil {
		t.Fatalf("decodeConfig error = %v", err)
	}
	if !cfg.Enabled || cfg.MultimodalModel != "claude-3-5-sonnet-20241022" || !cfg.LogDecision {
		t.Fatalf("yaml config=%#v", cfg)
	}
	if !reflect.DeepEqual(cfg.textModels, []string{"deepseek-v3", "gpt-3.5-turbo"}) {
		t.Fatalf("yaml text_models parsed=%v", cfg.textModels)
	}
}

func TestDecodeLifecycleConfigYAMLArray(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "block list",
			yaml: "enabled: true\nmultimodal_model: claude-3-5-sonnet-20241022\ntext_models:\n  - deepseek-v3\n  - gpt-3.5-turbo\nlog_decision: true\n",
			want: []string{"deepseek-v3", "gpt-3.5-turbo"},
		},
		{
			name: "block list single item",
			yaml: "text_models:\n  - deepseek-v3\n",
			want: []string{"deepseek-v3"},
		},
		{
			name: "flow array quoted",
			yaml: "text_models: [\"deepseek-v3\", \"gpt-3.5-turbo\"]\n",
			want: []string{"deepseek-v3", "gpt-3.5-turbo"},
		},
		{
			name: "flow array bare",
			yaml: "text_models: [deepseek-v3, gpt-3.5-turbo]\n",
			want: []string{"deepseek-v3", "gpt-3.5-turbo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawReq, err := json.Marshal(map[string]string{"config_yaml": base64.StdEncoding.EncodeToString([]byte(tt.yaml))})
			if err != nil {
				t.Fatalf("marshal lifecycle: %v", err)
			}
			cfgRaw, isYAML, err := decodeLifecycleConfig(rawReq)
			if err != nil {
				t.Fatalf("decodeLifecycleConfig error = %v", err)
			}
			if !isYAML {
				t.Fatal("expected yaml path")
			}
			cfg, err := decodeConfig(cfgRaw)
			if err != nil {
				t.Fatalf("decodeConfig error = %v", err)
			}
			if !reflect.DeepEqual(cfg.textModels, tt.want) {
				t.Fatalf("text_models parsed=%v, want %v", cfg.textModels, tt.want)
			}
		})
	}
}

func TestDecodeLifecycleConfigRawJSON(t *testing.T) {
	raw := []byte(`{"enabled":true,"multimodal_model":"claude-3-5-sonnet"}`)
	cfgRaw, isYAML, err := decodeLifecycleConfig(raw)
	if err != nil {
		t.Fatalf("decodeLifecycleConfig error = %v", err)
	}
	if isYAML {
		t.Fatal("raw JSON should not use yaml path")
	}
	cfg, err := decodeConfig(cfgRaw)
	if err != nil {
		t.Fatalf("decodeConfig error = %v", err)
	}
	if cfg.MultimodalModel != "claude-3-5-sonnet" {
		t.Fatalf("config=%#v", cfg)
	}
}

// ---------------------------------------------------------------------------
// Model route query preservation (TC-04: /v1/messages?beta=true)
// ---------------------------------------------------------------------------

func TestRoutePreservesQueryAndHeaders(t *testing.T) {
	setLoadedConfigForTest(testConfig())
	defer setLoadedConfigForTest(defaultConfig())

	claudeBody := `{"model":"claude-3-5-haiku","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`
	m := &mockHost{}
	call := newMockCaller(m)
	headers := http.Header{"Authorization": []string{"Bearer x"}, "anthropic-version": []string{"2023-06-01"}, "anthropic-beta": []string{"true"}}
	query := url.Values{"beta": []string{"true"}}
	req := executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			SourceFormat:    "claude",
			Format:          "claude",
			Model:           "claude-3-5-haiku",
			OriginalRequest: []byte(claudeBody),
			Headers:         headers,
			Query:           query,
		},
		HostCallbackID: "cb-1",
	}
	rawReq, _ := json.Marshal(req)
	if _, err := handleExecutorExecute(rawReq, call); err != nil {
		t.Fatalf("execute error = %v", err)
	}
	if m.nonStreamRequest.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("claude document upstream model=%q", m.nonStreamRequest.Model)
	}
	// Headers and query must be preserved verbatim. Note: json.Unmarshal into
	// http.Header keeps keys in wire (lowercase) form, while http.Header.Get
	// canonicalizes the lookup key and would miss them; compare case-insensitively.
	headerValue := func(h http.Header, key string) string {
		for k, v := range h {
			if strings.EqualFold(k, key) {
				if len(v) > 0 {
					return v[0]
				}
			}
		}
		return ""
	}
	if got := headerValue(m.nonStreamRequest.Headers, "anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version=%q", got)
	}
	if got := headerValue(m.nonStreamRequest.Headers, "anthropic-beta"); got != "true" {
		t.Fatalf("anthropic-beta=%q", got)
	}
	if got := headerValue(m.nonStreamRequest.Headers, "Authorization"); got != "Bearer x" {
		t.Fatalf("authorization=%q", got)
	}
	if got := m.nonStreamRequest.Query.Get("beta"); got != "true" {
		t.Fatalf("query=%v", m.nonStreamRequest.Query)
	}
}
