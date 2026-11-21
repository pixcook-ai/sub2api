# 双环境 CI/CD 与开发站上线

用户授权：开发站dev.pixcook.ai完成构建、发布、部署、健康与实际访问闭环；生产pixcook.ai仍在旧服务器，只预置禁用配置，不切DNS、流量、容器、数据库或密钥。基线bcd1349e300a57d8079edde932829c11da9ee257。

## 契约与验收边界

- pixcook推送在质量门禁成功后构建自有amd64镜像，以不可变digest部署dev；拒绝不匹配源码和运行镜像。
- GitHub dev与production环境分离；production保持constant-false及变量门禁，无生产连接凭证或可执行部署。
- dev SSH使用专用restricted forced-command账号，仅接受固定deploy命令与严格digest/SHA；无交互shell、端口转发或任意sudo。
- 保留现有Compose project、PG17.12、Redis7、卷、原env及其他bps/us/vm容器；Nginx、证书和DNS不改。
- 备份一致性数据库与文件/配置；克隆隔离PG/Redis/应用试跑迁移成功，才允许切换。失败保留旧服务或停止失败候选，不自动恢复已服务流量的数据库。
- 验收接口沿用既有Compose/部署CLI/发布来源与公网HTTP边界，新增SSH入口拒绝production、非自有digest和非法命令的行为测试；不重复产品需求访谈。
- 证据分别记录配置、测试、真实Actions、真实主机digest/source、HTTPS前端资源/后台与数据保留；无真实模型探测授权，因此不主动触发付费上游/自动迁组。

## 初始状态

2026-11-21实时DNS：dev167.114.157.231，prod51.210.34.93；两站/health均200。dev现有3个account、1个user。321个已应用迁移按实际TrimSpace算法全部与候选一致，待255至261七个迁移。原app镜像sha256:6feb3260ee97b8acac0144db696f93f575fc6bf3a9811601c118d27eec209de2；应用/PG卷为sub2api_sub2api_data/sub2api_postgres_data。

## 配置与本地门禁

- dev Environment仅允许pixcook分支，DEV SSH凭证仅存dev，专用forced-command账号已安装；任意id及production部署命令通过真实SSH都返回拒绝，未触发应用变更。
- production Environment存在，分支策略production-disabled且有人工reviewer；仓库PROD_DEPLOY_ENABLED=false，workflow constant-false，无生产SSH秘密，不连接旧生产服务器。
- 真实开发目标保留原JWT/DB/配置；既有TOTP未配置且0用户启用，验证器允许既有两边均缺省，fresh仍需密钥，既有present/unset变化拒绝。
- 远程只读inspect_target通过。客户端9/9、部署与Compose/SSH28/28，actionlint通过；镜像迁移manifest独立stdlib测试及328条生成通过。最终真实执行结果见下节。


## 最终交付与独立验收（2026-11-21）

开发环境已上线，生产仅预置禁用配置。运行源码：554f62ca94390f424e9546c43ef39ab6c3a214b3。

固定镜像：ghcr.io/pixcook-ai/sub2api@sha256:663fb85c07e1a7638bf16275c614170fb162e9843537a3439d3a5f4b1be728cb。

- [开发CD44860155823](https://github.com/pixcook-ai/sub2api/actions/runs/44860155823)全部成功，包含五项candidate门禁、image与deploy。完整unit、真实PostgreSQL integration、race、前端类型/定向测试/构建、安全与发布/同步/部署契约通过；既有skip不作为通过计数。普通CI44860155972及Security Scan44860155925同源成功。初次CD44858448112因审查修复被人工取消，保留历史。
- 镜像发布后登出并匿名拉取、核对OCI revision，再固定digest部署。部署完成于2026-11-21 03:53:16 UTC（11:53:16 CST）。[部署证明artifact14690638161](https://github.com/pixcook-ai/sub2api/actions/runs/44860155823/artifacts/14690638161)保留90天，下载后独立核对image/runtime_image与source/runtime_source完全一致。
- 远端独立回读：app容器c25187d3f9a3运行上述digest；schema_migrations的328条记录与镜像328条清单逐项匹配。原PG、Redis、vm2api、bps-local及pixcook-us三个容器ID全部保持且运行。试迁移容器/网络均无残留。
- 完整备份保留于 /var/backups/pixcook-dev/20261121T035253Z-1e77af79，database.dump 19,158,203 bytes，app-data.tar 105,410,560 bytes；backup.json=complete，deployed.json=deployed。先恢复隔离副本完成试迁移，再切换live开发应用；切换后失败不自动让旧二进制连接已迁移数据库。
- 账号ID/credentials摘要/proxy_id、用户与分组ID逐项一致，仍为3账号、1用户；原.env、基础Compose及全部Nginx vhost的SHA256不变。未输出凭证、JWT、数据库密码、环境秘密或备份私有配置。
- 独立公网检查dev仍解析167.114.157.231、health200；部署proof中首页、管理页和3项JS/CSS资源均200。真实HTTPS管理员登录与auth/me均200；Tibo设置从旧版404变200，current_version=dev-554f62ca94390f424e9546c43ef39ab6c3a214b3、update_mode=image。
- Ego Lite后台保留登录并显示新版本、3账号和1用户；系统设置→网关服务显示Tibo探测、异常停止调度、健康恢复及可选迁组。未保存设置、启动探测或变更分组。API读取enabled=false、auto_group_enabled=false、pause_on_degraded=true、auto_resume_on_healthy=false。
- 生产保护再次核验：PROD_DEPLOY_ENABLED=false，workflow constant-false；production环境分支限制production-disabled，required reviewer且prevent_self_review=true，生产secrets为空。pixcook.ai仍解析51.210.34.93，health200且正文SHA256与部署前一致。未连接生产SSH，未改DNS、代理、服务或数据库。
- 两轴审查完成：Specs无遗留；Standards发现隔离Redis空变量继承旧YAML认证/TLS的P2，已在554f62c修复且复审无遗留。克隆Redis采用随机非空密码、default用户名和显式TLS=false；真实隔离Redis7认证PONG、无认证NOAUTH，未改变live Redis。

## 后续运行与边界

推送pixcook后，质量门禁通过才自动构建并部署dev；部署前检查分支最新SHA，按不可变digest切换。仅docs/tasks/**和docs/dev-journal.md归档变更不触发部署，本次后续文档提交不改变已部署源码。上游继续通过普通merge候选PR同步，保留本仓工作流。

生产仍停留在禁用占位配置，实际目标接入、生产迁移与启用须另行执行。未触发真实模型主动探测、自动隔离/恢复业务动作或计费请求；不以CI/CD成功声称上述业务验收通过。
