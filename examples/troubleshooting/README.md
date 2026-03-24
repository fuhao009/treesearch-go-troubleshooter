# 单 Go 二进制离线故障排查方案

现在这套主路径已经改成一个 Go 二进制完成全部能力，不再依赖 Python `treesearch_service.py`。

主程序是：

- `examples/troubleshooting/go_walker`

它现在既能做索引，也能起 Gin HTTP 服务，还能直接执行 runbook、回写经验、导入导出经验。
最新版本还支持后台持续诊断：周期性探针、多树路由、经验沉淀、自动生成新树。

## 10 秒看懂

```text
探针/定时任务
   |
   v
go_walker serve
   |
   +--> 导入 playbook
   +--> 读取探针输出
   +--> TreeSearch 选多棵树
   +--> 执行 tsdiag 动作块
   +--> 保存经验 JSON
   +--> 回写 SQLite / FTS
   `--> 自动生成新树再入库
```

一句话：以前是 `Python TreeSearch + Go 执行单元`，现在收敛成 `一个 Gin 承载的 Go 服务进程`。

## 目录

```text
examples/troubleshooting/
|-- README.md
|-- AGENTS.md
|-- go_walker/
|   |-- go.mod
|   |-- go.sum
|   |-- daemon.go
|   |-- main.go
|   `-- server.go
|-- playbooks/
|   |-- host_anomaly_runbook.md
|   |-- categraf_evidence_chain_playbook.md
|   |-- categraf_performance_radar_playbook.md
|   `-- incident_record_template.md
|-- indexes/      # 运行期生成，gitignore
`-- records/      # 运行期生成，gitignore
```

历史参考代码还在，但已经不是主路径：

- `examples/troubleshooting/treesearch_service.py`
- `examples/troubleshooting/executor_unit/`

## 这个二进制现在做什么

`go_walker` 现在包含 7 类能力：

- 导入 Markdown/JSON 到 SQLite
- 用 SQLite FTS5 搜索文档树
- 按 `步骤 N` 提取可执行步骤
- 执行 fenced `tsdiag/diag/godiag` 内建动作块
- 把执行结果写成经验 JSON，并重新索引回知识库
- 后台周期性执行探针和诊断任务
- 从重复经验自动生成新的 troubleshooting tree

## 核心 API

同一个服务进程里提供这些接口：

- `GET /api/v1/health`
- `GET /api/v1/documents`
- `POST /api/v1/playbooks/import`
- `POST /api/v1/experience/import`
- `POST /api/v1/search`
- `GET /api/v1/doc/<doc_id>`
- `POST /api/v1/executions/result`
- `GET /api/v1/experience/export`
- `POST /api/v1/run`
- `GET /api/v1/daemon/status`
- `GET /api/v1/daemon/jobs`
- `POST /api/v1/daemon/jobs`
- `GET /api/v1/daemon/jobs/<job_id>`
- `POST /api/v1/daemon/jobs/<job_id>/start`
- `POST /api/v1/daemon/jobs/<job_id>/stop`
- `POST /api/v1/daemon/jobs/<job_id>/run`

其中：

- `/api/v1/search`：找相关文档和节点
- `/api/v1/doc/<doc_id>`：返回结构化步骤和动作块
- `/api/v1/run`：直接执行整份手册
- `/api/v1/executions/result`：外部系统也可以主动回写结果
- `/api/v1/experience/import` / `/api/v1/experience/export`：导入导出经验
- `/api/v1/daemon/*`：常驻任务管理、状态查看、手动触发

服务实现补充：

- HTTP 层使用 Gin
- 守护进程和 API 在同一个二进制里
- 收到 `SIGTERM` / `SIGINT` 时会优雅停止 HTTP 服务和后台调度

## 构建

```bash
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go build
```

如果你想自定义产物名：

```bash
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec \
  go build -o tsdiag
```

## 启动

```bash
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec \
  ./go_walker serve \
  --listen 127.0.0.1:19065 \
  --db ../indexes/service.db \
  --record-dir ../records \
  --generated-dir ../records/generated_trees \
  --scheduler-interval 5s
```

## 端到端使用

### 1. 导入 playbook

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/playbooks/import \
  -H 'Content-Type: application/json' \
  -d '{
    "paths": ["/Users/fuhaoliang/TreeSearch/examples/troubleshooting/playbooks/*.md"],
    "force": true
  }'
```

### 2. 搜索

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/search \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "iowait 热点进程 容器重启",
    "top_k_docs": 5,
    "max_nodes_per_doc": 5
  }'
```

### 3. 取某个 runbook 的步骤

```bash
curl -s http://127.0.0.1:19065/api/v1/doc/host_anomaly_runbook
```

返回里最关键的是：

- `structure`：原始文档树
- `steps`：提取出的步骤
- `steps[].actions`：当前步骤中的可执行 `tsdiag` 动作块

### 4. 直接执行整份手册

