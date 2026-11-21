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
- 远程只读inspect_target通过。客户端9/9、部署与Compose/SSH28/28，actionlint通过；镜像迁移manifest独立stdlib测试及328条生成通过。真实镜像/迁移/部署尚待执行。
