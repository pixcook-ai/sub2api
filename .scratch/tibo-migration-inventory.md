# Tibo migration touchpoint inventory

Source: /Users/leo/Projects/pixcook-ai/sub2api-czc
Target worktree: /Users/leo/Projects/pixcook-ai/.worktrees/sub2api-tibo-20261120
Baseline: 28842f406469c0b51a9737d085935ee3fb978b70

| Area | Required touchpoints | Integration check |
| --- | --- | --- |
| Probe | openai_tibo_probe/answer; account-directed existing probe transport | same account/token/proxy, model/effort, successful SSE |
| State | openai_tibo_route/prober; concurrency/budget/holds/votes/staleness/restart | paused stays paused, unknown does not vote |
| Lifecycle | gateway state fields, start, shutdown cancellation | no orphan probe goroutines |
| Configuration | config defaults/validation plus runtime settings | optional grouping and user-selected IDs, no fixed names |
| Persistence | locked fresh sample update, durable outbox, scheduler cache projection/version | old probe/rebuild cannot replace newer quality state |
| Admin | account DTO/runtime status, private-extra redaction/edit preservation | lists/edit show last 24 points, no runtime state overwrite |
| Scheduling | health tiers, sticky/previous-response handling, optional isolation | every supported selection entrance honors chosen policy |
| Forward | plan/result observation and passive mismatch | existing transports/quality guard/identity remain compatible |
| Usage | RouteDegraded tri-state through service, all INSERT/SELECT paths, filters/stats/cache/admin DTO | NULL/false/true preserved; ordinary user DTO excludes field |
| Migration | new 260 column, 261 nontransactional partial index/retry | old migration contents/checksums unchanged |
| Frontend | sparkline, account usage/edit UI, refresh identity, admin filters/types/i18n | paused refresh and complete API contract |
| Tests | state/probe/hold, scheduling, persistence PG, admin-only usage, frontend interactions | run only after Go-cache pause lifted |

## Deliberate separation

Do not migrate the unrelated intelligent-test platform, registration dual-key behavior, Luna group model bans, deleted Cookie-WS compatibility stubs, or source Codex/BPS/file changes merely because they reference Tibo.

Source auto-grouping uses each fresh definitive raw sample, while confirmed route state uses voting. These are different behaviors and must be documented if preserved. Group migration alone is not global scheduling isolation.

## Usage observation slice

Implemented code and inherited behavior tests; Go-cache pause has been lifted; the main agent coordinates the single heavy build slot, so this slice has not run Go tests yet. Main agent owns final integration and verification. Ordinary user responses never receive route_degraded; administrator list/statistics accept optional route_degraded=true|false, where each explicit value excludes SQL NULL rows.
