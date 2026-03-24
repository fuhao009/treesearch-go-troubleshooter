# examples/benchmark/

## Overview
Benchmark harnesses for document retrieval and code retrieval; produces local outputs under ignored directories.

## Where To Look
| Task | Location | Notes |
|------|----------|-------|
| QASPER benchmark | `examples/benchmark/qasper_benchmark.py` | document retrieval evaluation |
| CodeSearchNet benchmark | `examples/benchmark/codesearchnet_benchmark.py` | code retrieval evaluation |
| Shared helpers | `examples/benchmark/benchmark_utils.py`, `examples/benchmark/metrics.py` | dataset loading, metrics |

## Commands
```bash
python examples/benchmark/qasper_benchmark.py --help
python examples/benchmark/codesearchnet_benchmark.py --help
```

## Notes
- Outputs are gitignored: see `.gitignore` (`benchmark_results/`, `examples/benchmark/indexes/`).
