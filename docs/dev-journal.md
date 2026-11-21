# Development journal

## 2026-11-20 — Fresh official SDK and PostgreSQL acceptance after snapshot fix

Source-built isolated main 840446e3c1849f0c464ad9d31e93f65e48c521fa / gateway 85af8ec1b052aa648108578f22ae89ae6779116a passed launch provenance with both source trees clean. Official Python SDK 3.55.1 / JavaScript SDK 7.60.0 executed 86 cases through the same main base_url, real authentication/scheduler, gateway, and local synthetic BPS. Exactly 84 passed and two failed: both SDK Files DELETE calls returned plain 404. Baseline and ticket gates 01/03/04/06/07/08 passed, but the overall run correctly remains failed; the report runner no longer hides nonbaseline failures. Evidence: docs/verification/bps-sdk-20261120-1143.json records source/binary hashes, per-case deltas, and original report digests.

The eight managed-file positive cases include 33 MiB-plus JSON/SSE inputs and repeated references. Actual PostgreSQL readback confirms one successful upload operation per file, exact SHA256/bytes/account/group/backend identity, reuse without a second upload, released reservations, and no started/uncertain operations left. Ten failed/incomplete response cases each have exactly one correlated usage row (6 uncached input + 5 cached input, 4 output); 112 total usage rows cost 0.02824, matching synthetic user balance 99.97176 from 100. These are local full-chain results, not real BPS document understanding or production acceptance. Stored responses, metadata recovery and mixed inline input are subsequent source slices outside this frozen run.

## 2026-11-20 — Integrate bounded same-identity recovery ledger module

Integrated the four-file recovery module as 544cb4517 (worker 08a7740e7f51a067da6657bcd1b1885eded8709b). It reserves new kind=recovery operations under existing full owner/selection/version/lease fences and database-clock rolling recovery budgets while preserving old confirmed remote facts. No new migration is required. Actual module validation: service 101 tests/subtests and PostgreSQL 42 tests/subtests passed, zero skipped; two-axis final review reported no P1/P2. The first PG final run had one test assertion that incorrectly forbade normal lease cleanup on an unaffected batch member; only that assertion was narrowed, then the entire 42-case PG slice passed. Transport/resolver/gateway integration, real restart/TTL recovery, final SDK and native BPS acceptance remain separate gates; this commit alone does not close plan 21.

## 2026-11-20 — Managed first-reference snapshot admission repair

The fresh source-built official dual-SDK run on main f23ab9643 / gateway 122ef937 reached the real auth/scheduler chain but exposed managed-file first references failing with capability_mismatch. The advanced scheduler receives credential-filtered candidate metadata; that projection cannot validate the complete private BPS backend contract. The server-only managed first-reference pass now reads the owning account rows before the existing scheduler filters. Ordinary snapshot scheduling, secret projection, explicit capability checks, current-row/configuration fences around slots and generation, and account pinning remain unchanged. Added an actual scheduler-entry regression with a redacted candidate snapshot and complete owning row, covering both scheduler modes. The advanced case failed for the observed reason before the change; the final affected run passed 61 tests/subtests, zero failures/skips, service 2.337s. Raw logs: /tmp/bps-managed-snapshot-red-v2-20261120.jsonl and /tmp/bps-managed-snapshot-green-20261120.jsonl. Initial test constructor compilation error was corrected and is not counted as behavioral RED. Final rebuilt dual-SDK/PG acceptance remains pending; the earlier report contains 47 passing and 17 failing cases despite its baseline-only top-level status. No production, real BPS, credential activation, push or deployment.

## 2026-11-20 — Managed generation and failure-contract composition

Integrated main 7cc1b1bb2 contains the reviewed managed files/lifecycle, failure-v1, cooldown, seed/launcher and stored-response modules. Added a focused actual Forward regression combining trusted managed-generation context with selected failure-v1 JSON429 and SSE200 failed terminals. It asserts the failed response ID, observed9/4 usage, private-header override, one authorization and one upstream send, plus no second send on another Forward invocation. Independent initial review caught an invalid ordinary account fixture; the test now reuses the existing explicit managed-account fixture without changing production validation. Session79073 passed both modes, service1.832s; raw /tmp/bps-root-managed-failure-20261120.jsonl. This test is a development composition check, not actual admission, PostgreSQL settlement, complete SSE lifecycle or official SDK acceptance. Fresh immutable-source SDK and actual ledger readback remain pending; no real BPS, account enabling, push or deployment.

## 2026-11-20 — BPS managed file first-reference integration

Issue `pixcook-ai/bps-gateway` #10, plan 07. Existing missing outbound-identity and account-extra conventions remain absent; the coordinating agent's recorded authorization continues to apply. Existing identity behavior is preserved, and explicit BPS file configuration is stored in Credentials. Contract `bps-files/1` and strict JSON fixture are copied from the gateway contract package; ownership/current-group references are resolved by the main Files namespace before external traffic.

The shared FileService holds read leases while the resolver performs Describe, whole-batch Admit, then durable write-ahead upload operations and one bound Responses send. Migration 257 records permanent account/backend/subject/generation binding and immutable confirmed remote facts. Unknown upload outcomes stay uncertain; lease loss or lifecycle retirement prevents activation without discarding confirmed late facts. Lifecycle wiring shares the owning database with Files. The scheduler requires explicit native HTTP Responses capability, excludes incompatible routes, rechecks current group/account/configuration around the concurrency slot and before generation, and refuses busy or changed bindings without switching accounts or changing sticky sessions.

Public proof headers are stripped before route dispatch and restored only from trusted generation context. Bound request bytes replace the final builder body; redirect, GetBody, idempotency hints, protocol repair, pool replay and handler failover are disabled for that generation. Archive output redacts private service/admission/binding credentials. Typed Chat/Messages and every WebSocket file-bearing frame fail before forwarding; opaque tool arguments and native Anthropic document sources keep their existing meanings. Inline images retain their ordinary path; a managed-document/inline-image combination remains limited by the gateway manifest adapter.

Development evidence before final integration: shared contract 68 cases, private client 17 cases including actual 33 MiB finite-length multipart transfer/hash, resolver 6 HTTP-fake/private-backend cases, scheduler 129 selected regressions, ingress 80 HTTP/WS and 12 WS lifecycle/idle/cancellation checks passed. A focused combined service/contract/archive/server run passed 108 cases. A later collector negative reproduced an accepted nested typed tool-result file reference; its restricted typed-content recursion then passed all 14 collector cases. Wire was actually regenerated with v0.7.0 and project dependencies were unchanged.

