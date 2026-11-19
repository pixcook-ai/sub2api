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

默认正例池有同用户两把 key 及另一用户 key，用于所有权隔离。Python/JavaScript 的 429 不重试、默认重试、policy-rejections、policy-catalog、tool-strict-default，以及 04 的 failed-usage、truncated、incomplete、annotations 各 stream/JSON 场景均有独立 Group/key/Account 池（默认正例池加 26 个 SDK 场景池），保持主仓真实 cooldown 保护；policy-catalog 的八个步骤仍在同一场景池/会话中，不能拆分。manifest resources 同时记录 scenario_groups/scenario_accounts，供真实 PG 关联核验。一次完整负例后应重建 fixture 再重复验收，以免上轮 cooldown 污染结果。

Files 在同一主仓 base_url 使用真实鉴权和落盘服务，配置仅在本 fixture 打开。本地大文件管理按下述独立 gate 验证；工具不声明列表、删除、搜索、BPS 原生文档和真实产物已通过；按各自任务的独立成功门验收。真实 BPS 不参与本地组合测试。

## 已验证与未通过

2026-11-20 开发诊断：seed 首条缺 marker 测试由 undefined run 红转绿，launcher 八条边界和清理/来源回归测试通过。真实 cmd/server + 独立 PG/Redis + gateway `dba1b16` 已启动，双官方 SDK 22 场景诊断为 20 通过、2 失败（Python/JavaScript 默认重试 429 后被真实主仓 cooldown 转为 503；BPS accepted=0）。文本、SSE、function、鉴权、显式 strict 负例、Files 创建读取均通过；这一轮使用预构建 main `a2c9419`，属于诊断，不能作为最终提交来源验收。后续最终来源和主仓改动须重新运行并保留独立报告。


## Files 独立正式 gate

2026-11-20，实际 source main `8d67fa0adfe261c6f6634e894007dd6d1729d176`（包含 Files `7b6e84c`），gateway `3b022a742c42eb76ae24f006835262912bf28541`，三产物自建且两 checkout 无 tracked/untracked 修改，`source_provenance_verified=true`。

- 官方 Python `3.55.1` / JavaScript `7.60.0` 的 Files 创建、元数据读取、内容下载均通过。报告整体 `incomplete` 是因为这一轮没有跑 18 个推理基线；该结果不改变 Files 独立 gate。
- Python 以 rb 文件句柄上传 **34,603,008 字节（33 MiB）**，发送 SIGUSR1 后最终带脚本来源哈希的复验中，旧 main PID `67851` 确实退出，新 PID `74881` 健康启动；保留同一数据库和文件目录。
- 重建 SDK client 后，以同一 ID `file-d5c530531e417556ad3f9649ad1f3e28` 读取并流式下载。字节数与 SHA256 `59d8b2aea4b5a749b078f93edbc00cb8fae0a7d85bc232ed5848dbea509905c7` 一致；restart_count 从 1 增为 2。
- 全部 Files 场景的 BPS HTTP attempts、accepted generations、uploads 都是 0；这验证主仓文件管理，不证明 BPS 阅读能力。
- main binary SHA256：`a4f36e69cab0b21d93d789b8f2c1c41056082be035e47c63bd5b5a5488f18bb8`；seed：`399d04e7b24d4c9d8e920dac57e65d0bb0980fb349c7a3c5b13ceba2aaeffe57`；gateway fixture：`c56409ae109dbcb6487d165ae20c745354d7db026269c2bbb98dd6e0f8002298`。

最终复验 runner_source_sha256 为 `7319cef42ccc03a03f832a8288a58604abcf3ee2fec4f2d86fb001cf93f0987b`。持久脱敏报告位于附件仓 `pixcook-ai/bps-gateway` commit `0dc4a2c` 的 `scripts/sdk-conformance/evidence/files-large-restart-source-20261120.json` 与 `files-sdk-source-20261120.json`；本机报告：`/private/tmp/bps-main-files-source-sdk-20261120.report.json`、`/private/tmp/bps-main-files-large-restart-20261120.report.json`。客户端临时 33 MiB 文件已删除。服务端 fixture 暂为后续请求政策/SSE 验收保留；本票 #2 的完整 lifecycle 清理仍须实际退出后另记，不能从该 Files gate 推断已完成。上线持久卷、200 MiB 实际端到端和真实 BPS 文档理解未在本轮验证。
