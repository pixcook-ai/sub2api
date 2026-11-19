# BPS 请求策略拒绝

普通 OpenAI HTTP Forward 识别实际上游响应中的 `X-Bps-Request-Policy: rejected-v1`，仅用于禁止改写请求后重放。只有 HTTP 400、精确单值标记和以下完整标准错误形状共同成立才生效：

- `error.type` 是字符串 `invalid_request_error`。
- `error.code` 是字符串 `unsupported_parameter`、`unsupported_tool`、`unsupported_tool_choice` 或 `invalid_parameter`。
- `error.param` 是非空且不全为空白的字符串。

错误体的 type/code/param 保持原值。入站客户端头不作为证据；标记缺失、版本未知、重复值、JSON 无效或错误形状不匹配时，保留原兼容重试行为。无需通过 URL、账号名或测试开关推断 BPS 身份。

匹配后跳过同一个错误处理块中的请求恢复、参数修剪和 compact 模型兼容重放；正常的错误、健康、用量和账号处理保持执行。标记不是“生成未开始”的凭证，不改变计费，也不单独授权冷却、切换或重试。WS 与其他转发路径不在本补丁范围。

两仓契约对应 `pixcook-ai/bps-gateway` #3、父规格 #1；gateway 只在 `failPolicy(*sanitize.RequestError)` 返回该头，普通错误和上游失败不使用它。关闭该票前仍需真实主仓认证 HTTP → 调度 → gateway → 本地 BPS 的官方 Python/JavaScript SDK 验收。