The ledger's 11 tests used real local PostgreSQL 15 with actual migrations 255/256/257 through an isolated temporary harness after the normal PostgreSQL 18 container runner failed readiness and the Docker socket became unavailable. This proves the bounded ledger seam, not the full migration suite. Resolver tests use fake repositories and do not replace this evidence. Ledger, resolver, scheduler and ingress each received independent Spec/Standards review. Parent integration review found a P2 about precommit managed failures being mistaken for committed SSE; four precommit 429/503 cases reproduced HTTP 200 SSE, then passed after preserving actual writer/stream state. Six terminal cases and ordinary failover/cancellation regressions passed; final narrow Spec/Standards reviews closed that P2 without reopening replay.

Final full affected packages passed for managedfiles, upstreamrecord, routes, server and cmd/server. The full handler package passed in 101 seconds. A full service-package run hit its 180-second timeout and did not pass; this is retained as an unresolved suite limit rather than reported green. The final bounded affected service run passed 198 tests across managed resolver/client/transport/scheduler, ordinary Forward, WS execution scopes and client cancellation. Four shared production contract files plus both fixture/schema byte hashes match the gateway candidate exactly. `gofmt` and `git diff --check` were applied to the final candidate.

Official Python/JavaScript SDK → the same main base URL → real authentication/scheduling → gateway → local fake BPS with real database state remains a required fresh-source integration gate. Real BPS, remote recovery/deletion and full migration-suite acceptance remain separate and unverified. No production inference, deployment or account activation was performed, and #10 remains open.

## 2026-11-20 — BPS failed response protocol preservation

Issue pixcook-ai/bps-gateway#4. Real Forward regressions reproduced marked SSE and nonstream TPM failures returning typed account failover, losing the terminal/diagnostic and settlement result. The selected gateway now declares a versioned bounded failure envelope. Main validates exact header, API-key platform, duplicate-free JSON and current classifier public message/status before keeping that terminal on its existing attempt. SSE retains HTTP200; JSON retains its error HTTP status and safe diagnostic. Ordinary unmarked handling and outbound identity are preserved. Existing account-health side effects still run, failed IDs are not successful bindings, and the existing result builder carries observed usage and all billing/model/timing metadata. The required outbound convention remains absent as previously documented.

Independent Spec review additionally caught mixed valid/invalid stream counters reaching settlement; marked SSE now parses the same safe count projection and retains prior valid progressive counts, with six mixed-count regressions. Existing verbose error tests now expect empty output and safe counts while still rejecting private instructions. Independent Spec review caught a real-shape mismatch (SSE Details has status) and missing cancelled/503/413 classes; current tests now exercise those owning-classifier shapes. Failed client output stays empty while whitelisted exact integer counts remain visible. Unknown counts never create observed zero; exact arithmetic and bounded exponent parsing reject fractional counts that float64 rounds to integers and prevent unbounded exponent allocation. See conventions/bps-failure-response.md.

Red: session81067 both marked modes failed on typed429failover. Green: session25431 focused real Forward/contract/count-negative/cancelled/503/413 tests passed (service1.244s). Gateway marker checks session27355 passed0.412s. Final affected stream/Forward/usage regression session70085 passed5.606s; successful marked native usage and normalized bare error regression session30500 passed5.203s. Validated bare error terminals normalize to response.failed before retry logic, preserving already observed progress. Their authoritative SSE event field is added after all payload rewrites; a parsed event-field regression prevents the JS SDK from treating it as a bare error. Final affected regression session85330 passed5.215s. Full SDK fresh-source and actual PG usage readback are still pending; local tests do not close the ticket. No real BPS inference, deployment or account enabling.

## 2026-11-20 — Responses failed-stream partial usage

Issue `pixcook-ai/bps-gateway` #4, narrow main-side settlement fix. Required outbound identity convention remains absent as recorded below; current source and existing identity behavior are preserved. The real `OpenAIGatewayService.Forward` seam reproduced a terminal SSE failure carrying 9 input/4 output tokens returning a nil result, so the existing handler error-settlement path had no usage evidence.

The native API-key HTTP stream path now retains a partial result only after a recognized numeric usage count has been reported and the error is not a typed account failover. Missing, null, empty, unrelated and invalid-count usage do not create a zero-consumption result; explicit numeric zero remains reported. Progressive usage and current merge rules are unchanged. Partial results reuse the full existing model, header, timing and billing metadata construction and skip successful response-account binding. Account failover, compact fallback, rates, price selection and ordinary success remain unchanged.

Scope is deliberately limited to text stream failures without image output. Existing image-error handling can fall through to success scheduling and requires a separate change before partial image results are exposed. Nonstream BPS failures arrive as HTTP non-2xx with diagnostic data and require a separate trusted shape/identity contract; this patch does not parse arbitrary error bodies for billing. SDK request-ID-to-database settlement proof is pending and cannot be inferred from parser or Forward tests.

Red/green: failed SSE 9/4 initially failed on nil result; unknown/explicit-zero tests then caught empty-object ambiguity before the numeric-count evidence gate. Targeted Forward tests cover failed/truncated streams, unknown versus reported zero, typed credential failover and absence of failed response bindings. Affected SSE/compact/policy tests: 121 passed with the unit tag. Both review axes identified a P2 mismatch between the legacy first-object extractor and a later-object evidence scan; the mixed-location regression reproduced a fabricated zero result, and evidence now comes from the exact same selected usage object. Final narrow review is recorded at handoff.
## 2026-11-20 — Preserve account cooldown status for HTTP Responses

The real main-server SDK fixture exposed a classification gap: the first upstream 429 persisted a five-second account cooldown, but a fresh SDK HTTP retry saw an already-filtered empty scheduler pool and received 503. The response path now reuses the model-availability candidate query and can preserve 429 with `Retry-After` from the earliest real account reset. It does not bypass cooldown, select a slot, write account state, or call the upstream during diagnosis. Three initial red/green slices covered the lost cooldown attribution, unknown runtime state, and HTTP error/header classification.

