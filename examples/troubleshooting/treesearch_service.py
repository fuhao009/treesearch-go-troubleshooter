# -*- coding: utf-8 -*-
"""
Offline TreeSearch service for troubleshooting knowledge.

Two roles only:
1. TreeSearch service: import/search/get/update/export
2. Executor unit: reads steps via API, executes locally, writes result back
"""
from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
import re
import sqlite3
import sys
import threading
import time
import urllib.parse
from dataclasses import asdict, dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
if REPO_ROOT not in sys.path:
    sys.path.insert(0, REPO_ROOT)

from treesearch import (
    Document,
    FTS5Index,
    TreeSearchConfig,
    build_index,
    load_documents,
    search_sync,
    set_config,
)

logger = logging.getLogger(__name__)

DEFAULT_DB = os.path.join(os.path.dirname(os.path.abspath(__file__)), "indexes", "service.db")
DEFAULT_RECORD_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "records")

STEP_TITLE_RE = re.compile(r"^(?:step|步骤)\s*[0-9一二三四五六七八九十]+", re.IGNORECASE)
CODE_BLOCK_RE = re.compile(r"```([a-zA-Z0-9_-]*)\n(.*?)```", re.DOTALL)


@dataclass
class StepView:
    index: int
    node_id: str
    title: str
    path: list[str]
    text: str
    commands: list[dict[str, str]]