按文档 ID 执行：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/run \
  -H 'Content-Type: application/json' \
  -d '{
    "doc_id": "host_anomaly_runbook",
    "timeout_seconds": 10,
    "metadata": {
      "trigger": "manual_test"
    }
  }'
```

按 query 执行：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/run \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "主机异常 总流程",
    "timeout_seconds": 10
  }'
```

### 5. 导出经验

```bash
curl -s 'http://127.0.0.1:19065/api/v1/experience/export?limit=10'
```

导出指定记录：

```bash
curl -s 'http://127.0.0.1:19065/api/v1/experience/export?record_id=manual_import_demo'
```

### 6. 创建后台持续诊断任务

按 query 自动选树：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/daemon/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "job_id": "host_continuous_demo",
    "name": "host-continuous-demo",
    "query": "主机异常 热点进程 容器重启",
    "probe_text": "iowait cpu load process restart",
    "interval_seconds": 30,
    "timeout_seconds": 10,
    "max_docs_per_cycle": 3,
    "min_records_to_generate": 2,
    "generation_window": 10,
    "enabled": true
  }'
```

按固定 `doc_ids` 覆盖多棵树：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/daemon/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "job_id": "fixed_multi_tree_demo",
    "name": "fixed-multi-tree-demo",
    "doc_ids": [
      "host_anomaly_runbook",
      "categraf_evidence_chain_playbook",
      "categraf_performance_radar_playbook"
    ],
    "probe_text": "host anomaly categraf container io",
    "interval_seconds": 30,
    "timeout_seconds": 10,
    "max_docs_per_cycle": 3,
    "enabled": true
  }'
```

查看任务状态：

```bash
curl -s http://127.0.0.1:19065/api/v1/daemon/status
curl -s http://127.0.0.1:19065/api/v1/daemon/jobs/fixed_multi_tree_demo
```

手动触发一轮：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/daemon/jobs/fixed_multi_tree_demo/run
```

### 7. 导入经验

直接导入一条 JSON 经验：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/experience/import \
  -H 'Content-Type: application/json' \
  -d '{
    "record": {
      "record_id": "manual_import_demo",
      "source_doc_id": "host_anomaly_runbook",
      "source_doc_name": "Linux 主机异常排查总流程",
      "summary": {
        "status": "ok"
      },
      "steps": []
    }
  }'
```

也可以按路径导入已有 JSON 文件：

```bash
curl -s -X POST http://127.0.0.1:19065/api/v1/experience/import \
  -H 'Content-Type: application/json' \
  -d '{
    "paths": ["/Users/fuhaoliang/TreeSearch/examples/troubleshooting/records/*.json"]
  }'
```

## 自我完善怎么实现

现在的“自我完善”不靠 Python，也不靠第三个组件，而是靠这个闭环：

1. `go_walker serve` 导入 runbook
2. 后台任务周期性运行探针
3. 探针输出交给 TreeSearch 选择一棵或多棵树
4. 执行结果写成 `records/*.json`
5. 同时把经验重新写回 SQLite / FTS
6. 从重复经验聚合出 `generated_*.md` 自动树
7. 自动树重新入库，后续可搜索、可导出、可人工接管

所以它是一个离线知识闭环，而不是一个额外的 AI 编排系统。

## 手册约定

- 每个 `## 步骤 N` 会被识别成一个步骤节点
- 动作必须放在 fenced code block 里
- 支持的语言标签：`tsdiag`、`diag`、`godiag`
- 经验记录建议保留 `source_doc_id`、`source_doc_name`、`summary`、`steps`
- 后台任务如果要覆盖多棵树，推荐优先使用 `doc_ids`
- 自动生成树会写入 `records/generated_trees/`

## 当前实现细节

- 主路径已经是纯 Go，不依赖 Python 运行时
- SQLite 通过 `modernc.org/sqlite` 接入
- 运行时只执行 Go 内建采集动作，不调用 shell，不调用 Linux 命令
- 查询执行时会优先选到真正包含可执行动作块的文档
- 执行产物会同时落成 JSON 文件和 SQLite 可搜索文档
- 常驻任务会把探针步骤和路由步骤也写进经验记录
- 自动树默认不会再被后台任务递归拿来继续生成自动树

## 已验证

已经验证通过的链路：

- `go build`
- `serve`
- `playbooks/import`
- `search`
- `doc/<id>`
- `run`
- `experience/import`
- `experience/export`
- `daemon/jobs`
- `daemon/status`
- 单轮多树执行：`host_anomaly_runbook`、`categraf_evidence_chain_playbook`、`categraf_performance_radar_playbook`
- 自动树生成：`generated_host_anomaly_runbook`、`generated_categraf_evidence_chain_playbook`

当前运行时已经改成纯 Go 内建动作执行，新的经验记录会统计 `executed_actions` 和 `failed_actions`，不再依赖 shell 或 Linux 命令。
