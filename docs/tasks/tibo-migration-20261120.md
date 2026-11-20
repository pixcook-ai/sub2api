# Tibo 探测迁移与可选自动处置

初始基线：28842f406469c0b51a9737d085935ee3fb978b70。后合入目标仓已提交的 09def4827（基座文件授权/存储生命周期依赖），最终 Tibo diff 与评审固定相对此基线。来源：本机 sub2api-czc；目标：KlN-4096-sub2api。用户已确认目标并授权完整迁移 Tibo，两个目的分组必须可选。

## 范围与接口
- 迁移定时账号定向 HTTP 探测、投票/时效/并发预算、24 点历史、持久化、健康优先选号与管理展示。保留目标 Codex 身份、质量防护、BPS/WS 协议。
- 不迁移源仓与检测无关的注册双 Key、硬编码组名模型限制或整套智能测试平台。
- 管理 GET/PUT /api/v1/admin/tibo/settings，JSON 字段 enabled、auto_group_enabled、healthy_group_id、degraded_group_id、pause_on_degraded、auto_resume_on_healthy。默认 enabled=false、auto_group_enabled=false、pause_on_degraded=true、auto_resume_on_healthy=false、group IDs=0。开启探测与选择自动动作需要管理员显式保存。
- 分组按 ID 选择，不自动建组，不按名称识别。自动迁组开启时两组须为不同、有效的 OpenAI 组；关闭时可不选组。保持其他分组与优先级/创建时间。
- service.TiboAutomationSettings 与 SettingService.GetOpenAITiboSettings(ctx) (TiboAutomationSettings,error)、UpdateOpenAITiboSettings(ctx,settings) error 为共享契约。读取有短缓存；写入校验并使缓存失效。
- 自动处置只由新鲜、明确的主动探测样本触发，不由历史载入、unknown、HTTP 错误、管理设置保存触发。迁组沿用源方案的单次明确样本语义，路由仍投票确认。
- pause_on_degraded 设置独立 extra 标记 openai_tibo_quality_isolated=true，不翻转管理员 Schedulable 开关。auto_resume_on_healthy 只能清除该质量标记；绝不恢复手动暂停。
- POST /api/v1/admin/accounts/:id/tibo/clear-isolation 手动清除该标记，仍不修改 Schedulable。
- 隔离需覆盖一般选号、sticky、previous_response_id 和最终转发准入；恢复探测不因质量标记而停止。

## 验收边界（复用源仓既有测试边界）
1. 探测 HTTP：目标账号/代理/模型参数、完成/失败/答案分类、定时预算与暂停账号行为。
2. Repository：真实 PostgreSQL 事务、乱序样本、可选组/改名/禁用/删除、保留其他组、手动暂停/并发与调度快照。
3. 管理 API/前端：设置保存与校验、可选分组选择、红绿点与质量隔离状态/清除。
4. 调度/转发：健康排序、不回退已隔离账号，sticky/previous-response 与并行路径不绕过。

