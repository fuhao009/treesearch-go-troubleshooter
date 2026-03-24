# PROJECT KNOWLEDGE BASE

Generated: 2026-03-12
Commit: 9c8cd4b
Branch: main

## Overview
TreeSearch (package name: `pytreesearch`) is a Python 3.10+ structure-aware document retrieval library: parse docs/code into trees, then search via SQLite FTS5 (plus exact grep-style prefilter for code).

## Structure
```
./
|-- treesearch/           # library code (core engine, indexer, FTS, parsers, CLI)
|-- tests/                # pytest suite
|-- examples/             # runnable demos
|   |-- troubleshooting/  # offline troubleshooting runtime: single Go binary service
|   `-- benchmark/        # benchmark harnesses
|-- docs/                 # architecture + API docs
`-- .github/workflows/    # CI
```

## Where To Look
| Task | Location | Notes |
|------|----------|-------|
| High-level API (`TreeSearch`) | `treesearch/treesearch.py` | wraps indexing + search over a single SQLite DB |
| CLI entry point | `treesearch/cli.py` | implements `treesearch index` / `treesearch search` |
| `python -m treesearch` entry | `treesearch/__main__.py` | calls `treesearch.cli.main()` |
| Unified search pipeline | `treesearch/search.py` | `search()` + `PreFilter` protocol + `GrepFilter` |
| SQLite FTS5 engine | `treesearch/fts.py` | schema, BM25 weights, incremental metadata |
| Index/build trees | `treesearch/indexer.py` | `build_index()`, `md_to_tree()`, `text_to_tree()`, `code_to_tree()` |
| Data model + tree utils | `treesearch/tree.py` | `Document`, traversal, persistence |
| Parser routing/registry | `treesearch/parsers/registry.py` | extension -> parser; prefilter chain routing |
| Public API exports | `treesearch/__init__.py` | single import surface + sqlite3/FTS5 compatibility swap |
| Troubleshooting all-in-one Go entry | `examples/troubleshooting/go_walker/main.go` | CLI + `serve` mode for the single-binary runtime |
| Troubleshooting Go HTTP service | `examples/troubleshooting/go_walker/server.go` | Gin daemon API, graceful shutdown, import/search/doc/run/result/export/daemon APIs |
| Troubleshooting continuous scheduler | `examples/troubleshooting/go_walker/daemon.go` | probe loop, multi-tree routing, auto-tree generation |
| Troubleshooting playbooks | `examples/troubleshooting/playbooks/` | markdown runbooks for host anomaly / evidence_chain / performance_radar |
| Project docs | `docs/architecture.md`, `docs/api.md` | module overview + API signatures |

## AGENTS Hierarchy (Complexity-Scored)
```
./AGENTS.md
|-- treesearch/AGENTS.md            # score ~29 (core engine + biggest modules)
|-- treesearch/parsers/AGENTS.md    # score ~17 (routing + optional parsers)
|-- tests/AGENTS.md                 # score ~14 (async + env isolation)
|-- examples/AGENTS.md              # score ~9  (runnable demos + sample data)
|   `-- examples/troubleshooting/AGENTS.md   # score ~18 (single-binary offline troubleshooting flow)
`-- examples/benchmark/AGENTS.md    # score ~13 (benchmark harness)
```

## Conventions (Project-Specific)
- Packaging: `pyproject.toml` uses setuptools; CLI script `treesearch = treesearch.cli:main`.
- Tests: pytest; config in `pyproject.toml` (`testpaths=["tests"]`, `asyncio_mode="auto"`).
- Optional deps: extras in `pyproject.toml` (`jieba`, `pdf`, `docx`, `html`, `fts5`, `treesitter`, `all`, `dev`).
- Generated artifacts are not tracked: see `.gitignore` for `indexes/`, `*.db`, `benchmark_results/`.
- Troubleshooting demo primary path is now a single Go binary service under `examples/troubleshooting/go_walker/`.

## Anti-Patterns (This Project)
- Never use real API keys in CI/tests; mock LLM calls: `CONTRIBUTING.md`.
- Optional tools are never auto-enabled; users must opt in: `examples/data/markdowns/agent-tools.md`.
- Tree-sitter language loader uses a deprecated API (suppressed warning): `treesearch/parsers/treesitter_parser.py`.
- For `examples/troubleshooting/go_walker`, do not assume the shell `go` command is usable as-is on this machine; local Go env may contain a stale `GOROOT`.

## Commands
```bash
# Install (dev)
pip install -e .

# Run tests (local)
python -m pytest tests/ -v

# CI-style tests (coverage)
python -m pytest tests/ -v --tb=short --cov=treesearch --cov-report=term-missing

# CLI
treesearch --help
treesearch index --paths "docs/*.md" --add-description
treesearch search --index_dir ./indexes/ --query "How does auth work?"

# Module entry
python -m treesearch --help

# Benchmarks (see examples/benchmark/AGENTS.md)
python examples/benchmark/qasper_benchmark.py --help
python examples/benchmark/codesearchnet_benchmark.py --help

# Troubleshooting demo (single binary)
cd examples/troubleshooting/go_walker
env GOENV=/dev/null GOROOT=/usr/local/Cellar/go/1.24.6/libexec go build
./go_walker serve --listen 127.0.0.1:19065 --db ../indexes/service.db --record-dir ../records
curl -s -X POST http://127.0.0.1:19065/api/v1/playbooks/import -H 'Content-Type: application/json' -d '{"paths":["/Users/fuhaoliang/TreeSearch/examples/troubleshooting/playbooks/*.md"],"force":true}'
curl -s -X POST http://127.0.0.1:19065/api/v1/run -H 'Content-Type: application/json' -d '{"doc_id":"host_anomaly_runbook","timeout_seconds":10}'
curl -s 'http://127.0.0.1:19065/api/v1/experience/export?limit=10'
```

## Notes
- There is a root `AGENT.md` (singular) with dev notes/history; treat its architecture list as historical (some modules referenced there no longer exist).
- New troubleshooting capability added under `examples/troubleshooting/`:
  - `go_walker serve` is the primary runtime path and exposes import/search/doc/run/result/export/daemon APIs
  - the HTTP layer is now Gin-based but remains a single offline Go process
  - the service is fully Go-based and does not require the Python TreeSearch service in the main path
  - the runtime executes pure-Go `tsdiag` built-in collectors only; it does not invoke shell or Linux commands
  - experience records are stored as JSON and reindexed into the same SQLite DB for later search
  - daemon jobs can continuously run probes, route across multiple playbooks, and generate new troubleshooting trees
  - the full 2026-03-24 implementation / smoke test / remote deployment / pure-Go action-runtime operation log is recorded in `examples/troubleshooting/AGENTS.md`
