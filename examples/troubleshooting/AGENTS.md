# examples/troubleshooting/

## Overview
Primary runtime is now a single Go binary under `examples/troubleshooting/go_walker/`.

That binary is the production path for this subtree:

- import playbooks
- search documents
- return structured steps
- execute pure-Go `tsdiag` action blocks
- store experience JSON
- reindex experience back into SQLite/FTS
- run continuous daemon jobs
- generate new troubleshooting trees from repeated experience

Historical references still exist but are no longer the main path:

- `examples/troubleshooting/treesearch_service.py`
- `examples/troubleshooting/executor_unit/`

## Where To Look
| Task | Location | Notes |
|------|----------|-------|
| All-in-one Go entry | `examples/troubleshooting/go_walker/main.go` | CLI entry; now includes `serve` mode |
| HTTP service layer | `examples/troubleshooting/go_walker/server.go` | Gin-based daemon API, graceful shutdown, import/search/doc/run/result/export |
| Continuous scheduler | `examples/troubleshooting/go_walker/daemon.go` | daemon jobs, multi-tree routing, probe/action loop, auto-tree generation |
| Built-in collectors | `examples/troubleshooting/go_walker/builtin_actions.go` | pure-Go host/process/fs/net collectors; no shell execution |
| Go module | `examples/troubleshooting/go_walker/go.mod` | uses `modernc.org/sqlite` for pure-Go SQLite |
| Usage doc | `examples/troubleshooting/README.md` | single-binary startup and API examples |
| Host runbook | `examples/troubleshooting/playbooks/host_anomaly_runbook.md` | executable `tsdiag` action blocks |
| `evidence_chain` runbook | `examples/troubleshooting/playbooks/categraf_evidence_chain_playbook.md` | executable `tsdiag` action blocks |
| `performance_radar` runbook | `examples/troubleshooting/playbooks/categraf_performance_radar_playbook.md` | executable `tsdiag` action blocks |
| Record template | `examples/troubleshooting/playbooks/incident_record_template.md` | manual writeup template |

## Core Flow
1. `go_walker serve` starts a single offline Gin daemon service.
2. `POST /api/v1/playbooks/import` writes Markdown playbooks into SQLite document-tree + FTS tables.
3. `POST /api/v1/search` and `GET /api/v1/doc/<id>` expose retrieval and structured steps.
4. `POST /api/v1/run` executes a single tree immediately.
5. `/api/v1/daemon/jobs*` manages continuous jobs that run probes, route across multiple trees, and execute them.
6. Execution results are stored as JSON files and reindexed into SQLite as searchable experience.
7. Repeated experience is aggregated into `generated_*.md` trees and reindexed back into SQLite.

## Conventions (Local)
- Treat `go_walker` as the primary implementation path for this subtree.
- Keep the runtime fully offline and Go-first.
- Keep the API layer in the same single-binary Gin process as the scheduler.
- Keep API import/export compatibility for playbooks and experience.
- Playbooks should keep `## 步骤 N` headings so step extraction stays stable.
- Executable content belongs in fenced `tsdiag`, `diag`, or `godiag` blocks.
- Runtime artifacts belong in gitignored paths such as `examples/troubleshooting/indexes/` and `examples/troubleshooting/records/`.
- Continuous jobs should prefer original playbooks; auto-generated trees are for knowledge growth, not recursive daemon input.
- If you need a multi-tree demo, prefer configuring `doc_ids` explicitly.

## Anti-Patterns (Local)
- Do not make Python `treesearch_service.py` the primary path again unless explicitly requested.
- Do not split the new main path back into multiple required runtimes.
- Do not keep execution results only in memory or logs; they must be written to JSON and reindexed.
- Do not let query-based execution prefer a non-runnable experience document when a runnable playbook exists.
- Do not let daemon auto-generation recurse into `generated_generated_*`.
- Do not assume the shell `go` command works on this machine without the explicit local `GOROOT` override.

