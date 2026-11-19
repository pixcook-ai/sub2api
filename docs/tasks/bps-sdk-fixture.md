# BPS 官方 SDK 的真实主仓组合 fixture

关联：`pixcook-ai/bps-gateway` #1、#2。验收入口使用原 `backend/cmd/server`，运行模式固定为 standard，经真实 API key 鉴权、模型白名单、调度器和 Forward 到本地 gateway/fake BPS。工具不模拟主仓 HTTP handler，不需要管理面登录或合规确认。

## 启动与停止

本机须有 Go、Python 3、rtk、Docker，以及已经存在的 `postgres:18.1-alpine3.23`、`redis:8.4-alpine` 镜像。启动器不会拉取镜像；运行时仅使用新建隔离 PostgreSQL/Redis 和随机合成凭据。

```sh
rtk proxy python3 tools/bps-sdk-fixture/launch.py \
  --gateway-repo /absolute/path/to/bps-gateway \
  --out /private/tmp/bps-main.fixture.json
```

进程保持前台。manifest 出现表示 HTTP 健康检查已通过，SDK 场景仍须另行运行。向 manifest 中的 launcher_pid 发送 SIGUSR1 只重启真实 main 进程，保留同一 PG/Redis、端口和文件目录；等待 main_restart_file 中 restart_count 增加及新 main_pid 后，再做原文件 ID 的 SDK retrieve/content 和哈希比对。manifest 的 main_pid 是初次启动 PID，当前 PID 以该私有状态文件为准。向启动器发送 SIGINT/SIGTERM 后停止本次 main/gateway 进程和容器，删除自己的临时文件及输出 manifest。`--keep-private-logs` 显式保留私有目录供排错，其中含合成凭据，使用者须自行清理。

```sh
rtk proxy python3 /absolute/path/to/bps-gateway/scripts/sdk-conformance/run.py \
  --fixture-manifest /private/tmp/bps-main.fixture.json \
  --main-repo /absolute/path/to/sub2api \
  --report /private/tmp/bps-main.report.json
```

正式来源证明使用 `--gateway-repo`，由启动器在实际 checkout 构建 seed、main 和 gateway。manifest 记录实际 Git HEAD、tracked dirty 状态、三个二进制 SHA256、镜像和资源 ID；key 仅通过私有文件路径传递。构建前后 Git 状态须一致且没有 tracked/untracked 修改才声明来源已验证。`--main-binary`、`--seed-binary` 或外部 `--gateway-manifest` 仅供诊断，`source_provenance_verified` 为 false。未提交的 tracked 修改、缺少来源字段或诊断二进制不得作为最终提交验收。

## 隔离与场景合同

启动器只接受 HTTP literal loopback gateway/metrics 地址、显式端口和私有普通 manifest/key 文件。seed 在连接数据库前要求私有 marker、DATA_DIR/CONFIG_FILE 一致、standard mode、loopback DB/Redis/server，以及精确的随机数据库名；数据库存在用户、账号或 key 即拒绝 seed。

默认正例池有同用户两把 key 及另一用户 key，用于所有权隔离。Python/JavaScript 的 429 不重试、默认重试和截断各有独立 Group/key/Account 池，保持主仓真实 cooldown 保护。一次完整负例后应重建 fixture 再重复验收，以免上轮 cooldown 污染结果。

Files 在同一主仓 base_url 使用真实鉴权和落盘服务，配置仅在本 fixture 打开。工具不声明大文件、列表、删除、搜索、BPS 原生文档和真实产物已通过；按各自任务的独立成功门验收。真实 BPS 不参与本地组合测试。

## 已验证与未通过

2026-11-20 开发诊断：seed 首条缺 marker 测试由 undefined run 红转绿，launcher 八条边界和清理/来源回归测试通过。真实 cmd/server + 独立 PG/Redis + gateway `dba1b16` 已启动，双官方 SDK 22 场景诊断为 20 通过、2 失败（Python/JavaScript 默认重试 429 后被真实主仓 cooldown 转为 503；BPS accepted=0）。文本、SSE、function、鉴权、显式 strict 负例、Files 创建读取均通过；这一轮使用预构建 main `a2c9419`，属于诊断，不能作为最终提交来源验收。后续最终来源和主仓改动须重新运行并保留独立报告。
