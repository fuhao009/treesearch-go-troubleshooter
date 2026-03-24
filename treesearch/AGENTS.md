# treesearch/

## Overview
Core library package: build document trees, persist to SQLite, and search via FTS5 + structure-aware routing.

## Core Flows
- Indexing: `treesearch/indexer.py` parses sources via `treesearch/parsers/registry.py`, then finalizes node ids + summaries + optional doc description.
- Search: `treesearch/search.py` runs pre-filter scoring (FTS5 and/or Grep) then merges and attaches node fields.
- Storage: `treesearch/fts.py` stores both documents and FTS5 tables in one SQLite DB; incremental metadata is kept alongside.

## Key APIs
| API | Location | Notes |
|-----|----------|-------|
| `TreeSearch` | `treesearch/treesearch.py` | high-level wrapper; lazy indexing + incremental reload |
| `build_index()` | `treesearch/indexer.py` | batch async indexing; used by CLI and examples |
| `search()` | `treesearch/search.py` | unified multi-doc search + result shaping |
| `FTS5Index` | `treesearch/fts.py` | prefilter scorer + persistence layer |
| `Document` | `treesearch/tree.py` | in-memory tree + node_id map cache |

## Hotspots
- `treesearch/fts.py` and `treesearch/indexer.py` are the largest modules; expect most performance/behavior changes to land there.

## Conventions (Local)
- Prefer async APIs (`build_index()`, `search()`); sync wrappers exist in `TreeSearch` and `search_sync()`.
- Config is global singleton; tests reset via `tests/conftest.py`.

## Anti-Patterns (Local)
- Do not assume FTS5 is available in the system sqlite3; `treesearch/__init__.py` may swap to `pysqlite3` when needed.
