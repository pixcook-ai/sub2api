# Tibo 探测迁移与可选自动处置

基线：28842f406469c0b51a9737d085935ee3fb978b70。来源：本机 sub2api-czc；目标：KlN-4096-sub2api。用户已确认目标并授权完整迁移 Tibo，两个目的分组必须可选。

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
- 本机 Go 缓存迁移进行中：暂不运行 Go build/test/run/install/gopls 或清缓存，收到解除通知后验证。
- 独立集成工作树，保留原主工作区及其他任务的未提交改动。
