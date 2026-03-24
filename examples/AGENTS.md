# examples/

## Overview
Runnable demo scripts and sample data showing typical indexing/search workflows.

## Where To Look
| Task | Location | Notes |
|------|----------|-------|
| Quick API demo | `examples/01_basic_demo.py` | minimal high-level usage |
| Build + search (API) | `examples/02_index_and_search.py` | indexing then query |
| CLI workflow demo | `examples/03_cli_workflow.py` | prints equivalent `treesearch ...` commands |
| Multi-doc search demo | `examples/04_multi_doc_search.py` | routing across multiple docs |
| Offline troubleshooting runtime | `examples/troubleshooting/` | single Go binary service for import/search/run/daemon/experience replay |
| Sample markdowns | `examples/data/markdowns/` | small documents used by demos |

## Notes
- Local outputs are gitignored: see `.gitignore` (`examples/indexes/`, `examples/results/`, `examples/*.db`).
- `examples/troubleshooting/` has its own `AGENTS.md`; use that subtree doc before changing the single-binary service or playbooks.
