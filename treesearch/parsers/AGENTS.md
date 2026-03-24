# treesearch/parsers/

## Overview
Parsers convert source files into a heading/tree structure; registry decides parser + default prefilter chain by extension/source_type.

## Entry Points
| Concern | Location | Notes |
|---------|----------|-------|
| Extension routing | `treesearch/parsers/registry.py` | `SOURCE_TYPE_MAP` + built-in registration |
| Default prefilters | `treesearch/parsers/registry.py` | `PREFILTER_ROUTING` drives FTS5 vs Grep combinations |
| Python structure | `treesearch/parsers/ast_parser.py` | AST extraction for `.py` headings/signatures |
| Multi-language structure (optional) | `treesearch/parsers/treesitter_parser.py` | enabled only if `tree-sitter-languages` is installed |
| Optional doc formats | `treesearch/parsers/pdf_parser.py`, `treesearch/parsers/docx_parser.py`, `treesearch/parsers/html_parser.py` | guarded imports; enabled via extras |

## How To Add A Parser
- Register: `ParserRegistry.register(ext, parser_fn, source_type=...)` in `treesearch/parsers/registry.py`.
- Pick prefilters: update `PREFILTER_ROUTING` for the new `source_type` if defaults differ.
- Keep deps optional: wrap imports in try/except and log a clear debug message when unavailable.

## Conventions (Local)
- Built-in parsers are registered at import time by `treesearch/parsers/registry.py`.
- `SOURCE_TYPE_MAP` is extension-based; unknown extensions fall back to `text`.

## Anti-Patterns (Local)
- Avoid auto-enabling optional parsers without guarding imports; keep optional deps behind try/except with clear debug logs.
- Tree-sitter loader currently suppresses a deprecation warning; avoid expanding reliance on deprecated APIs without a plan.