## Commands
```bash
# Build the single Go binary
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go build

# Start the all-in-one service
./go_walker serve \
  --listen 127.0.0.1:19065 \
  --db ../indexes/service.db \
  --record-dir ../records \
  --generated-dir ../records/generated_trees \
  --scheduler-interval 5s

# Import playbooks
curl -s -X POST http://127.0.0.1:19065/api/v1/playbooks/import \
  -H 'Content-Type: application/json' \
  -d '{"paths":["/Users/fuhaoliang/TreeSearch/examples/troubleshooting/playbooks/*.md"],"force":true}'

# Execute a runbook directly on the same service
curl -s -X POST http://127.0.0.1:19065/api/v1/run \
  -H 'Content-Type: application/json' \
  -d '{"doc_id":"host_anomaly_runbook","timeout_seconds":10}'

# Create a fixed multi-tree daemon job
curl -s -X POST http://127.0.0.1:19065/api/v1/daemon/jobs \
  -H 'Content-Type: application/json' \
  -d '{"job_id":"fixed_multi_tree_demo","doc_ids":["host_anomaly_runbook","categraf_evidence_chain_playbook","categraf_performance_radar_playbook"],"probe_text":"host anomaly categraf container io","interval_seconds":30,"timeout_seconds":10,"max_docs_per_cycle":3,"enabled":true}'

# Export recent experience
curl -s 'http://127.0.0.1:19065/api/v1/experience/export?limit=10'
```

## Verified Behavior
- `go build` succeeds with the explicit `GOENV` / `GOROOT` override.
- `serve` starts successfully and exposes health/import/search/doc/run/export/daemon APIs.
- `serve` now runs behind Gin and shuts down cleanly on `SIGTERM` / `SIGINT`.
- Playbooks can be imported over HTTP and returned as structured steps with executable `tsdiag` action blocks.
- `/api/v1/run` executes pure-Go action blocks and persists outputs back into JSON + SQLite.
- `/api/v1/experience/import` and `/api/v1/experience/export` both work in the single-binary path.
- `/api/v1/daemon/jobs` can drive continuous probe -> multi-tree route -> execute -> reindex cycles.
- Auto-generated trees are written under `records/generated_trees/` and indexed as `source_type=generated`.

## 2026-03-24 Operation Log

This section records the exact implementation / verification / deployment operations that were performed for the continuous offline troubleshooting daemon.

### 1. Gin migration
- Replaced the `net/http` mux entry path in `examples/troubleshooting/go_walker/server.go` with a Gin engine.
- Added `buildGinEngine(...)` and registered all existing API routes through Gin.
- Kept the existing handler functions and business logic unchanged by reusing them with `gin.WrapF(...)`.
- Added graceful shutdown with `signal.NotifyContext(...)`, `SIGTERM`, `SIGINT`, and `http.Server.Shutdown(...)`.
- Added Gin dependency in `examples/troubleshooting/go_walker/go.mod`.

### 2. Local dependency and build commands
```bash
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go get github.com/gin-gonic/gin@v1.10.1
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go mod tidy
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go build ./...
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go build -o /tmp/go_walker_local
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec GOOS=linux GOARCH=amd64 go build -o /tmp/go_walker_linux_amd64
```

### 3. Local smoke test operations
```bash
tmpdir=$(mktemp -d /tmp/go_walker_smoke.XXXXXX)

/tmp/go_walker_local serve \
  --listen 127.0.0.1:19066 \
  --db "${tmpdir}/service.db" \
  --record-dir "${tmpdir}/records" \
  --generated-dir "${tmpdir}/generated" \
  --scheduler-interval 1s

curl -s -X POST http://127.0.0.1:19066/api/v1/playbooks/import \
  -H 'Content-Type: application/json' \
  -d '{"paths":["/Users/fuhaoliang/TreeSearch/examples/troubleshooting/playbooks/*.md"],"force":true}'

curl -s http://127.0.0.1:19066/api/v1/health
curl -s http://127.0.0.1:19066/api/v1/documents

curl -s -X POST http://127.0.0.1:19066/api/v1/daemon/jobs \
  -H 'Content-Type: application/json' \
  -d '{"job_id":"smoke_job","name":"smoke_job","query":"cpu load io categraf","doc_ids":["host_anomaly_runbook","categraf_evidence_chain_playbook"],"interval_seconds":30,"timeout_seconds":5,"probe_text":"host anomaly categraf","probe_merge_mode":"replace","enabled":true,"auto_generate_tree":true,"generation_window":3,"min_records_to_generate":2}'

curl -s http://127.0.0.1:19066/api/v1/daemon/jobs/smoke_job
curl -s 'http://127.0.0.1:19066/api/v1/experience/export?limit=4'
```

