# tests/

## Overview
Pytest suite validating indexing, parsing, FTS/scoring, and CLI behavior.

## Where To Look
| Area | Location | Notes |
|------|----------|-------|
| Test environment isolation | `tests/conftest.py` | strips `TREESEARCH_*` and `OPENAI_*`; resets global config + FTS cache |
| CLI tests | `tests/test_cli.py` | parser args + DB loading behavior |
| Search tests | `tests/test_search.py` | multi-doc scoring/ranking behaviors |
| FTS tests | `tests/test_fts.py` | FTS5 behavior and fallbacks |
| Indexer tests | `tests/test_indexer.py` | build_index + tree conversion |
| Parser tests | `tests/test_treesitter_parser.py` | tree-sitter structural parsing (if available) |

## Commands
```bash
python -m pytest tests/ -v
```

## Conventions
- Tests assume global config can be reset between cases (fixture in `tests/conftest.py`).
