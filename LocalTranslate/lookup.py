"""Offline CC-CEDICT lookup. One JSON request on stdin, one response on stdout."""

import json
import re
import sqlite3
import sys
import unicodedata
from pathlib import Path


DATABASE = Path(__file__).with_name("data") / "dictionary.sqlite3"
HAN = re.compile(r"[\u3400-\u9fff\uf900-\ufaff\U00020000-\U000323af]")
MAX_LENGTH = 200


def normalize(text):
    text = unicodedata.normalize("NFKC", text).casefold()
    text = text.replace("’", "'").replace("–", "-").replace("—", "-")
    return " ".join(text.split()).strip(" \t\n.!?。！？,，;；\"“”")


def search(request, database=DATABASE):
    if not isinstance(request, dict) or not isinstance(request.get("text"), str):
        return {"status": "error", "reason": "invalid_request", "message": "请输入中文或英文词条。"}
    text = request["text"].strip()
    mode = request.get("direction", "auto")
    if mode not in ("auto", "zh", "en"):
        return {"status": "error", "reason": "invalid_direction", "message": "翻译方向无效。"}
    direction = ("zh" if HAN.search(text) else "en") if mode == "auto" else mode
    result = {"query": text, "direction": direction, "entries": [], "suggestions": []}
    if len(text) > MAX_LENGTH:
        return dict(result, status="error", reason="input_too_long", message="最多输入 200 个字符，请查询词条或短语。")
    key = normalize(text)
    if not key:
        return dict(result, status="idle", reason="empty_input", message="输入中文或英文词条，离线查询。")
    if not HAN.search(key) and not re.search(r"[a-zA-Z0-9]", key):
        return dict(result, status="empty", reason="unsupported_input", message="请输入中文或英文词条。")
    try:
        with sqlite3.connect(Path(database).resolve().as_uri() + "?mode=ro", uri=True) as db:
            db.row_factory = sqlite3.Row
            result["dictionaryCount"] = int(db.execute("SELECT value FROM metadata WHERE key='entries'").fetchone()[0])
            rows = db.execute("""
                SELECT e.*, MIN(t.rank) AS match_rank FROM terms t
                JOIN entries e ON e.id = t.entry_id
                WHERE t.language = ? AND t.term = ?
                GROUP BY e.id
                ORDER BY match_rank, length(e.simplified), e.simplified, e.id LIMIT 21
            """, (direction, key)).fetchall()
            result["truncated"] = len(rows) > 20
            for row in rows[:20]:
                meanings = json.loads(row["meanings"])
                result["entries"].append({
                    "simplified": row["simplified"], "traditional": row["traditional"],
                    "pinyin": row["pinyin"], "meanings": meanings,
                    "translation": "; ".join(meanings) if direction == "zh" else row["simplified"],
                })
            if rows:
                return dict(result, status="ready", reason="exact_match", message="内置字典匹配")
            # An indexed range, not LIKE: punctuation cannot act as SQL wildcards.
            suggestions = db.execute("""
                SELECT DISTINCT term FROM terms WHERE language = ? AND term >= ? AND term < ?
                ORDER BY term LIMIT 8
            """, (direction, key, key + chr(0x10FFFF))).fetchall()
            result["suggestions"] = [row[0] for row in suggestions]
            return dict(result, status="empty", reason="not_found", message="内置字典未收录此词条或短语；可缩短输入或查询原形词。")
    except (sqlite3.Error, OSError, ValueError, TypeError):
        return dict(result, status="error", reason="dictionary_unavailable", message="无法读取内置字典，请重新安装插件的 data 目录。")


def main():
    try:
        raw = sys.stdin.buffer.read(16385)
        if len(raw) > 16384:
            raise ValueError("request too large")
        request = json.loads(raw)
        result = search(request)
    except (ValueError, UnicodeError):
        result = {"status": "error", "reason": "invalid_request", "message": "查询请求格式无效。"}
    print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    main()
