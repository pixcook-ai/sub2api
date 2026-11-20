# Pixcook 镜像部署配置

生产发布流程、备份和 Tibo 回滚语义由仓库发布说明维护。本目录只提供可检查的配置，不自动连接服务器或发布镜像。

- `compose.yaml` 仅用于全新安装，独立 project 与持久卷，应用只监听宿主机127.0.0.1，PG/Redis不发布端口。由反向代理提供HTTPS。
- `compose.existing.yaml` 仅覆盖既有部署的应用镜像与 `SUB2API_UPDATE_MODE=image`。原 Compose 必须放第一位；保留原 project name、env、卷、PG/Redis版本和密钥，不用新安装模板替换它们。
- `.env.example` 只用于新安装。既有站点仅向原env增加 `PIXCOOK_IMAGE`，不要轮换原JWT/TOTP/数据库身份。
- 镜像必须为 `ghcr.io/pixcook-ai/sub2api@sha256:<64位小写十六进制>`，从已批准发布元数据复制。

验证不会启动容器、打印渲染后配置或密钥：

```sh
# 新安装（在本目录，先填写私有.env）
rtk python3 validate.py --mode fresh --env-file .env --compose-file compose.yaml

# 既有部署（所有路径按实际站点指定；同一原始env/project用于前后渲染）
rtk python3 validate.py --mode existing --env-file /path/site/.env --project-name ORIGINAL_PROJECT \
  --baseline-file /path/site/docker-compose.yml \
  --compose-file /path/site/docker-compose.yml --compose-file /path/compose.existing.yaml
```

验证器检查固定自有镜像、镜像管理更新、密钥、loopback入口与应用数据持久化；既有部署还要求原其他服务、数据挂载、project、网络和身份完全不变。它不证明数据库迁移可逆、备份可恢复、镜像来源可信或真实账号调度通过。已有公网直出端口需先完成独立的反代/loopback调整，不由镜像覆盖隐式修改。

启动/更新必须在单独的发布步骤中执行，且先备份数据库、应用数据/私有文件与配置。应用镜像回退不等于数据库或Tibo隔离状态回退。保留旧镜像digest与配置；不要执行 `down -v`。