The proof is deliberately limited to ordinary OpenAI API-key candidates on HTTP Responses. It requires every matching candidate to become eligible when only `RateLimitResetAt` is removed from a local value. Other cooldowns, quota, privacy, endpoint incompatibility and unknown state retain the existing fallback; unsupported model configuration retains 404. Proxy, OAuth, gateway-pool, auto-reset, compact/strong-binding, restrictive channel and profit-control policies are conservatively unproven. Other endpoints and the pre-existing model-level text-reason classifier are unchanged; this patch does not claim to correct that classifier's attribution for mixed model-level reasons.

Two-axis review found four P2 gaps before commit: implicit guardian context, global quota defaults absent from the handler context, top-level channel billing restrictions, and cold policy getters detaching from the diagnosis deadline. All four were reproduced red. A subsequent narrow review also reproduced fresh quota/threshold error-fallback caches being mistaken for known policy; successful-read/explicit-update metadata now distinguishes those from verified defaults. The correction reads only authenticated group fields and existing valid quota/channel/threshold snapshots, declines cold/stale/error/restrictive policy, and uses shared compatibility checks with a read-only model-runtime peek. Channel, quota and threshold cache success metadata distinguishes verified snapshots from existing error fallbacks; ordinary scheduling fallback behavior is unchanged. The configured-pool repository lookup has a 500 ms context deadline, proof is capped at 256 candidates, and no second candidate lookup runs.

Validation: relevant service/handler and channel-cache regressions passed after the review corrections; stale and active model-runtime entries retain their timestamps and account block generations remain unchanged. A warm-policy wiring regression initially omitted `Group.Hydrated` in its test data; actual API-key repository projection sets it and auth middleware injects only valid groups. Only that test fixture was corrected, without weakening the production proof gate. The first affected-package full run passed the complete handler package in 101 seconds; the service package reached its three-minute aggregate timeout in an unrelated gateway-pool interval-stream test, without an earlier assertion failure. That service full run is not recorded as passing. Final focused validation passed 95 tests/subtests across service and handler (both packages passed); fresh official Python/JavaScript SDK acceptance on the integrated source build remains a separate gate; real BPS credentials, production deployment and quota entitlement remain untested here.

## 2026-11-20 — BPS policy rejection compatibility replay

Issue `pixcook-ai/bps-gateway` #3 / parent #1. The required outbound identity convention is still absent, as documented in the previous entry; this patch preserves outbound identity and uses current source/tests. The actual official Python/JavaScript SDK report `/private/tmp/bps-main-policy-A-before-fix-20261120.report.json` reproduced `max_output_tokens=7` returning HTTP 200 after main removed the rejected control and forwarded twice.

Gateway now adds `X-Bps-Request-Policy: rejected-v1` only for structured policy HTTP 400. Main recognizes the exact upstream response marker plus the agreed error shape and skips request-mutating compatibility replays; ordinary health, usage, failover and error handling remain in place. The marker never establishes nonexecution or billing exemptions. See `docs/conventions/bps-request-policy.md` for the narrow contract; WS and deferred plan 03 are unchanged.

Red/green: the gateway authenticated HTTP regression first failed for a missing marker, then passed. The real `Forward` regression first returned success after the marked rejection, then preserved the original 400 and one outbound request. Negative controls cover inbound header spoofing, unknown marker version/case/duplicate values and malformed error shape while retaining ordinary unmarked parameter retries. These targeted service tests support development; the official double-SDK main combination gate remains pending and is required before issue closure.

Affected regression: gateway proxy/sanitize 209 passed; main rejected-field normalization, retry budget, policy marker and related Forward/compact cases 66 passed. `git diff --check` passed in both repositories. No production inference, deployment, full build or whole-repository suite was run.

## 2026-11-20 — BPS compatibility 06: durable main Files

The worktree AGENTS.md requires `docs/conventions/codex-outbound-identity.md`, latest `docs/dev-journal.md`, and `docs/tasks/` before implementation. These paths do not exist in this checkout or its owning main checkout. Exact-name search under the current project found no convention source; `git ls-files` and `git log --all -- docs/conventions/codex-outbound-identity.md docs/dev-journal.md docs/tasks` returned no entries. The coordinating agent explicitly authorized documenting the missing prerequisites and proceeding from current source/tests without reconstructing unknown conventions. Files work retains current outbound Codex identity behavior and adds only a separate authenticated storage boundary.

Issue: pixcook-ai/bps-gateway#5, plan 06, specification #1. Red/green acceptance uses actual authenticated main HTTP, migrated PostgreSQL, and private filesystem storage. SDK acceptance remains a separate gate owned by issue #2.

Implemented a separate no-model Files route, user/current-group authorization, private persistent streaming storage and PostgreSQL budget/upload/read leases. Upload expiry is checked before SimpleMode and billing bypass; no existing outbound identity fields were changed. Full main wiring was regenerated using `go run github.com/google/wire/cmd/wire@v0.7.0 ./cmd/server` (tool-specific missing sums were avoided without changing project dependencies).

Initial real HTTP red was 403 for a zero-balance owner. The Files policy green retains exact key/user/IP/group/expiry checks while skipping inference billing. Real PG/HTTP acceptance now has 21 passing tests/subtests, including restart/hash/key rotation, actual active-status ExpiresAt, SimpleMode, revoked identities, streaming limits/purpose order, client cancellation, actual PG trigger write failures and rename-before-DB-commit staging recovery. Repository read-lease invalid renewal and mutation-lock competition both had red evidence before fixes. Middleware/routes/server unit packages: 653 passed. Private store explicit-file race: 24 passed; actual finite-volume and final combined validation still pending at this entry.

Final boundary review identified two P2 issues: skipping billing also skipped subscription-group entitlement, and the full server's outer global body cap constrained the independent Files route. Both were reproduced red using the real migrated-PG/auth/`ProvideHTTPServer` seam: a 4 KiB upload under a 1 KiB global JSON cap returned 413, and an unauthorized subscription-group key returned 200. The fixes preserve subscription authorization (including SimpleMode) without inference window/quota enforcement, and apply the independent finite Files cap only to enabled `POST /v1/files`. JSON retains the 1 KiB acceptance cap. Final PG/HTTP tests: 24 passed, including feature-off rollback returning 404 and re-enable preserving the same public ID and original bytes; full affected config/server/middleware/routes/cmd-server unit packages: 1042 passed. Candidate before these two fixes additionally passed 21 real PG/HTTP tests under `-race`.

