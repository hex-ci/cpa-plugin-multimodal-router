# CLAUDE.md

本文件为 Claude Code 在此仓库工作时提供指引。

## 项目是什么

`multimodal-router` 是一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) 原生插件（Go cgo C 共享库）。
它同时实现 CPA 的 **ModelRouter** 与 **Executor** 两个能力：深度扫描请求 `messages` 全历史，
检测多模态内容（图片/音频/文档）；命中时把请求 `model` 改写为配置的多模态目标模型并经 host 转发，
纯文本/未命中时 `Handled:false` 原样透传。

## 常用命令

```bash
# 环境：SDK 要求 Go 1.26+。本地 Go 较低时用 GOTOOLCHAIN=auto 自动下载工具链。
# 本机 proxy.golang.org 不稳定，统一用 GOPROXY=direct。

# 测试（普通 / race / 单测列表）
GOPROXY=direct GOTOOLCHAIN=auto go test ./...
GOPROXY=direct GOTOOLCHAIN=auto go test -race ./...
GOPROXY=direct GOTOOLCHAIN=auto go test -v ./...

# 静态检查 + 构建插件
GOPROXY=direct GOTOOLCHAIN=auto go vet ./...
GOPROXY=direct GOTOOLCHAIN=auto make build   # 产物 dist/linux_amd64/multimodal-router.so
```

## 架构

```
客户端请求 → CPA model.route
    ├── 名单内模型 + 含多模态块 ──► TargetKind: self ──► Executor: 改写 body.model ──► host.model.execute(_stream)
    └── 其他 ──► Handled: false（原样透传）
```

### 文件职责

- `main.go` — 全部插件逻辑：配置、注册、检测、路由、执行、流式转发、host 回调、方法分发。
- `abi_cgo.go` — cgo C ABI 桥接层（`cliproxy_plugin_init` / `PluginCall` / `PluginFree` / `PluginShutdown`）。`//go:build cgo`。
- `main_test.go` — 单测 + 端到端（mock host 模拟完整 RPC 流程）。
- `Makefile` — `build` / `test` / `vet` / `clean`。
- `README.md` — 安装、配置、验收对照。

### 核心数据流

1. `routeModel` 决定是否接管（model.route）。只处理：enabled + `shouldInspect`（名单内）+ 含多模态 + 目标模型非空 + 原模型≠目标。
2. `handleExecutorExecute` / `runStreamForward` 调 `prepareUpstream(req, cfg)` 计算上游 model/body——**幂等**，body 已等于目标则不改。
3. 非流式：`host.model.execute`；流式：`host.model.execute_stream` → `stream_read` 循环 → `stream.emit` 直通 → 关闭。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    multimodal-router:
      enabled: true
      multimodal_model: "claude-3-5-sonnet-20241022"
      text_models:
        - deepseek-v3
        - gpt-3.5-turbo   # 也兼容 JSON 数组 / 旧逗号分隔字符串
      log_decision: true
```

- `text_models`：**只有名单内模型**才会被检测多模态；名单外一律透传（视为原生支持多模态）。
- **空列表 = 全部不检测**（插件不接管任何请求）。
- `log_decision`：每次路由决策经 `host.log` 记录 `[OpenAI/Claude] 多模态: 是/否 | 原始模型: X -> 最终模型: Y`。

## 检测矩阵

- **OpenAI**：`content` 数组含 `type:"image_url"` / `type:"input_audio"` 块；或消息块直接含 `image_url`/`input_audio` 属性。
- **Claude**：`content` 数组含 `type:"image"` / `type:"document"` / `type:"image_url"` 块；或消息块含 `source` 属性。
- 深度遍历**所有**历史消息，不只最后一条。

## 关键设计决策（勿轻易改动）

1. **`text_models` 空列表 = 不检测**。
   实现于 `shouldInspect`（空列表返回 false）。
2. **只改 `body.model`，绝不碰 Headers/Query/其余字段** —— 满足透传要求。
3. **流式透明管道** —— `runStreamForward` 逐块 `stream.emit`，不缓冲不改 chunk，保打字机延迟。
4. **fail-safe** —— 任何 JSON 解析失败返回 `Handled:false` / 原样透传，绝不崩溃。
5. **流式 goroutine 不读全局配置** —— `startExecutorStream` 在 spawn 前快照 `cfg` 传入，
   避免转发循环与 `reconfigure` 并发读 `loadedConfig()`（曾触发 race，已修）。
6. **`logRoute` 用调用方传入的 cfg 判断 `LogDecision`**，而非读全局 —— 决策与日志不脱节。
7. `executor.count_tokens` 返回 `unsupported`（与参考插件一致）。

## 代码约定

- 与 CPA SDK 模式对齐，不重新发明轮子。
- `Config` 内 `textModels` 是规范化小写列表（`parseTextModels`）；`TextModelsRaw` 是规范化后的逗号分隔字符串。
  配置入口 `text_models` 以**字符串数组**为主（YAML 块列表 / flow 数组 / JSON 数组），`normalizeTextModelsScalar` 兼容旧逗号分隔形式。
- host 回调经 `callHost`（包装 `pluginabi.Envelope`）。
- 所有 JSON 解析失败路径都要 fail-safe 透传，不得向上抛致命错误。

## 测试注意

- 测试通过 `setLoadedConfigForTest` 切换全局配置；测试间会污染，注意 `defer setLoadedConfigForTest(defaultConfig())`。
- mock host（`mockHost`）字段读写跨 goroutine（流式转发是异步的），已加 `sync.Mutex` 保护；
  断言用 `snapNonStream()` / `snapStream()` / `snapEmitted()` 快照方法，不要直接读字段。
- 流式核心 `runStreamForward` 同步测（`TestExecutorStreamTransparentPipe`）；goroutine 包装单测走
  `TestExecutorExecuteStreamSmoke`（轮询 `snapEmitted` + `runtime.Gosched`）。
- 用 `go test -race` 验证并发正确性。

## 已知限制

- 插件为 CPA trusted in-process 动态库。
- executor 依赖主机内置 provider 的 auth 记录（上游 4xx 会透传）。
- 大体积请求（≥50MB Base64）在内存处理；过大时应在上游限制 body。