class TreeSearchService:
    def __init__(self, db_path: str, record_dir: str):
        self.db_path = os.path.abspath(db_path)
        self.record_dir = os.path.abspath(record_dir)
        self._write_lock = threading.RLock()
        os.makedirs(os.path.dirname(self.db_path), exist_ok=True)
        os.makedirs(self.record_dir, exist_ok=True)

        cfg = TreeSearchConfig(
            fts_db_path=self.db_path,
            if_add_node_summary=True,
            if_add_doc_description=True,
            if_add_node_text=True,
        )
        set_config(cfg)
        self._ensure_experience_table()

    def _sqlite_conn(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=5.0)
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("PRAGMA busy_timeout=5000")
        return conn

    def _ensure_experience_table(self) -> None:
        conn = self._sqlite_conn()
        try:
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS experience_records (
                    record_id TEXT PRIMARY KEY,
                    source_doc_id TEXT DEFAULT '',
                    source_doc_name TEXT DEFAULT '',
                    created_at TEXT DEFAULT '',
                    updated_at TEXT DEFAULT '',
                    payload_json TEXT NOT NULL
                )
                """
            )
            conn.commit()
        finally:
            conn.close()

    def health(self) -> dict[str, Any]:
        doc_count = 0
        exp_count = 0
        if os.path.exists(self.db_path):
            conn = self._sqlite_conn()
            try:
                try:
                    row = conn.execute("SELECT COUNT(*) FROM documents").fetchone()
                    doc_count = int(row[0]) if row else 0
                except sqlite3.OperationalError:
                    doc_count = 0
                row = conn.execute("SELECT COUNT(*) FROM experience_records").fetchone()
                exp_count = int(row[0]) if row else 0
            finally:
                conn.close()
        return {
            "status": "ok",
            "db_path": self.db_path,
            "record_dir": self.record_dir,
            "document_count": doc_count,
            "experience_count": exp_count,
        }

    def import_playbooks(self, paths: list[str], force: bool = False) -> dict[str, Any]:
        if not paths:
            raise ValueError("paths is required")
        with self._write_lock:
            docs = asyncio.run(
                build_index(
                    paths=paths,
                    db_path=self.db_path,
                    if_add_node_summary=True,
                    if_add_doc_description=True,
                    if_add_node_text=True,
                    force=force,
                )
            )
        return {
            "imported_count": len(docs),
            "documents": [self._doc_meta(doc) for doc in docs],
        }

    def import_experience_paths(self, paths: list[str]) -> dict[str, Any]:
        imported: list[dict[str, Any]] = []
        for path in paths:
            with open(path, "r", encoding="utf-8") as f:
                payload = json.load(f)
            imported.append(self.save_experience_record(payload, source_path=os.path.abspath(path)))
        return {"imported_count": len(imported), "records": imported}

    def save_experience_record(self, record: dict[str, Any], source_path: str = "") -> dict[str, Any]:
        record = dict(record)
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        record_id = str(record.get("record_id") or record.get("id") or f"record_{int(time.time() * 1000)}")
        record["record_id"] = record_id
        record.setdefault("created_at", now)
        record["updated_at"] = now

        if source_path:
            record.setdefault("source_path", source_path)

        doc = self._record_to_document(record, source_path=source_path)
        payload_json = json.dumps(record, ensure_ascii=False, indent=2)

        if not source_path:
            source_path = os.path.join(self.record_dir, f"{record_id}.json")
            with open(source_path, "w", encoding="utf-8") as f:
                f.write(payload_json)
            record["source_path"] = source_path
            payload_json = json.dumps(record, ensure_ascii=False, indent=2)
            doc = self._record_to_document(record, source_path=source_path)

        with self._write_lock:
            fts = FTS5Index(db_path=self.db_path)
            try:
                fts.save_document(doc)
                fts.index_document(doc, force=True)
            finally:
                fts.close()

            conn = self._sqlite_conn()
            try:
                conn.execute(
                    """
                    INSERT OR REPLACE INTO experience_records
                    (record_id, source_doc_id, source_doc_name, created_at, updated_at, payload_json)
                    VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (
                        record_id,
                        str(record.get("source_doc_id", "")),
                        str(record.get("source_doc_name", "")),
                        str(record.get("created_at", now)),
                        str(record.get("updated_at", now)),
                        payload_json,
                    ),
                )
                conn.commit()
            finally:
                conn.close()

        return {
            "record_id": record_id,
            "doc_id": doc.doc_id,
            "source_path": source_path,
        }

    def export_experience(self, record_id: str | None = None, limit: int = 100) -> dict[str, Any]:
        conn = self._sqlite_conn()
        try:
            if record_id:
                row = conn.execute(
                    """
                    SELECT record_id, source_doc_id, source_doc_name, created_at, updated_at, payload_json
                    FROM experience_records WHERE record_id = ?
                    """,
                    (record_id,),
                ).fetchone()
                if not row:
                    raise KeyError(f"record not found: {record_id}")
                return {"record": self._row_to_record(row)}

            rows = conn.execute(
                """
                SELECT record_id, source_doc_id, source_doc_name, created_at, updated_at, payload_json
                FROM experience_records
                ORDER BY updated_at DESC, record_id DESC
                LIMIT ?
                """,
                (limit,),
            ).fetchall()
            return {"records": [self._row_to_record(row) for row in rows]}
        finally:
            conn.close()

    def list_documents(self) -> dict[str, Any]:
        docs = load_documents(self.db_path) if os.path.exists(self.db_path) else []
        return {"documents": [self._doc_meta(doc) for doc in docs]}

    def search(self, query: str, top_k_docs: int = 5, max_nodes_per_doc: int = 5) -> dict[str, Any]:
        docs = load_documents(self.db_path)
        result = search_sync(
            query=query,
            documents=docs,
            top_k_docs=top_k_docs,
            max_nodes_per_doc=max_nodes_per_doc,
            include_ancestors=True,
        )
        return result

    def get_document(self, doc_id: str) -> dict[str, Any]:
        fts = FTS5Index(db_path=self.db_path)
        try:
            doc = fts.load_document(doc_id)
        finally:
            fts.close()
        if doc is None:
            raise KeyError(f"document not found: {doc_id}")

        steps = self._extract_steps(doc)
        return {
            "doc_id": doc.doc_id,
            "doc_name": doc.doc_name,
            "doc_description": doc.doc_description,
            "source_type": doc.source_type,
            "source_path": doc.metadata.get("source_path", ""),
            "structure": doc.structure,
            "steps": [asdict(step) for step in steps],
        }

    def _doc_meta(self, doc: Document) -> dict[str, Any]:
        return {
            "doc_id": doc.doc_id,
            "doc_name": doc.doc_name,
            "doc_description": doc.doc_description,
            "source_type": doc.source_type,
            "source_path": doc.metadata.get("source_path", ""),
        }

    def _record_to_document(self, record: dict[str, Any], source_path: str = "") -> Document:
        record_id = str(record["record_id"])
        title = f"排查记录：{record.get('source_doc_name') or record.get('doc_name') or record_id}"
        steps = []
        for idx, step in enumerate(record.get("steps", []), 1):
            step_text = self._build_step_text(step)
            steps.append(
                {
                    "title": str(step.get("title") or f"步骤 {idx}"),
                    "node_id": str(idx),
                    "text": step_text,
                }
            )

        root = {
            "title": title,
            "node_id": "0",
            "text": json.dumps(
                {
                    "record_id": record_id,
                    "query": record.get("query", ""),
                    "created_at": record.get("created_at", ""),
                    "updated_at": record.get("updated_at", ""),
                    "summary": record.get("summary", {}),
                },
                ensure_ascii=False,
                indent=2,
            ),
            "nodes": steps,
        }

        return Document(
            doc_id=record_id,
            doc_name=record_id,
            structure=[root],
            doc_description=f"排查记录 {record.get('source_doc_name', '')}，步骤数 {len(steps)}",
            metadata={"source_path": source_path or record.get("source_path", "")},
            source_type="experience",
        )

    def _build_step_text(self, step: dict[str, Any]) -> str:
        parts = [str(step.get("text", "")).strip()]
        note = str(step.get("note", "")).strip()
        if note:
            parts.append("观察记录\n" + note)
        for idx, command in enumerate(step.get("commands", []), 1):
            block = [
                f"命令块 {idx}",
                str(command.get("command", "")).strip(),
                f"exit_code={command.get('exit_code', 0)}",
            ]
            if command.get("timed_out"):
                block.append("timed_out=true")
            output = str(command.get("output", "")).strip()
            if output:
                block.append("output:\n" + output)
            parts.append("\n".join(block))
        return "\n\n".join(part for part in parts if part)

    def _row_to_record(self, row: sqlite3.Row | tuple[Any, ...]) -> dict[str, Any]:
        payload = json.loads(row[5])
        payload.setdefault("record_id", row[0])
        payload.setdefault("source_doc_id", row[1])
        payload.setdefault("source_doc_name", row[2])
        payload.setdefault("created_at", row[3])
        payload.setdefault("updated_at", row[4])
        return payload

    def _extract_steps(self, doc: Document) -> list[StepView]:
        steps: list[StepView] = []

        def walk(nodes: list[dict[str, Any]], path: list[str]) -> None:
            for node in nodes:
                title = str(node.get("title", ""))
                node_path = [*path, title]
                if STEP_TITLE_RE.match(title):
                    full_text = self._aggregate_text(node)
                    commands = self._extract_commands(full_text)
                    steps.append(
                        StepView(
                            index=len(steps) + 1,
                            node_id=str(node.get("node_id", "")),
                            title=title,
                            path=node_path,
                            text=full_text,
                            commands=commands,
                        )
                    )
                child_nodes = node.get("nodes") or []
                if child_nodes:
                    walk(child_nodes, node_path)

        walk(doc.structure, [])
        return steps

    def _aggregate_text(self, node: dict[str, Any]) -> str:
        parts = []
        text = str(node.get("text", "")).strip()
        if text:
            parts.append(text)
        for child in node.get("nodes") or []:
            child_text = self._aggregate_text(child)
            if child_text:
                parts.append(child_text)
        return "\n\n".join(parts)

    def _extract_commands(self, text: str) -> list[dict[str, str]]:
        commands: list[dict[str, str]] = []
        for match in CODE_BLOCK_RE.finditer(text):
            lang = match.group(1).strip().lower()
            if lang not in {"bash", "sh", "shell", "zsh"}:
                continue
            body = match.group(2).strip()
            if not body:
                continue
            commands.append({"lang": lang, "body": body})
        return commands


