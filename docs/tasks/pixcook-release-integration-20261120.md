# Pixcook 上游同步与自有镜像发布

用户确认方案后授权实现；发布集成固定审查基线 `30282b32f`。集成后续 BPS `b9025b3d3`，merge `48bb3b5`，保留 Tibo 与 Files/Responses 实时授权。原主工作区无改动。

## 交付契约

1. 长期 `pixcook` 集成分支与本地上游跟踪分离。原始上游提交不推成本仓分支，避免携带的上游 Actions 被触发；通过候选 merge parent、baseline JSON 和报告保留精确 SHA。同步 KlN 稳定发行提交，生成候选 PR，不重写共享分支、不重置 main、不自动部署或发布。
2. 保留本仓自有 CI/同步/发布工作流；冲突停止并保留诊断。上游输入及分支必须严格校验，不把输入直接拼入 shell。
3. Release 从固定源码提交构建自有 GHCR 镜像、来源与上游信息、版本/commit标签和可部署digest；支持不发布的dry-run。发布不回写main，不依赖latest生产升级。
4. 镜像托管模式阻止应用内二进制升级和回滚，避免覆盖自有功能；普通二进制模式保持原行为。
5. 独立部署模板必须固定镜像digest、本机绑定、持久化配置/文件及数据库；现有实例只替换应用镜像，不自动升级PostgreSQL主版本或重建卷。
6. 部署前备份、测试环境小范围验证、灰度开Tibo、质量隔离兼容回滚。不得从健康接口推断真实账号检测/调度链路已验收。

## 验证边界

- 同步：真实临时Git仓测试候选merge、重复执行、冲突和保护工作流；校验稳定release/tag SHA及PR行为。
- 发布：现有release工具测试+自有来源/镜像标签/digest与dry-run回归；workflow语法和权限检查。
- 后端：完整unit，镜像模式禁更改回归，Tibo及BPS组合定向PG/race。
- 前端：合入后受影响测试及完整build；不因无前端源码差异扩大浏览器声明。
- 部署：Compose实际解析、digest拒绝/升级与回滚指令检查；本地隔离启动候选并验证版本/登录/Tibo API。生产切换另给具体镜像、备份与回滚方案。

## 上游核对

2026-11-20实时GitHub API：KlN最新稳定发行 `v0.2.23-klno.1`，发布于2026-11-18T12:16:09Z。已实际fetch该tag并确认它是当前候选祖先，无额外上游代码待合入。不要由仓内VERSION字面值推断实际发行来源；来源记录固定SHA。


## 远端启动记录

- 2026-11-20 已将 `1602474253873eab7cd256f581b0307fdc7100b7` 普通推送到新的 `pixcook` 分支，并将本仓默认分支改为 `pixcook`；旧main历史未重写。
- Release dry-run `44737735020` 已启动：prepare、前端构建和release-contracts已通过；剩余后端和镜像构建仍运行。这不是发布成功证据。
- GitHub Secrets仍为空，专用 `SYNC_GITHUB_TOKEN` 未配置；同步会在任何写入前报错停止。未上传个人CLI凭证。
- 合并后的unit发现StoredResponses两处顶层测试的旧夹具缺少当前GroupRepository，实时授权按预期fail closed；修复只补测试依赖，保留生产围栏。修复后的handler、Tibo/BPS race/PG仍待最终结果。
- 独立双轴审查任务被网关反复省略，代理未执行，本轮不得宣称独立审查通过；主代理已核对来源、工作流保留、原始上游分支触发风险与部署覆盖契约。

- 最终修复提交：`2f833bb33`补实时分组授权夹具，`25b1e8736`升级x/image0.45.0（准确SHA以git log为准），`3425840`清理集成lint，`a0b35fa96`将漏洞扫描纳入候选发布门禁。第二轮dry-run44742048802运行中；第一轮44737735020因backend测试失败而阻止release，未推镜像。

- 安全修复版govulncheck远端通过（Security Scan44741995862，candidate security44742048802）。lint清理一度误改httptest.Server.Close返回值，已按实际签名修复；该错误是主代理改动，不能归为旧问题。最终service与lint重验排队中。

- 最终候选源码 `e1c1c3c03f65e1490a4717cccb00efcfa6f88833`。安全扫描44743321853通过；含lint门禁的dry-run44743498402排队/运行中。旧dry-run因测试或编译失败阻止发布，未推送镜像。

- service完整unit24073pass/29skip，其他包此前最终重验9478pass/31skip；不能将跨运行加总冒充单次全量通过。最后errcheck补丁仅清理/接口断言/错误文案，最终race/PG及lint仍运行。最新源码83438519c，完整门禁dry-run44745022519；本轮未推发布tag、未发布镜像。

- 最终扩大race：899pass/1skip，service/repository均通过；对应日志/tmp/pixcook-final-1-20261120.jsonl。最终PG仍执行。

- 最终真实PG88pass，覆盖repository及server/middleware，日志/tmp/pixcook-final-2-20261120.jsonl；扩大race899pass/1skip。源码功能回归通过；本地lint和远程完整候选仍待结果。

- 最新候选4e66de713的完整门禁dry-run44747770130执行中；此前失败lint均保留为失败，没有关闭门禁或标记跳过。日志完整检查取消默认告警截断，受影响测试排队。

- 完整lint输出最后68项主要是测试夹具断言，已引入仅测试使用的checkedFixtureValue并补生产读取断言，提交d87489119；最新dry-run44750425633执行中，未完成验证不得发布。