## 执行记录
- Go 缓存迁移暂停已经解除；使用移动盘上的 Go 缓存，由主代理统一占用单一大型构建槽。
- 独立集成工作树，保留原主工作区及其他任务的未提交改动。
- 已按来源清单迁入探测、24 点历史、持久化/缓存、调度/发送、管理配置与 usage 三态链；分组按管理员选定 ID，不硬编码源仓两个组名。使用说明见 `docs/tibo-quality-probing.md`。
- 新鲜观察、自动动作、迁组与 outbox 同事务；手动清除 fence 防在飞旧样本重新隔离；credentials/proxy 身份摘要拒绝旧身份结果。
- 前端 10 个相关测试文件 251/251 通过（零跳过）；vue-tsc 与 Vite 完整构建通过，8 个相关生产文件 ESLint 通过。报告 `/tmp/tibo-frontend-tests-20261120.json`。
- 首轮后台全套 unit 暴露两项本迁移回归：billing update SQL mock 未预期运行态保护读取；旧 AlphaSearch mock 的嵌入 GetByID 被全对象权威门调用。前者已修夹具，后者以最终发送的窄质量 SQL 门替代完整对象读取，不靠扩大 mock 掩盖。首轮结果不作为完整通过证据。
- PostgreSQL 首次因 OrbStack 停止而失败（CI=1，未静默跳过），启动本机运行时后，第一轮隔离测试容器真实迁移/事务/用量测试 34 项通过、零失败/跳过；后续新增发送门与身份修复须统一复验。
- 独立 Spec 发现恢复探针被质量门拒绝及旧身份污染内存两项；Standards 发现默认关闭仍重复完整 DB 读取。已分派修复并要求窄复核。最终发送门改为一次质量投影读取，关闭监控不绕过历史隔离；DB 读取失败仍拒绝发送。
- Ent 已真实生成；Wire 已真实重新生成且没有生成差异或 go.mod/go.sum 变化。未运行真实上游账号、浏览器联调或生产部署，不据本地测试声称这些范围通过。
- Spec/Standards 最终窄复核已关闭全部发现：恢复探针私有豁免、先提交后发布、身份不匹配读取为 unknown，以及被动观察发布不覆盖并发探测预算。主动/被动观察各有事务身份围栏；非 fresh 端口仅接收历史，不授予自动动作。
- 完整后台 unit 第一次终态运行：65 个包通过、52 个无测试包跳过，service 包出现既有流响应断言和 300 秒整包超时；已完成的 tests/subtests 为 30,975 PASS、60 SKIP、2 FAIL（失败父测试及其单个子测试）。这不是全套绿灯。日志 `/tmp/tibo-backend-unit-final-20261120.jsonl`。
- 在独立、干净的 09def4827 临时工作树实际复现 `TestOpenAIResponseFlush_FailedAndErrorEventsFlushAtBoundaries/failed_at_EOF` 同一失败：测试期待剔除 usage/output，而基线 `sanitizeOpenAIResponseFailedEventForClient` 已保留公开 usage 并补 output 数组；helper 绕过 Tibo 路径。证据 `/tmp/tibo-baseline-response-flush-20261120.jsonl`。验证后已回收该临时工作树；不为本迁移更改基线失败响应契约或放松断言。
- 最终 service 整包用 `-parallel=16 -timeout=900s` 完整跑完，316.726s，无超时：23,915 PASS、29 SKIP、4 FAIL（两个父测试及其对应子测试）。除上述基线断言外，`TestGatewayPoolRetryDeadlinePreventsLateBusinessDispatch/during_fetch=true` 曾在全包记录预热探针导致断言失败；该用例单独 20 轮共 60 个 tests/subtests 全通过，但这不能证明波动根因已消除，也未将它标为基线必现。日志 `/tmp/tibo-service-final-20261120.jsonl`、`/tmp/tibo-gwpool-deadline-repeat-20261120.jsonl`。
- 最终 Tibo 功能关键回归（隔离后真实模拟 HTTP 恢复探测、拒绝伪造豁免、旧身份不发布/不读取、并发预算及退款、shadow 续接 owner 保留）在完整 service 运行中均通过。未放宽基线失败用例，也不宣称完整后台全绿。
- 最终真实 PostgreSQL：`CI=1 go test -p=1 -tags=integration ./internal/repository -run 'TestTibo(AutoGroup|QualityIsolation|Observation)|TestUsageLog_RouteDegradedRoundTripFilterAndPartialIndex' -count=1 -timeout=300s -json`，39/39 tests/subtests 通过，零失败、零跳过；使用隔离 PG18/Redis 容器并实际应用所有迁移，非既有开发/生产数据库。日志 `/tmp/tibo-postgres-final-20261120.jsonl`。新 narrow-read 夹具先后发现 dimension 约束和真实 RESTRICT 错误码断言不符，均只修正夹具后完整重验通过，未修改基线 schema。
- 最终定向 `-race` 覆盖 service/repository 的 Tibo、RouteDegraded 与 ClassifyTibo：199/199 tests/subtests 通过，零失败、零跳过、零 DATA RACE，两包均通过（`/tmp/tibo-race-recheck-20261120.jsonl`）。此前竞态来自 `newTiboRouteCase` 在另一探针运行时重复调用全局 `gin.SetMode(TestMode)`；删除夹具中的该全局写入后同组重验转绿。Standards 已核对 Gin 源码，确认只影响调试输出，未更改生产逻辑或放松断言。

## 后续：修复完整 service 验收失败

用户要求继续处理两项失败，沿用现有流输出与 gwpool 业务截止测试边界，工作基线固定为 `829e710fe`。

- EOF 失败终态已在当前工作树重新复现：旧断言删除 usage/output，当前公开契约保留安全计数与空 output；与既有 `TestOpenAIForwardFailedStreamPreservesSafeClientTerminal` 一致。仅修正预期并加强两次 flush 完整字节及 output token 断言，不修改生产响应。
- 响应 flush、真实 Forward 安全终态及部分用量定向测试通过（service 6.887s），独立只读复核无发现。
- gwpool 受控共享场景已确定性 RED：阻塞取票，业务 waiter 到期返回 429 后放行仍存活的 follower，后者完成两次 state-echo；旧“全部发送为零”断言实际失败为 2（`/tmp/tibo-gwpool-shared-red-20261120.jsonl`）。这证明总发送不等于业务发送，未发现需要修改生产截止逻辑的证据。
- 原失败测试使用 atomic 区分业务/预热、明确业务正文并等待 worker 收尾；新增场景严格要求业务零发送、活 follower 两次预热完成、原 context 不受截止污染。仅改测试，未放宽业务发送约束。
- gwpool 同组 100 轮共 400 tests/subtests 通过，零失败/跳过（`/tmp/tibo-gwpool-repeat-green-20261120.jsonl`）。Spec/Standards 独立评审均无发现；最终 race/全包结果见下方。
- 最终受影响竞态组（gwpool 截止、response flush、真实 Forward 安全终态）5 轮共 155 tests/subtests 通过，零失败/跳过、零 DATA RACE（`/tmp/tibo-failures-race-green-20261120.jsonl`）。
- 最终完整后台 unit：`go test -p=1 -parallel=16 -tags=unit ./... -count=1 -timeout=900s -json` 退出0；66个测试包通过、52个无测试包，33,398 tests/subtests通过、60跳过、零失败/超时。service整包307.691s通过。跳过项包括需要外部服务/指定环境的集成或实时测试及既有显式跳过，不视为这些范围通过。日志 `/tmp/tibo-backend-after-failure-fixes-20261120.jsonl`。此前两项失败在该最终全套均通过；保留前次失败记录用于追溯。没有真实上游、浏览器、生产操作、推送或部署。