Actual disk-capacity acceptance passed in an isolated network-disabled/read-only-root Docker container with a real 1 MiB tmpfs volume: `TestPrivateFileBlobStoreRealVolumeExhaustion` streamed an 8 MiB source, returned storage exhaustion and left both staging/blob directories empty. Cross-compiled only the two private-store files with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`; no production volume or BPS account was used. Both Standards and Spec narrow reviews closed the two P2 findings at `efd66f8`, with no new P1/P2. The final evidence commit adds only the rollback test and this journal update. `codex/bps-openai-compat-20261120` was merged before handoff and was already up to date at `24e2a2890f4f647a8999945728ba3b6af1212ef8`. SDK, production volume recreation and issue-closing gates remain outside this local acceptance.


## 2026-11-20 — BPS compatibility 08: local file lifecycle

Issue pixcook-ai/bps-gateway#11, fixed implementation baseline `7620df25eba236ebac89ded75416d40b9ba58776`. The local Files lifecycle uses owner UserID/current GroupID, immutable `(created_at,id)` pagination and official bracket multipart retention fields. Migration 256 keeps migration 255 unchanged, separates published retirement from staging tombstones, and preserves original bytes and remote facts after local quota release. The per-file monotonic `remote_activity_possible` write-ahead flag supersedes the initial global local-only capability proposal: false permits local-only cleanup; true requires the same-transaction managed ledger port, and unavailable/error fails closed.

Read grants return actual database clock and expiry; service deadlines conservatively anchor remaining duration before SQL. Existing readers renew only live tokens, with expiry plus fixed drain cap; a watchdog closes the FD and releases the read even without further client Read. Local cleanup uses independent token/version CAS and retry after unlink-before-confirm; remote state remains retired and no remote DELETE/public deleted success is invented. A bounded periodic worker also revisits staging recovery and advances past blocked rows.

Upload admission now reserves maximum bytes and user/global slots before the first multipart NextPart. Staging/tombstone retain both charges until physical cleanup confirmation. Database upload deadlines cap renewal and final publication; actual metadata and retention publish atomically. Transport absolute deadlines and lease-loss cancellation share the existing idle/drain deadline state, so first-boundary stalls, trailing fields and continuous drip cannot keep uploads alive. Publication rechecks uncached key/user/current-group/IP/subscription authorization, preserving unbilled Files policy.

Earlier Docker-off runs were skipped and are not passing evidence. After OrbStack restart, 10 actual migrated PostgreSQL/authenticated HTTP/private-filesystem cases passed; two additional actual slow-first-boundary and mid-upload key-revocation cases passed. A subsequent 14-case lifecycle suite passed under race; final expanded suite and review results remain pending at this entry. Transport-only actual HTTP/1+HTTP/2 package race passed (16.508s), including same-connection stream isolation; final trailing multipart case independently race-passed. No real BPS calls, push, deployment or Issue closure. Actual 07 ledger integration and official dual-SDK acceptance remain separate required gates.


Final local acceptance: 43 real Files PostgreSQL/authHTTP/private-store tests and subtests passed under race with zero skipped (`/tmp/bps-files08-final-pg-race.jsonl`). The subsequent narrow absolute-body deadline case passed after removing generic network-timeout reclassification. Actual primed subscription cache revocation and real table-unavailable cases both passed under race; the former returns403, the latter503, and both release local charges. Independent Spec and Standards reviewers closed their original P2 findings with no remaining/new P1/P2 in the narrow re-review (`/tmp/bps-files-current-spec-review.md`, `/tmp/bps-files-current-standards-review.md`). A final atomic cross-process global slot test is additional to the43. Affected config/middleware/routes/cmd-server full unit packages passed; focused handler error classification passed. Wire was generated using temporary modfile to avoid repository dependency churn. Combined07, official SDK and realBPS remain pending; no Issue closure.

## 2026-11-20 — BPS compatibility 07/08: combined managed-file lifecycle

Integrated local lifecycle tip `4afd041` as `f9413138e`, then the coordinating main failure/cooldown candidate `517d60c` as `3aa2e3438`. Journal conflicts retained both sides; no main failure-response production logic was manually rewritten. Existing resolver and ingress test repositories now implement finite database-clock read grants and synchronize watchdog activity counters. The unchanged old read interface reproduced nil-interface panics before this adaptation (session 83983); the same focused service/handler tests passed afterward (session 4586).

Three new actual PostgreSQL combination tests pass using the normal repository TestMain, fresh PostgreSQL 18 and Redis, and full ApplyMigrations through 257. `CI=1` makes missing Docker a failure instead of a silent skip. The authenticated HTTP upload uses the actual private-store file descriptor and managed ledger repository. Cases verify retirement plus ledger fencing in one transaction, started-to-uncertain transitions, immutable late-success facts without renewed generation authority, active-reader purge exclusion, watchdog closure without further reads, fixed drain limits, stale purge CAS rejection, physical cleanup preserving complete remote facts, trigger-induced atomic rollback, and advisory-lock-controlled completion versus retirement row-lock competition. Session 14961 exited zero: three tests passed, none failed or skipped, package 8.388s (`/tmp/bps-0708-combined-pg.jsonl`).

Final affected ordinary service/handler regression on the combined candidate passed 179 tests/subtests, none failed or skipped (session 20690; service 1.875s, handler 1.670s; `/tmp/bps-0708-final-ordinary.jsonl`). It includes managed files, request collection, private BPS file control transport, single-send generation, failure/stream boundaries, marked policy rejection and cooldown selection. Actual Wire regeneration after the main merge succeeded without generated or dependency changes (session 4111). Independent Spec and Standards combination reviews report no P1/P2 findings in this bounded diff.

The focused combined race attempt could not build because the linker/vet ran out of disk space (session 23749); it is not passing race evidence. The earlier full service package run timed out at 180s and is not a passing whole-package result. No real BPS calls, push, deployment or Issue closure occurred. The official dual-SDK main-base-URL/auth/scheduler/gateway/fake-BPS composition and authorized real-BPS document/search/artifact checks remain separate required gates. The heavy Go slot was released to the stored-response/SDK fixture owner immediately after the final affected regression.

## 2026-11-20 — BPS compatibility 03: immutable tool-case fixture snapshot

按已冻结的 SDK 夹具接口，主仓启动器将 `scripts/sdk-conformance/cases/tools.json` 纳入第五份只读源码快照及 `gateway_source_files_sha256`，并向快照中的 wrapper 显式传递绝对 `--tool-cases` 路径。原 SSE 合同、两份 Go 源及既有场景保持不变；工具和文件场景使用的 `store:false` 由 SDK 合同定义，本次不改变请求政策或生产转发逻辑。

TDD 首先复现快照缺少工具合同，再复现启动参数缺少 `--tool-cases`；对应修复后，全部 20 项启动器 Python 测试通过（10.092s，ResourceWarning 按错误处理）。新增启动接缝测试截获子进程参数，不启动 Go、Docker 或 BPS；快照测试验证源文件改动不影响只读副本和独立 SHA-256。本次未改正在正式验收的 `sub2api-files-20261120` 工作树，也未操作旧夹具 49874。新版 11 项工具场景的真实主仓/双 SDK/网关组合仍须由主线另行验收；无 Go 构建、数据库、真实 BPS、推送或部署完成声明。

## 2026-11-20 — BPS 21/22 恢复与实时授权接线

在已协商 metadata recovery 的 Describe 后，Resolve 使用一次恢复轮次并采用返回的完整 reservation；重新打开公共文件及内联原文，关闭时释放新许可。Describe、admission、durable Start 前后和最终发送前复核当前授权。Start 或账本授权等待期间撤权、以及 detached send 授权期间原客户端取消均已确定性复现 RED 后修复；已开始但未发送的上传保留 uncertain 事实。ManagedFilePreparationError 将本地准备失败与账号传输健康处理分离，私有恢复头在公开入口及出站清除。

最终定向 service/handler 测试 41 PASS、零失败或跳过（/tmp/bps-managed-recovery-final.jsonl）；独立 Spec 复核关闭两个 P2，Standards 初审无问题。handler 独立切片 c4752705b 同样 41 PASS。网关私有恢复头检测 1 PASS（/tmp/bps-private-recovery-header-final.jsonl）。这些数字不代表最终 SDK/PG/restart 验收；Composite 路由及模型映射鲜读围栏、14 存储合并和正式双 SDK 仍待后续集成。未推送、部署或调用真实 BPS。

## 2026-11-20 — BPS compatibility 14: stored Responses production admission

本地 Responses 存储接入最终 HTTP 请求：只接受服务端明确声明的独立本地能力，最终 wire 预检通过后才保留一份记录并发送一次。预检绑定账号完整身份、模型及原始 wire；客户端私有头会剥离，推理 ACK 用 HMAC 校验。标准 policy400 通过严格字段、错误码白名单及固定消息映射，预检拒绝不创建记录、不发送推理。坏 ACK 不输出上游内容，不重放，并保留可验证的 usage。候选账号改从 owning rows 校验完整私有合同，普通调度仍使用现有快照。

原 caller、首输出 header deadline 和存储 lease 取消分别保留：网络继承 header 取消并接收 lease 的单向取消；正常关闭响应体不会反向取消数据库 Finalize。最终 wire 和临时预检 envelope 在复制前预留共享 ingress 内存，失败按容量503处理并退款。生成前沿用新实时授权端口，在预检前、Begin 前和唯一发送前核验原始客户模型、当前用户权限及 owning account 配置，缺端口拒绝执行。

真实 RED 已复现：脱敏快照导致 advanced scheduler capability_mismatch、慢预检在超时后仍发送、慢 header 超过配置期限、预算不足仍进入预检、正常 BodyClose 导致 Finalize 的上下文取消，以及缺少实时端口或三阶段撤权后仍发送。对应回归覆盖安全 policy400、单次预检/保留/发送、坏 ACK usage 保留、正常失败状态与最终公开 payload，以及预算拒绝和临时 envelope 退款。Spec 与 Standards 独立窄复核已关闭全部原 P1/P2；最终受影响单元集合121项通过、0失败、0跳过（service5.167s、managedfiles0.343s），原始记录 /tmp/bps-main14-integrator-final.jsonl；包含普通调度/流处理的相关回归。

配套 capture 独立提交保留 JSON/SSE/SSE转JSON 的最终公开字节与本地ID：持久化先于终态输出，异常只本地终止并保留已观测用量；完整终态帧立即输出，不依赖上游随后空行。HTTP 分流独立提交在存储域混用时提前拒绝，统一处理已发心跳的错误流，保留普通账号过期策略，并将实时撤权固定映射403、授权不可用映射503。其最终受影响集合38项通过、0失败、0跳过；capture11组与普通超时相关回归已通过。

本条仅记录本地源码接线与定向验证；最终同主仓 base_url 的官方双 SDK、PG 重启/TTL/读取/删除及源码干净度由主线统一验收。未操作真实 BPS、推送、部署或关闭 Issue。

## 2026-11-20 — Tibo 质量探测迁移与可选分组

在独立 `codex/tibo-migration-20261120` 工作树迁入本机 `sub2api-czc` 的 Tibo HTTP 探测、投票/退避/预算、24 点历史、持久化与缓存、健康路由排序和管理员使用记录三态观测。初始基线 28842f406，后合入目标仓已提交的 09def4827 保留 Files/Responses 授权生命周期；最终功能 diff 与双轴评审固定对比 09def4827，不覆盖原主工作区的其他脏改动。使用说明见 `docs/tibo-quality-probing.md`，验收契约及过程见 `docs/tasks/tibo-migration-20261120.md`。

系统设置新增独立 Tibo 面板。探测总开关与自动迁组默认关闭，目的组按 ID 选择，不再依赖 `GPT-不降智`/`GPT-福利` 名称。关闭迁组无需选组；迁组仅触达已属于两个所选组之一的 OAuth 账号，并保留其他关联及元数据。自动质量隔离独立于分组和管理员 Schedulable；自动恢复/手动清除只移除质量标记，旧在飞探测受清除围栏约束，不能擅自重新启用管理员暂停账号。

新鲜观察、动作、组关联与 outbox 同事务；运行态 extra 不接受普通管理表单覆盖。探测绑定凭据/代理身份，主动和被动观察各自通过事务围栏，私有副本仅在提交成功后发布内存。身份切换不继承旧历史，读到不匹配快照时返回 unknown。HTTP/WS 最终发送通过窄 SQL 复核账号及母账号质量位，关闭监控不会绕过已有隔离；内部账号绑定 context 标记允许恢复探针继续运行，公开 header/body 无法取得该豁免。质量读取失败仍拒绝发送，数据库读取到网络发送之间的正常并发窗口仍存在。

独立 Spec/Standards 评审发现并关闭恢复探针被隔离、旧身份内存污染/读取、重复完整账号 DB 读取及被动观察覆盖并发预算的问题。固定 Tibo 单题仍是启发式信号；自动动作沿用新鲜明确原始样本，路由 confirmed verdict 则使用投票，两者不混为一谈。Ent 与 Wire 已实际生成，未改旧迁移、依赖或锁文件；仅新增 260/261 迁移。前端受影响 10 个文件 251/251 测试、vue-tsc/Vite 完整构建与相关生产文件 ESLint 通过。最终后台/PG/race 结果记录于下文。

完整 unit 首轮终态暴露 service 包 300 秒超时与既有 `TestOpenAIResponseFlush_FailedAndErrorEventsFlushAtBoundaries/failed_at_EOF` 断言不一致；该子测试已在临时干净 09def4827 实际复现同一 usage/output 差异（`/tmp/tibo-baseline-response-flush-20261120.jsonl`），其 owning 实现和测试均未被 Tibo 修改。临时基线工作树已清洁回收；不为本任务修改失败响应契约或放宽断言。全套不是绿灯，不以已完成的 30,975 个通过测试掩盖失败/跳过/超时。

本次不部署、不推送，不操作生产账号/分组；未完成浏览器或真实上游账号联调，不把模拟上游 HTTP 与隔离测试数据库视为这些范围的完成证据。

最终 service 全包以 parallel=16、timeout=900s 完整结束（316.726s）：23,915 PASS、29 SKIP、4 FAIL，即上述既有响应断言和一次 gwpool 超时/预热时序子测试，分别连同父测试计数；无超时。gwpool 用例单独 20 轮共 60 个 tests/subtests 通过，保留全包失败与未排除波动的事实，不将它描述为已修复。Tibo 所有关键功能回归通过，包括实际模拟 HTTP 恢复、私有豁免边界、身份拒绝不污染内存、读取 unknown、并发预算退款及 shadow owner 绑定。最终隔离 PG18/Redis 迁移/事务/缓存与 usage 三态验证 39/39 通过、零跳过（`/tmp/tibo-postgres-final-20261120.jsonl`）；未触碰既有开发或生产 DB。

最终 service/repository 定向竞态检测 199/199 tests/subtests 通过，零失败、零跳过、零 DATA RACE（`/tmp/tibo-race-recheck-20261120.jsonl`）。该轮先发现测试夹具重复 `gin.SetMode(TestMode)` 与已运行探针 `gin.New` 的全局读写竞态；删除夹具全局写入后同组重验通过。Standards 核对 Gin 源码确认仅调试输出有差异，未改变生产逻辑或放松断言。


## 2026-11-20 — Tibo 完整 service 验收失败修复

用户要求处理前次保留的两项失败，工作基线固定 `829e710fe`。EOF 失败终态旧预期删除 usage/output，与已提交的公开安全终态契约冲突；当前工作树已实际复现 RED，修正为保留计数和空 output，保留完整字节相等并加强两次 flush 快照及 output token 检查。没有修改生产响应。

gwpool 新受控场景把真实共享取票阻塞到业务 waiter 返回过期429，再放行仍存活 follower；旧全部发送为零的断言确定性失败为2次合法预热（`/tmp/tibo-gwpool-shared-red-20261120.jsonl`）。测试改用 atomic 分别记录业务/预热，并等待 prepare/warm worker 收尾；严格要求过期业务零发送、活 follower 完成两次预热、原context保持有效。无生产调度逻辑改动。连续100轮400 tests/subtests通过；受影响组5轮竞态155项通过，零失败/跳过、零DATA RACE（`/tmp/tibo-gwpool-repeat-green-20261120.jsonl`、`/tmp/tibo-failures-race-green-20261120.jsonl`）。Spec和Standards独立评审均无发现。最终全后台unit结果记录如下。

最终完整后台 unit：`go test -p=1 -parallel=16 -tags=unit ./... -count=1 -timeout=900s -json` 退出0；66个测试包通过、52个无测试包，33,398 tests/subtests通过、60跳过、零失败/超时。service整包307.691s通过。跳过项包括需要外部服务/指定环境的集成或实时测试及既有显式跳过，不视为这些范围通过。日志 `/tmp/tibo-backend-after-failure-fixes-20261120.jsonl`。此前两项失败在该最终全套均通过；保留前次失败记录用于追溯。没有真实上游、浏览器、生产操作、推送或部署。

## 2026-11-20 — BPS 实时授权、URL 下载与最终组合候选

已集成 `405f8311c`：Files 与 Stored 使用独立严格能力授权，冻结客户原始 public model，并在预检、Begin 和最终发送前鲜读当前权限；97 项定向测试通过，零失败或跳过，证据 `/tmp/bps-auth-freshness-final.jsonl`。Composite/current Group/account/model mapping 的独立授权实现位于 `09def4827`，不能用调度缓存代替最终 owning rows。根 Forward/routes 接线保留 Composite public model、ctxkey.Model、body 的优先顺序，并将私有 resolver 注入请求上下文；该接线候选的 service/routes 定向回归20项通过，零失败或跳过（`/tmp/bps-root-forward-auth-expanded.jsonl`），独立 Spec/Standards 复核均无 P1/P2，三文件冻结哈希一致；限定提交 `ee0ec85f0`，最终组合验收仍待完成。

`34948bcb3` 增加真实隔离 PostgreSQL 15.17、完整迁移、生产 middleware/repository 与实际 Ristretto 缓存预热后的 SQL 撤权测试。最终 `-race` 26个子测试加父用例均通过，零失败或跳过；原始证据 `/tmp/bps-fresh-auth-pg-final.jsonl`，复跑和边界 `/tmp/bps-fresh-auth-pg-progress.md`。测试数据库已停止，未修改既有系统数据库或缓存；此证据不是完整 SDK 计费结算验收。

`7f0ac9e5c` 提供受控 URL 下载器：逐次 DNS/连接/跳转校验、流式字节和组合预算、取消/失败清理。普通组合、定向 service 与 race 各151项通过、零失败或跳过；证据索引 `/tmp/bps-file-url12-final-handoff.md`。URL/public ID/inline 的 full-index 暂存与 recovery 集成已提交 `6431a1669`，174项相关回归及23项race均通过，零失败或跳过（`/tmp/bps-url-public-integration-final.jsonl`、`/tmp/bps-url-public-integration-race.jsonl`），独立双审无P1/P2，五文件冻结哈希一致。

gateway `ff30f0d` / `37acec4` 已登记客户端 tool_search 与 metadata recovery 双 SDK 场景，`0675b35` 增加内联/混合附件场景；登记或 fixture 通过不能替代最终公开链路。下一验收必须从干净的两个提交构建，使用 launcher 自建隔离 PG/Redis，运行全部124个官方 Python/JavaScript SDK 场景并回读 usage/ledger/balance/lease 及原生上传/生成计数。旧86项中84通过、2项DELETE失败的报告仅为历史证据，不继承为当前候选通过。真实 BPS 文档阅读、搜索、远端删除/产物及续接能力门保持独立；未调用真实 BPS、推送、部署或关闭 Issue。

## 2026-11-20 — 1836 SDK/PG 验收与来源归档缺口

固定 main `71879e98411947748078cbf95c49effff7ea0740` / gateway `1ba185fdec438e18d77f032239531c6075ce0d7d`，两仓均从干净工作树由 launcher 构建，`source_provenance_verified=true`。全新 1836 marker 与隔离 PostgreSQL/Redis 上，官方 Python SDK 3.55.1（62 场景）及 JavaScript SDK 7.60.0（62 场景）经同一个主仓 base_url、实际鉴权与调度、gateway、本地假 BPS，一次完整运行 **124/124 PASS、零失败或跳过**；supported_baseline / all_selected 均 passed，10 个所选协议 gate 均通过。包括本地 Files DELETE、Stored Responses 生命周期、14 个内联/混合附件场景、6 个同身份恢复场景、4 个客户端 auto tool_search 三轮场景。

原始 report SHA256 `5763ab607a33c7d3f804438982e81a9a5289d224aa666903fef5dec533b4314b`；该轮临时脱敏摘要仅作私有历史留存，不作为最终入库来源证据。独立 PostgreSQL 回读固定于 2026-11-20T10:42:30.986483Z，66 个要求关联的案例全部通过：24 次公共 upload 与6次 recovery、14次 inline operation 全 succeeded，10个 Stored 记录全为删除墓碑；164条 usage 无重复，总 actual_cost=0.0412400000，测试用户余额100−0.04124=99.95876000精确一致，全部活动租约及未完成操作均为0。16个 failure/incomplete/annotations 案例通过同条安全日志的 request_id→client_request_id 精确关联；6个已清理记录保留的未过期 cleanup_token 按实际源码定义为完成后的幂等围栏，原始时间字段与最初失败评估完整保留在 assessment_history，未通过修改数据库消除失败。独立 Spec/Standards 最终复核均无剩余或新增 P1/P2，分别35/112项离线断言通过；未将本地 SQL 事实扩大为真实 BPS、未执行的物理文件系统验收或两个 Files DELETE 案例的逐项 PG 关联。

本轮修正只涉及 gateway 验收夹具：完整 manifest 读上限 1 MiB（控制文件仍 64 KiB），macOS `Python` 解释器精确识别，以及 native fake 对 SDK user.content 字符串工具搜索 marker 的识别。旧 49 项前缀阻塞及 recovery/search 诊断失败保留为历史，不能与本轮结果拼接。

124 矩阵没有独立 URL 输入场景；额外纯 URL 公开入口首次补验返回503：原生夹具已有346,031,074 bytes，而512MiB总预算按每次200MiB预留，导致下一次上传在正文前被507拒绝，经gateway映射到主仓503；无新增成功上传/生成。此为本地夹具容量边界，不修改生产下载器或放宽限制；另用全新隔离夹具补验，不能由124全绿推导 URL SDK 已通过。真实 BPS、完整22票、required/forced/allowed_tools、工具来源文件、previous_response_id、跨后端迁移及原始 SSE 尾帧等门保持各自边界；未推送、部署或关闭 Issue。


## 2026-11-20 — 1908 最终源码归档与独立 URL 补验

1836 运行确实完成124项SDK与66项PG关联，但启动器清理时删除了未归档的原始manifest，独立两轴各保留一个P2来源缺口。没有重造原文件或把自述当原始来源；保留其原始report/PG及失败历史后，用全新1908 marker和隔离数据库从相同干净源码重新构建、重跑。两轮数字不相加。

最终被测源码仍为 main `71879e98411947748078cbf95c49effff7ea0740` / gateway `1ba185fdec438e18d77f032239531c6075ce0d7d`。1908原manifest在清理前已以0600私有原字节归档，逐字节相等，SHA256 `4098ab8a0665f7c2b1743d7315c4d78800608f47fbbb10e3f830a0e8f88d1903`；敏感路径不入库。两仓source_clean、launcher-built和source_provenance_verified均成立。官方Python3.55.1与JavaScript7.60.0分别62项，一次完整124/124 PASS，零失败或跳过，10个所选协议gate通过。原始report SHA256 `93f781c694def1b6f3043a38dee3bb7f9474aa62d99075fbdac5cf6a133908c3`。

1908 PostgreSQL只读快照固定于 `2026-11-20T11:14:39.235252Z`：66/66要求关联的案例通过；164条usage无重复，总实际成本0.04124，余额从100到99.95876精确吻合。公共账本24个upload和6个recovery全成功，14个inline operation全成功，10个Stored记录为内容和预算均已释放的删除墓碑；全部活动upload/cleanup/binding/read/inline/stored租约为0。6个原始未到期cleanup token仍作为已完成清理的幂等围栏完整保留；16组failure/incomplete/annotations的外层请求ID→client ID→usage精确桥接，不以每group计数代替。协议、来源和实际账本合并的脱敏证据为 `docs/verification/bps-sdk-20261120-1908.json`；独立两轴最终复核仍以其冻结文件为准。

URL使用另一个1858隔离实例补验，不混入124矩阵：官方双SDK各完成纯URL、URL+公共file ID+inline混合、私网URL400拒绝，合计6个步骤通过。RFC2606正文实际下载并上传8008bytes，SHA256 `b6869c8984701701bc2e6973b6ffc750d497f845cc1a65a106e9301590a13ab0`，每SDK原生4次上传和2次生成，按原始输入顺序精确核对native IDs。a2范围PG关联2个公共文件/2个public operation、4个released inline request/6个inline operation及4条usage，活动租约为0；该实例全账本（含a1诊断）的实际成本0.0015，余额99.9985。原始manifest与0600归档逐字节一致。独立补验和PG证据为 `docs/verification/bps-url-sdk-20261120-1858.json`。

URL历史诊断完整保留：1836额外请求因native夹具330MiB留存加200MiB预留超过512MiB总量而507→502→503；1858 a1请求实际成功，但补验脚本误认上传传输顺序，修正为验证最终文档顺序后在独立a2 key范围重跑。没有改生产下载器、放宽SSRF/容量或抹去a1残留来制造通过。URL仅覆盖本次JSON、指定公共文本源与一个私网地址拒绝，不代表任意URL/SSE/全部SSRF分支或真实BPS理解。

本次证据不验证物理文件系统清理、未来TTL sweep或两个Files DELETE案例的逐例PG关联。Stored仍为本地纯文本生命周期；客户端auto tool_search不等于网页搜索；同身份metadata recovery不等于跨后端迁移。required/forced/allowed_tools、工具来源文件、previous_response_id及跨后端迁移仍有实质缺口；真实BPS文档、搜索、产物与远端删除门未通过。未执行真实BPS、推送、部署、生产配置变更或Issue关闭。


## 2026-11-20 Pixcook 长期集成与自有镜像发布

- 基于 Tibo `30282b32f` 合入 BPS `b9025b3d3`，集成 merge `48bb3b5`；保留双方 journal。
- 稳定上游固定 `v0.2.23-klno.1` / `249125a7737999da2fbe561718b76aaaf189b753`，已是候选祖先；baseline JSON 记录实际来源。
- 同步改为普通 merge 候选 PR，保留本仓 .github，仅更新来源；不推原始上游分支，防止携带的工作流被触发；不 force/reset/main 回写。
- 发布仅接受 Pixcook 版本，固定源码 SHA、自有镜像标签及 digest；默认 dry-run，CI 先验证 unit/integration/race/frontend 和发布/同步/部署契约。
- 新增 image 托管更新模式，拒绝继承 KlN 二进制更新/回滚；独立部署及既有应用覆盖分开，既有部署禁止顺带改 PG/卷/密钥。
- 本地发布工具15项、同步7项、部署11项通过；actionlint/Bash语法/diff检查通过。完整合并后unit在运行，其余集成验收与远程dry-run尚待记录；不把旧Tibo/BPS单独结果当组合证据。
- 生产未切换；自动同步缺专用 SYNC_GITHUB_TOKEN，不能声明定时同步已启用。具体部署、备份和质量隔离回滚见 docs/pixcook-release.md。

- 合并后完整unit：33547 pass/60 skip，失败仅StoredResponses两处顶层测试（含子测试4项）。实时授权新增GroupRepository，原夹具缺失；仅为两处安装当前分组snapshot，不改变生产检查。完整handler重验中。远程普通CI另有继承代码lint失败，完整日志私有保存在/tmp/pixcook-ci-44737020101.log；发布前仍需清理并复核，未宣称全绿。

- 最新窄验证：handler2579pass/1skip，Tibo/BPS/隔离/更新保护race284pass，真实PG88pass。远端首轮dry-run前端、来源、契约、linux-amd64归档通过，backend因已修复旧夹具失败而正确阻止release。
- CI揭示WebP可达漏洞GO-2026-6222/5061/4961，官方vuln.go.dev确认x/image0.45.0统一修复，已最小升级（不tidy）。同时清理errcheck、等价静态规则与无调用遗留私有函数；私有文件root仅operator配置，保留no-follow/descriptor围栏并注明G703边界。最终修改后必须重验。

- 最新源码e1c1c3c03安全扫描通过；最新完整门禁dry-run44743498402运行中。部署无生产写入，自有镜像尚未发布；定时同步专用token仍缺失。

- 最终候选5cc87fa8b远端dry-run44757345587全部通过（unit/integration/race/lint/security/frontend/OCI）。本地完整integration19906pass/35skip，最终受影响1410pass/1skip。自动同步仍缺专用token；未发布镜像、未生产切换。详细记录见任务文档。

- 正式发布v0.2.23-pixcook.1/44771988597全部通过，固定digest见docs/verification/pixcook-release-20261121.json。GHCR匿名401且CLI缺read:packages，服务器拉取待配置；SYNC专用token仍缺。生产未切换。

## 2026-11-21 — 镜像拉取与自动同步配置完成

- 自有 GHCR sub2api 包由 private 改为 public；组织 Public package creation 仅在变更期间临时开启，完成后恢复禁用。匿名 OCI manifest GET 返回200，内容SHA256与已发布digest一致，linux/amd64子manifest与发布artifact一致。未下载镜像层或在服务器拉取。
- 新建本仓专用 fine-grained PAT，保存为 Actions Secret `SYNC_GITHUB_TOKEN`；仅授权 pixcook-ai/sub2api 的 Contents、Pull requests、Workflows写入和必需Metadata读取，无组织权限。有效期至2027-02-19，届时需轮换；没有复制个人CLI token或在仓库记录令牌。
- GitHub fork停用门阻止首次dispatch（422），仓库permissions和workflow.state虽显示enabled/active，网页要求重新启用。恢复Actions后真实运行44854017794全部成功；artifact sync-report.json为up_to_date，上游v0.2.23-klno.1 / 249125a7737999da2fbe561718b76aaaf189b753已在pixcook中。
- 定时同步已配置为每日03:17 UTC（北京时间11:17）；本轮是手动dispatch验收，不宣称定时触发已实际发生。无新增上游变更，未创建候选PR，真实分支写入及PR CI触发仍未在本轮执行。
- 测试环境、生产及旧localhost:18080实例均未切换。此前凭证缺失和匿名401为历史状态，由本节与更新后的verification JSON替代。
