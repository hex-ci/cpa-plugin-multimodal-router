# CPA Multimodal Router Plugin

多模态感知动态模型路由组件 —— 一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) 原生插件。

当请求的对话历史中含有**多模态内容**（图片、音频、文档）时，自动将请求的 `model` 重定向到配置的**多模态目标模型**；纯文本请求则保持客户端原始 `model` 原样透传。这样既避免纯文本请求被迫使用昂贵的多模态模型，也避免多模态内容被发给纯文本模型而报 `HTTP 400`。

## 工作原理

插件同时实现 CPA 的 **ModelRouter** 与 **Executor** 两个能力：

```
[客户端请求] → CPA model.route
                    │
                    ├── 仅白名单文本模型 + 含多模态块 ──► TargetKind: self
                    │        └─► Executor 改写 body.model = multimodal_model
                    │             └─► host.model.execute(_stream) 透传转发
                    │
                    └── 其他 ──► Handled: false（原样透传，model 不变）
```

- **深度全历史扫描**：遍历 `messages` 数组中所有历史消息（不仅是最后一条），只要任一条含多模态块即命中。因为即便最新提问是纯文本，上游非多模态模型也无法处理历史中的图片上下文。
- **只改 `model` 字段**：请求头、URL query、其余 body 全部原样保留，满足 Header 透传与 Body 长度重算要求。
- **SSE 透明管道**：流式请求经 `host.model.execute_stream → stream_read → stream.emit` 逐块直通，不缓冲、不截断。
- **Fail-safe**：非 JSON / 无法解析的请求原样透传，绝不崩溃。

### 检测规则

| 协议 | 判定为多模态的特征（任一命中） |
| --- | --- |
| **OpenAI** | ① `content` 数组含 `type:"image_url"` 块 ② 含 `type:"input_audio"` 块 ③ 消息块直接含 `image_url` / `input_audio` 属性 |
| **Claude** | ① `content` 数组含 `type:"image"` 块 ② 含 `type:"document"` 块 ③ 含 `type:"image_url"` 块 ④ 消息块含 `source` 属性对象 |

## 配置

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    multimodal-router:
      enabled: true
      multimodal_model: "claude-3-5-sonnet-20241022"
      text_models:
        - deepseek-v3
        - gpt-3.5-turbo
        - gpt-4o-mini
      log_decision: true
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 插件总开关，默认 `true` |
| `multimodal_model` | string | 多模态目标模型。命中时请求 `model` 被改写为此值 |
| `text_models` | array | **文本模型白名单**（字符串数组，支持 YAML 块列表/flow 数组与 JSON 数组，兼容旧的逗号分隔字符串）。**只有名单内的模型**才会被检测是否含多模态；名单外的模型视为本身支持多模态，一律透传。**空列表表示插件完全不接管任何请求**（检测与改写全部禁用） |
| `log_decision` | bool | 每次路由决策通过 `host.log` 记录，默认 `true` |

### 行为语义

- **名单内模型 + 多模态** → 改写 `model` 为目标模型并转发。
- **名单内模型 + 纯文本** → 保持原 `model` 透传。
- **名单外模型** → 无论是否含多模态，一律透传（视为原生支持多模态）。
- **空 `text_models`** → 不接管任何请求（需要显式配置白名单才能生效）。

日志示例：

```text
[OpenAI] 多模态: 是 | 原始模型: deepseek-v3 -> 最终模型: claude-3-5-sonnet-20241022
[Claude] 多模态: 否 | 原始模型: claude-haiku-4.5 -> 最终模型: claude-haiku-4.5
```

## 构建

需要 Go 1.26+（SDK 依赖要求；本地 Go 较低版本可用 `GOTOOLCHAIN=auto` 自动下载）。

```bash
make build
# 产物: dist/linux_amd64/multimodal-router.so
```

插件为 cgo C 共享库（`-buildmode=c-shared`），实现 CPA 插件 ABI。也支持其他平台：

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -buildmode=c-shared -o dist/linux_arm64/multimodal-router.so .
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 go build -buildmode=c-shared -o dist/windows_amd64/multimodal-router.dll .
```

## 安装

1. 将 `multimodal-router.so`（Windows 下 `.dll`）放入 CPA 配置的 `plugins.dir` 目录。
2. 在 `config.yaml` 的 `plugins.configs.multimodal-router` 下配置字段（见上）。
3. 确认 `plugins.enabled: true`，重启 CPA。
4. **注意**：插件 executor 依赖主机内置 provider 的 auth 记录。需在 CPA 中为 `multimodal_model` 对应的 provider 配置好凭据，否则上游返回 4xx 时错误会透传。

## 测试与验收

```bash
make test   # go test ./...
make vet    # go vet ./...
```

测试用例：

| 场景 | 预期 | 测试 |
| --- | --- | --- |
| OpenAI 纯文本 | `model` 保持 `gpt-4o-mini` | `TestExecutorNonStreamTC` |
| OpenAI 新发图片 | `model` 改写为目标模型 | `TestExecutorNonStreamTC` |
| OpenAI 历史带图追问 | 全历史扫描命中，`model` 改写 | `TestExecutorNonStreamTC` |
| Claude 文档 + `/v1/messages?beta=true` | 保持 URL 参数，`model` 改写 | `TestRoutePreservesQueryAndHeaders` |
| 非 JSON 请求 | 原样透传 | `TestExecutorNonStreamTC` |

另覆盖：检测矩阵（`TestDetectMultimodal`）、`text_models` 白名单（`TestRouteModelTextModelsList`）、
配置解析（`TestDecodeConfig*`）、日志（`TestLogRoute*`）、SSE 透明管道（`TestExecutorStreamTransparentPipe`）、fail-safe 透传（`TestRewriteTopLevelModelNonJSON`）。

## 限制

- 插件为**原生动态库**，属 CPA 的 trusted in-process 代码。
- 检测基于 JSON body 解析，超大体量（如 50MB+ Base64）在内存中处理；超出合理范围时应在上游限制请求体大小。
- `executor.count_tokens` 未实现（返回 `unsupported`）。

## License

MIT
