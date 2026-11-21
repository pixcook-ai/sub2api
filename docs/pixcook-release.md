# Pixcook 版本同步、发布与部署

## 分支与来源

长期 `pixcook` 保存基座、Tibo 与BPS集成；上游来源由候选合并父提交与 `.github/upstream-baseline.json` 固定到 KlN 稳定发行的精确 SHA。开发分支通过PR进入pixcook。不要将上游镜像或KlN二进制直接替换自有构建，也不要通过rebase/force-push重写共享集成历史。

同步工作流生成候选PR，保留本仓CI/发布工作流；冲突和测试失败需要处理后再合入。GitHub定时任务只从默认分支调度，初次启用必须让默认分支承载本仓安全工作流；不能只将文件推到另一个分支就宣称定时同步已启用。发布候选必须记录上游tag/commit、自有完整commit和镜像digest。

## 发布顺序

1. 核对稳定上游候选及自有补丁，合入并完成CI。
2. 固定源码提交，执行Release dry-run；真实构建通过后再发布自有GHCR镜像。
3. 测试环境和生产都固定同一个 `ghcr.io/pixcook-ai/sub2api@sha256:...`，不使用latest作为生产升级来源。部署设置 `SUB2API_UPDATE_MODE=image`，禁止应用内下载KlN二进制升级或回滚。
4. 测试环境使用独立数据库、Redis、文件卷与密钥。完成登录、权限、已有Files/Responses和Tibo回归后，再安排生产切换。

## 首次部署与已有实例升级

`deploy/pixcook/` 提供自有镜像部署配置和验证入口。新安装可使用独立Compose；已有实例只在当前Compose上叠加应用镜像覆盖配置，沿用现有数据库版本、网络、配置、密钥及挂载路径。不能直接用新Compose替换现有部署，否则可能接到空卷或误升级PostgreSQL主版本。

备份必须包含PostgreSQL一致性备份、应用配置与持久文件（含Files/Responses存储）、部署文件与固定密钥；密钥只保留在私有备份，不提交仓库。记录当前镜像digest、数据库版本和挂载路径，并实际验证备份可读取。切换前检查新旧数据库迁移与应用读写兼容性；数据库备份与物理文件需在协调停止写入后形成一致恢复点。

切换应用时保留旧镜像及配置。先预拉取固定digest，停止新请求并给已有流式请求合理排空时间，运行数据库迁移并启动新应用，检查版本、登录及真实业务链路，再恢复流量。健康接口通过不代表模型、工具、计费或Tibo调度通过。

## Tibo 灰度验收

初始保持探测总开关和自动迁组关闭。先用少量指定测试账号观察，确认探测代理/凭据/结果和用量；再启用异常停止调度，验证普通调度、sticky、previous-response owner及HTTP/WS准入均拒绝已隔离账号。恢复探针应继续，健康自动恢复只清除质量隔离，管理员手动暂停仍保持暂停。按需选择两个目标组后再开启迁组，核对无关分组与关联元数据保留。

单题Tibo仅为启发式信号，不能证明内部模型降级。初始启用产生真实上游用量；用真实账号灰度必须在指定测试环境操作，不能从本地模拟测试推断完成。

## 回滚边界

优先回滚到另一个仍识别Tibo质量隔离和现有Files/Responses schema的自有digest。若退回不识别 `openai_tibo_quality_isolated` 的旧版本，必须先停止业务流量并核对全部质量隔离账号：将这些账号转换为旧版识别的管理员暂停状态并读回，保留操作前快照；不要批量清除质量标记或顺带启用账号。旧版忽略标记时会重新参与调度。

关闭探测不会解除已有质量隔离。恢复旧数据库会丢失备份后的写入，必须单独决策；不能将换回旧镜像描述为完整数据回滚。新增usage列与索引不主动删除，确认旧代码兼容后再运行；未来迁移可能不兼容，逐版本审核。

## 本次范围

当前源码集成与验收记录在 `docs/tasks/pixcook-release-integration-20261120.md`。自有 v0.2.23-pixcook.1 已完成远程CI和镜像发布，公开GHCR manifest匿名读取已验证；真实上游账号灰度、镜像层/服务器拉取及生产切换尚未执行。

自动同步凭证 `SYNC_GITHUB_TOKEN` 已配置，仅授权本仓 Contents、Pull requests、Workflows写入及Metadata读取，有效期至2027-02-19，需到期前轮换。每日03:17 UTC（北京时间11:17）检查稳定上游；有变更时仅提出候选PR，不自动合并、发布或部署。首次真实手动运行44854017794成功，结果up_to_date；新候选分支/PR及其CI触发留待上游有变更时验证。


## 可执行入口

新安装先按 `deploy/pixcook/.env.example` 创建私有环境文件并填写已核验的镜像 digest，再执行：

```sh
rtk python3 deploy/pixcook/validate.py --mode fresh --env-file /private/site.env --compose-file deploy/pixcook/compose.yaml
rtk docker compose --env-file /private/site.env -f deploy/pixcook/compose.yaml pull sub2api
rtk docker compose --env-file /private/site.env -f deploy/pixcook/compose.yaml up -d
```

已有部署保留原工作目录与 project name，向原私有 env 增加 PIXCOOK_IMAGE 后，先验证叠加配置：

```sh
rtk python3 deploy/pixcook/validate.py --mode existing --env-file /site/.env --project-name ORIGINAL_PROJECT \
  --baseline-file /site/docker-compose.yml \
  --compose-file /site/docker-compose.yml --compose-file deploy/pixcook/compose.existing.yaml
```

备份和排空验收后，只替换应用：

```sh
rtk docker compose --env-file /site/.env --project-name ORIGINAL_PROJECT -f /site/docker-compose.yml -f deploy/pixcook/compose.existing.yaml pull sub2api
rtk docker compose --env-file /site/.env --project-name ORIGINAL_PROJECT -f /site/docker-compose.yml -f deploy/pixcook/compose.existing.yaml up -d --no-deps sub2api
```

回滚时先满足上面的数据库与质量隔离兼容边界，将 PIXCOOK_IMAGE 换回记录的旧 digest，然后重复验证与应用替换。路径、服务名和 project 均需与现场对应；配置验证器不会代做停流量、备份或恢复。


## 双环境自动交付

pixcook分支推送触发开发CD，完整candidate门禁通过后构建Linux amd64镜像，以自有digest和完整源码SHA送往dev Environment。专用SSH账号只接受固定dev部署命令，无shell/转发；部署前备份，隔离克隆恢复与迁移成功后才替换开发应用，并核对运行digest/二进制commit和公网前端资源。详见deploy/pixcook/cicd-deploy.md。

production Environment已预置但保持禁用：workflow为constant-false，PROD_DEPLOY_ENABLED=false，分支策略production-disabled，无生产SSH凭证。该配置不修改pixcook.ai的DNS、Nginx、容器、数据库或现有流量；未来启用必须单独审查配置和迁移方案。
