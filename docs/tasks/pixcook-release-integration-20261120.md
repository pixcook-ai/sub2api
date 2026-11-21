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


## 最终验收与交付状态

- 固定源码：`5cc87fa8b429053fd6e898554dc4305926ea2ae5`，已推送 `pixcook`，默认分支为 `pixcook`。原main历史未重写。
- 远程 Release dry-run **44757345587 全部通过**：完整unit、真实integration、质量/调度race、lint、安全扫描、前端测试与build、来源/发布/同步/部署契约、Linux amd64二进制及OCI镜像构建。DRY_RUN=true，无镜像或GitHub Release发布。
- 产物：release-dry-run-report、release-source、release-binary-linux-amd64；已核对artifact未过期。
- 本地完整integration：19906pass/35skip、59包pass、无失败，日志 `/tmp/pixcook-full-integration-20261120.jsonl`。
- 本地受影响最终测试：1410pass/1skip；扩大race899pass/1skip；真实PG定向88pass；完整lint0 issues。跨次验证不冒充单次全量。
- 修复真实慢上传HTTP超时400→408竞争；实际HTTP/PG回归通过。分组测试按分页上限与数据库基线检查数量，保留总数断言。
- WebP可达漏洞GO-2026-6222/5061/4961已以x/image0.45.0修复，最终远端govulncheck通过。lint清理过程曾误改httptest.Close签名，已修复并通过最终门禁；失败运行保留。
- 独立双轴审查被网关省略，代理未执行，不声明其通过；主代理完成来源、工作流保留、更新保护、既有部署与回滚边界检查。
- 专用 `SYNC_GITHUB_TOKEN` 仍缺失，定时同步在任何写入前停止；未上传个人CLI凭证。真实自动PR行为尚未验证。
- 生产尚未切换；真实上游账号灰度、生产备份恢复、固定镜像digest部署验收仍待单独执行。旧本地18080服务仍为原测试构建，不自动替换。
- 代码索引服务因外置盘迁移暂停调用，未重启服务。

## 正式发布结果

- `v0.2.23-pixcook.1` 发布流水线44771988597全部通过，同一源码5cc87fa8b，平台Linux amd64。GitHub Release为预发布版。
- 发布产物记录的镜像：`ghcr.io/pixcook-ai/sub2api@sha256:52d67167e517297b401cedf57b8960e5425c2f16d6392caabe0e95f357db9ee5`。来源和manifest已保存到docs/verification/pixcook-release-20261121.json。
- 匿名GHCR读取返回401，当前CLI凭证无read:packages，临时私有Docker配置认证后仍无法读manifest；未保存/上传个人凭证。需具有read:packages的部署凭证或公开包可见性后才能验收服务器拉取。发布artifact来源已验证，独立拉取未通过。
- 专用SYNC_GITHUB_TOKEN仍缺失，自动同步尚未启用；测试环境和生产未切换，原18080本地测试服务未更新。

## 2026-11-21 — 镜像拉取与自动同步配置完成

- 自有 GHCR sub2api 包由 private 改为 public；组织 Public package creation 仅在变更期间临时开启，完成后恢复禁用。匿名 OCI manifest GET 返回200，内容SHA256与已发布digest一致，linux/amd64子manifest与发布artifact一致。未下载镜像层或在服务器拉取。
- 新建本仓专用 fine-grained PAT，保存为 Actions Secret `SYNC_GITHUB_TOKEN`；仅授权 pixcook-ai/sub2api 的 Contents、Pull requests、Workflows写入和必需Metadata读取，无组织权限。有效期至2027-02-19，届时需轮换；没有复制个人CLI token或在仓库记录令牌。
- GitHub fork停用门阻止首次dispatch（422），仓库permissions和workflow.state虽显示enabled/active，网页要求重新启用。恢复Actions后真实运行44854017794全部成功；artifact sync-report.json为up_to_date，上游v0.2.23-klno.1 / 249125a7737999da2fbe561718b76aaaf189b753已在pixcook中。
- 定时同步已配置为每日03:17 UTC（北京时间11:17）；本轮是手动dispatch验收，不宣称定时触发已实际发生。无新增上游变更，未创建候选PR，真实分支写入及PR CI触发仍未在本轮执行。
- 测试环境、生产及旧localhost:18080实例均未切换。此前凭证缺失和匿名401为历史状态，由本节与更新后的verification JSON替代。