class TreeSearchHandler(BaseHTTPRequestHandler):
    server_version = "TreeSearchService/0.1"

    @property
    def app(self) -> TreeSearchService:
        return self.server.app  # type: ignore[attr-defined]

    def do_GET(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        query = urllib.parse.parse_qs(parsed.query)

        try:
            if path == "/api/v1/health":
                return self._json(HTTPStatus.OK, self.app.health())
            if path == "/api/v1/documents":
                return self._json(HTTPStatus.OK, self.app.list_documents())
            if path.startswith("/api/v1/doc/"):
                doc_id = urllib.parse.unquote(path.rsplit("/", 1)[-1])
                return self._json(HTTPStatus.OK, self.app.get_document(doc_id))
            if path == "/api/v1/experience/export":
                record_id = query.get("record_id", [None])[0]
                limit = int(query.get("limit", ["100"])[0])
                return self._json(HTTPStatus.OK, self.app.export_experience(record_id=record_id, limit=limit))
            return self._json_error(HTTPStatus.NOT_FOUND, "not_found", f"unknown path: {path}")
        except KeyError as exc:
            return self._json_error(HTTPStatus.NOT_FOUND, "not_found", str(exc))
        except Exception as exc:  # noqa: BLE001
            logger.exception("GET failed")
            return self._json_error(HTTPStatus.INTERNAL_SERVER_ERROR, "internal_error", str(exc))

    def do_POST(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        try:
            payload = self._read_json()

            if path == "/api/v1/playbooks/import":
                paths = payload.get("paths") or []
                force = bool(payload.get("force", False))
                return self._json(HTTPStatus.OK, self.app.import_playbooks(paths=paths, force=force))

            if path == "/api/v1/experience/import":
                if payload.get("record") is not None:
                    result = self.app.save_experience_record(payload["record"])
                    return self._json(HTTPStatus.OK, result)
                paths = payload.get("paths") or []
                return self._json(HTTPStatus.OK, self.app.import_experience_paths(paths))

            if path == "/api/v1/search":
                query = str(payload.get("query", "")).strip()
                top_k_docs = int(payload.get("top_k_docs", 5))
                max_nodes_per_doc = int(payload.get("max_nodes_per_doc", 5))
                return self._json(
                    HTTPStatus.OK,
                    self.app.search(query=query, top_k_docs=top_k_docs, max_nodes_per_doc=max_nodes_per_doc),
                )

            if path == "/api/v1/executions/result":
                record = payload.get("record")
                if not isinstance(record, dict):
                    return self._json_error(HTTPStatus.BAD_REQUEST, "bad_request", "record is required")
                return self._json(HTTPStatus.OK, self.app.save_experience_record(record))

            return self._json_error(HTTPStatus.NOT_FOUND, "not_found", f"unknown path: {path}")
        except ValueError as exc:
            return self._json_error(HTTPStatus.BAD_REQUEST, "bad_request", str(exc))
        except KeyError as exc:
            return self._json_error(HTTPStatus.NOT_FOUND, "not_found", str(exc))
        except Exception as exc:  # noqa: BLE001
            logger.exception("POST failed")
            return self._json_error(HTTPStatus.INTERNAL_SERVER_ERROR, "internal_error", str(exc))

    def log_message(self, fmt: str, *args: Any) -> None:
        logger.info("%s - %s", self.address_string(), fmt % args)

    def _read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0:
            return {}
        raw = self.rfile.read(length)
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def _json(self, status: HTTPStatus, data: dict[str, Any]) -> None:
        body = json.dumps(data, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _json_error(self, status: HTTPStatus, code: str, message: str) -> None:
        self._json(status, {"error": code, "message": message})


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Offline TreeSearch service")
    parser.add_argument("--listen", default="127.0.0.1:8765", help="host:port")
    parser.add_argument("--db", default=DEFAULT_DB, help="SQLite DB path")
    parser.add_argument("--record-dir", default=DEFAULT_RECORD_DIR, help="Directory to store experience JSON files")
    parser.add_argument("--log-level", default="INFO", help="logging level")
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    logging.basicConfig(
        level=getattr(logging, args.log_level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(message)s",
    )

    host, port_text = args.listen.rsplit(":", 1)
    port = int(port_text)
    app = TreeSearchService(db_path=args.db, record_dir=args.record_dir)

    server = HTTPServer((host, port), TreeSearchHandler)
    server.app = app  # type: ignore[attr-defined]

    logger.info("TreeSearch service listening on http://%s", args.listen)
    server.serve_forever()


if __name__ == "__main__":
    main()