### 4. Local smoke test observations
- The Gin daemon started successfully and emitted Gin request logs.
- Playbook import, health, document listing, daemon job creation, daemon job status, and experience export all succeeded.
- The scheduler automatically picked up `smoke_job` and increased `run_count` without manual retriggering.
- In the later pure-Go collector runtime, Linux host evidence is gathered from built-in collectors instead of shell commands.
- Platform differences remain limited to data availability, for example `/proc`-backed collectors only produce full output on Linux hosts.
- Those collector-level differences do not block the main pipeline: daemon scheduling, execution recording, JSON persistence, and SQLite reindex still work.

### 5. Remote deployment target
- Host: `root@10.33.0.50`
- Binary path: `/root/tsdiag/bin/go_walker`
- DB path: `/root/tsdiag/indexes/service.db`
- Records path: `/root/tsdiag/records`
- Generated trees path: `/root/tsdiag/records/generated_trees`
- Log path: `/root/tsdiag/logs/service.log`
- Listen address: `127.0.0.1:29065`
- Important note: this remote runtime is not managed by `systemd`; it runs as a direct long-lived process.

### 6. Remote deployment operations
```bash
scp /tmp/go_walker_linux_amd64 root@10.33.0.50:/root/tsdiag/bin/go_walker.new

ssh root@10.33.0.50 '
set -e
stamp=$(date +%Y%m%d%H%M%S)
cp /root/tsdiag/bin/go_walker /root/tsdiag/bin/go_walker.bak_${stamp}
install -m 0755 /root/tsdiag/bin/go_walker.new /root/tsdiag/bin/go_walker
old_pid=$(pgrep -f "^/root/tsdiag/bin/go_walker serve --listen 127.0.0.1:29065" || true)
if [ -n "$old_pid" ]; then
  kill -TERM "$old_pid"
fi
for _ in $(seq 1 20); do
  if ! pgrep -f "^/root/tsdiag/bin/go_walker serve --listen 127.0.0.1:29065" >/dev/null; then
    break
  fi
  sleep 1
done
nohup /root/tsdiag/bin/go_walker serve \
  --listen 127.0.0.1:29065 \
  --db /root/tsdiag/indexes/service.db \
  --record-dir /root/tsdiag/records \
  --generated-dir /root/tsdiag/records/generated_trees \
  --scheduler-interval 2s \
  >> /root/tsdiag/logs/service.log 2>&1 < /dev/null &
'
```

### 7. Remote verification operations
```bash
ssh root@10.33.0.50 'ps -ef | grep [g]o_walker'
ssh root@10.33.0.50 'curl -s http://127.0.0.1:29065/api/v1/health'
ssh root@10.33.0.50 'curl -s http://127.0.0.1:29065/api/v1/daemon/jobs/fixed_multi_tree_remote'
ssh root@10.33.0.50 "curl -s 'http://127.0.0.1:29065/api/v1/experience/export?limit=1'"
ssh root@10.33.0.50 'tail -n 20 /root/tsdiag/logs/service.log'
```

### 8. Remote verification results
- The new process was relaunched successfully and later observed as PID `19660`.
- `/root/tsdiag/logs/service.log` showed Gin access logs with `[GIN]`, confirming that the deployed process was the Gin-based binary.
- `GET /api/v1/health` returned `status=ok`.
- Existing daemon job `fixed_multi_tree_remote` was automatically loaded from SQLite on restart.
- That job resumed normal scheduling after restart and continued cycling between `running` and `waiting`.
- A verified post-restart state was:
  - `job_id=fixed_multi_tree_remote`
  - `state=waiting`
  - `last_run_at=2026-03-24T06:34:04Z`
  - `next_run_at=2026-03-24T06:35:04Z`
  - `run_count=17`
- Experience export after restart returned new records, confirming that execution -> JSON persistence -> SQLite export still worked on the remote host.

### 9. 2026-03-24 benign pipeline exit normalization
Superseded by the later pure-Go action runtime. Kept only as a historical deployment milestone before shell execution was fully removed.

