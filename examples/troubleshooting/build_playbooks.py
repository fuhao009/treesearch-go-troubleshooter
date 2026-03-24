# -*- coding: utf-8 -*-
"""
Build a troubleshooting knowledge base for server incident response.

Usage:
    python examples/troubleshooting/build_playbooks.py
    python examples/troubleshooting/build_playbooks.py --include-records
"""
import argparse
import asyncio
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))

from treesearch import build_index, load_documents, search

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DB = os.path.join(BASE_DIR, "indexes", "troubleshooting.db")
DEFAULT_PLAYBOOKS = os.path.join(BASE_DIR, "playbooks", "*.md")
DEFAULT_RECORDS = os.path.join(BASE_DIR, "records", "*.json")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Build TreeSearch indexes for troubleshooting playbooks.")
    parser.add_argument("--db", default=DEFAULT_DB, help="SQLite DB path for TreeSearch")
    parser.add_argument("--playbooks-glob", default=DEFAULT_PLAYBOOKS, help="Playbook glob")
    parser.add_argument("--records-glob", default=DEFAULT_RECORDS, help="Historical record glob")
    parser.add_argument("--include-records", action="store_true", help="Index historical record JSON files too")
    parser.add_argument("--force", action="store_true", help="Force rebuild even if files look unchanged")
    parser.add_argument(
        "--query",
        action="append",
        default=[],
        help="Optional demo query. Can be repeated.",
    )
    return parser.parse_args()


async def main() -> None:
    args = parse_args()
    os.makedirs(os.path.dirname(os.path.abspath(args.db)), exist_ok=True)

    paths = [args.playbooks_glob]
    if args.include_records:
        paths.append(args.records_glob)

    print("=== Building troubleshooting knowledge base ===")
    print(f"DB: {args.db}")
    print("Paths:")
    for path in paths:
        print(f"  - {path}")

    documents = await build_index(
        paths=paths,
        db_path=args.db,
        if_add_node_summary=True,
        if_add_node_text=True,
        if_add_doc_description=True,
        force=args.force,
    )

    print(f"\nIndexed {len(documents)} documents:")
    for doc in documents:
        source_path = doc.metadata.get("source_path", "")
        print(f"  - {doc.doc_id}: {doc.doc_name}")
        if source_path:
            print(f"    source: {source_path}")
        if doc.doc_description:
            print(f"    desc: {doc.doc_description[:120]}")

    demo_queries = args.query or [
        "CPU 高 iowait 进程热点",
        "容器重启 black out 采集器",
        "performance radar L3 L4 trigger",
    ]

    loaded = load_documents(args.db)
    print("\n=== Demo search ===")
    for query in demo_queries:
        result = await search(
            query=query,
            documents=loaded,
            max_nodes_per_doc=2,
            include_ancestors=True,
        )
        print(f"\nQuery: {query}")
        if not result["flat_nodes"]:
            print("  no matches")
            continue
        for node in result["flat_nodes"][:5]:
            print(
                "  - "
                f"{node['doc_name']} / {node['title']} "
                f"(score={node['score']:.4f})"
            )

    print("\nDone.")


if __name__ == "__main__":
    asyncio.run(main())