### 10. 2026-03-24 pure-Go action runtime收口
- Goal: remove all runtime shell / Linux command execution from `go_walker`; keep a single offline Go binary with TreeSearch + execution loop + Gin daemon API.
- Code changes:
  - Added `examples/troubleshooting/go_walker/builtin_actions.go` as the pure-Go collector engine.
  - Collectors now read directly from `/proc`, `/proc/net/*`, `/proc/self/mounts`, `syscall.Statfs`, and process metadata APIs.
  - Supported collectors include:
    - `host_identity`
    - `proc_uptime`
    - `scheduler_overview`
    - `memory_overview`
    - `filesystem_overview`
    - `network_overview`
    - `top_processes`
    - `process_match`
    - `recent_process_starts`
    - `open_file_overview`
  - `main.go` action parser now only accepts fenced `tsdiag` / `diag` / `godiag` blocks.
  - Action execution is now `executeActionBlock(...)` and stores `engine=go_builtin` + structured `spec`.
  - HTTP / record payloads now use `actions` instead of `commands`.
  - Daemon jobs now use `probe_action` and `probe_text` instead of `probe_command`.
  - Backward compatibility is kept for old `commands` and `probe_command` payloads; they are normalized on read and no longer emitted on new writes.
- Playbook changes:
  - Converted core runbooks under `examples/troubleshooting/playbooks/` from shell blocks to `tsdiag` JSON action blocks.
  - Auto-generated trees continue to be written as `generated_*.md`, but their recommended execution content is now also `tsdiag`.
- Validation:
```bash
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go test ./...
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec GOOS=linux GOARCH=amd64 go build -o /tmp/go_walker_linux_amd64
```
- Result:
  - runtime path no longer depends on shell execution
  - new records report `executed_actions` / `failed_actions`
  - daemon pipeline remains continuous and offline
  - old data can still be imported and read

### 11. 2026-03-24 deployment on `10.33.2.49`
- Goal: deploy the same pure-Go offline troubleshooting service to `root@10.33.2.49` with `systemd` management, imported runbooks, and a continuous multi-tree daemon job.
- Access path:
  - direct SSH from the local workstation to `10.33.2.49:22` timed out
  - deployment was completed through the reachable jump host `root@10.33.0.50`
- Installed layout on `10.33.2.49`:
  - binary: `/root/tsdiag/bin/go_walker`
  - playbooks: `/root/tsdiag/playbooks/*.md`
  - db: `/root/tsdiag/indexes/service.db`
  - records: `/root/tsdiag/records`
  - generated trees: `/root/tsdiag/records/generated_trees`
  - service unit: `/etc/systemd/system/tsdiag.service`
- Important host-specific note:
  - `10.33.2.49` has `net.ipv4.ip_local_port_range = 10000 61000`
  - local client traffic repeatedly left `TIME_WAIT` entries on ports like `29065`, `29067`, and `29999`
  - because of that, `go_walker serve --listen 127.0.0.1:<port>` failed with `bind: address already in use` on any listen port inside that ephemeral range
  - the final stable listen address on this host is `127.0.0.1:62065`, which is outside the ephemeral range
- Deployment result:
  - `tsdiag.service` is enabled and running on `10.33.2.49`
  - health endpoint: `http://127.0.0.1:62065/api/v1/health`
  - imported documents:
    - `host_anomaly_runbook`
    - `categraf_evidence_chain_playbook`
    - `categraf_performance_radar_playbook`
  - daemon job created and triggered:
    - `job_id=fixed_multi_tree_remote`
    - `probe_text=host anomaly categraf container io`
    - `doc_ids=[host_anomaly_runbook, categraf_evidence_chain_playbook, categraf_performance_radar_playbook]`
  - first verification state:
    - service `Active: active (running)`
    - `last_execution_status = ok`
    - `run_count = 1`
    - returned runbook steps already expose `actions` with `lang=tsdiag`

### 12. 2026-03-24 GitHub publication
- Goal: publish the offline Go troubleshooting implementation into a separate personal GitHub repository for continued development and distribution.
- Repository:
  - owner: `fuhao009`
  - repo: `treesearch-go-troubleshooter`
  - remote URL: `git@github.com:fuhao009/treesearch-go-troubleshooter.git`
- Local publish scope:
  - include the new Go runtime, playbooks, subtree `AGENTS.md`, and related documentation under `examples/troubleshooting/`
  - include repository-level `AGENTS.md` files generated for this work
  - exclude generated runtime artifacts such as exported experience, SQLite indexes, and compiled binaries
- Publish steps:
  - updated `.gitignore` to keep runtime artifacts out of version control
  - prepared a dedicated commit from local `main`
  - added a new SSH remote for the personal repository
  - pushed local `main` to the new GitHub repository
