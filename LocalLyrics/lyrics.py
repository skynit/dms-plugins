"""Read embedded audio lyrics offline. JSON stdin/stdout, ffprobe metadata."""

import json
import os
import re
import sqlite3
import subprocess
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit

MAX_BYTES = 1024 * 1024
MAX_LINES = 5000
STAMP = re.compile(r"\[(\d{1,4}):([0-5]\d)(?:[.:](\d{1,3}))?\]")
META = re.compile(r"^\[(?:ar|al|ti|au|by|re|ve|length|offset):.*\]$", re.I)


def parse_lrc(text):
    offsets = re.findall(r"\[offset:([+-]?\d+)\]", text, re.I)
    offset = int(offsets[-1]) / 1000 if offsets else 0
    timed = {}
    plain = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line or META.fullmatch(line):
            continue
        stamps = list(STAMP.finditer(line))
        if not stamps or stamps[0].start() != 0:
            plain.append(line)
            continue
        content = STAMP.sub("", line).strip()
        # Enhanced LRC word timings are ignored; this is line-level display.
        content = re.sub(r"<\d{1,4}:[0-5]\d(?:[.:]\d{1,3})?>", "", content).strip()
        for stamp in stamps:
            seconds = int(stamp[1]) * 60 + int(stamp[2])
            fraction = float("0." + stamp[3]) if stamp[3] else 0
            time = round(seconds + fraction - offset, 3)
            values = timed.setdefault(time, [])
            if content not in values:
                values.append(content)
        if len(timed) > MAX_LINES:
            raise ValueError("歌词行数超过 5000 行。")
    if timed:
        return {"status": "synced", "lines": [{"time": time, "text": "\n".join(value for value in values if value)}
                                               for time, values in sorted(timed.items())]}
    if len(plain) > MAX_LINES:
        raise ValueError("歌词行数超过 5000 行。")
    if plain:
        return {"status": "plain", "lines": [{"time": None, "text": line} for line in plain]}
    return {"status": "empty", "reason": "empty_lyrics", "message": "内嵌歌词没有可显示的内容。", "lines": []}


def audio_path(request):
    url = urlsplit(request.get("url", ""))
    if url.scheme == "file" and url.netloc in ("", "localhost"):
        song = Path(unquote(url.path))
        if song.is_absolute():
            return song
    if url.scheme:
        raise ValueError("当前歌曲不是本地文件，无法离线读取内嵌歌词。")
    if "termusic" not in request.get("player", "").casefold():
        raise ValueError("播放器未提供本地歌曲路径，无法读取内嵌歌词。")
    if not request.get("title") or not request.get("artist"):
        raise ValueError("termusic 尚未提供完整的歌名和歌手信息。")
    config = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    library = config / "termusic/library2.db"
    if not library.is_file():
        raise ValueError("找不到 termusic 本地媒体库，请先在 termusic 中扫描歌曲。")
    with sqlite3.connect(library.as_uri() + "?mode=ro", uri=True, timeout=1) as db:
        db.row_factory = sqlite3.Row
        rows = db.execute("""
            SELECT t.file_dir, t.file_stem, t.file_ext, t.duration, a.title AS album
            FROM tracks t JOIN tracks_metadata m ON m.track = t.id
            LEFT JOIN albums a ON a.id = t.album
            WHERE m.title = ? AND m.artist_display = ?
        """, (request["title"], request["artist"])).fetchall()
    if len(rows) > 1 and request.get("album"):
        matches = [row for row in rows if row["album"] == request["album"]]
        if matches:
            rows = matches
    if len(rows) > 1 and request.get("duration", 0) > 0:
        matches = [row for row in rows if row["duration"] is not None and abs(row["duration"] - request["duration"]) <= 2]
        if matches:
            rows = matches
    paths = list(dict.fromkeys(Path(row["file_dir"]) / (row["file_stem"] + "." + row["file_ext"]) for row in rows))
    if not paths:
        raise ValueError("termusic 媒体库中未找到匹配的歌曲，请重新扫描媒体库。")
    if len(paths) != 1:
        raise ValueError("媒体库中有多个同名版本，无法确定当前歌曲文件。")
    return paths[0]


def embedded_lyrics(path):
    if path.suffix.lower() not in {".flac", ".mp3", ".m4a", ".mp4", ".ogg", ".opus", ".wav", ".wma", ".aac", ".ape", ".wv", ".aiff", ".aif"}:
        raise ValueError("不支持此音频文件格式。")
    if not path.is_file():
        raise ValueError("歌曲文件不存在或已移动，请刷新播放器媒体库。")
    try:
        process = subprocess.run(["ffprobe", "-v", "error", "-protocol_whitelist", "file", "-show_entries",
                                  "format_tags:stream_tags", "-of", "json", str(path)],
                                 capture_output=True, timeout=6, check=True)
    except FileNotFoundError:
        raise ValueError("请安装 FFmpeg（ffprobe），然后重新读取。") from None
    except subprocess.TimeoutExpired:
        raise ValueError("读取音频标签超时，请重试。") from None
    except subprocess.CalledProcessError:
        raise ValueError("无法读取音频标签，请确认文件完整且格式受支持。") from None
    if len(process.stdout) > MAX_BYTES * 4:
        raise ValueError("音频标签过大，无法显示。")
    metadata = json.loads(process.stdout)
    groups = [metadata.get("format", {}).get("tags", {})]
    groups.extend(stream.get("tags", {}) for stream in metadata.get("streams", []))
    texts = []
    for tags in groups:
        for key, value in tags.items():
            key = re.sub(r"[ _-]", "", key.casefold())
            if (key.startswith("lyrics") or key.startswith("unsyncedlyrics") or key in ("uslt", "©lyr")) and isinstance(value, str) and value.strip():
                if len(value.encode("utf-8")) > MAX_BYTES:
                    raise ValueError("内嵌歌词超过 1 MiB，无法显示。")
                texts.append(value)
    return texts


def load(request):
    if not isinstance(request, dict) or any(not isinstance(request.get(key, ""), str)
                                           for key in ("title", "artist", "album", "url", "player")):
        return {"status": "error", "reason": "invalid_request", "message": "歌词请求格式无效。", "lines": []}
    if not isinstance(request.get("duration", 0), (int, float)):
        return {"status": "error", "reason": "invalid_request", "message": "歌词请求格式无效。", "lines": []}
    if not request.get("title", "").strip() and not request.get("url", "").startswith("file:"):
        return {"status": "empty", "reason": "no_track", "message": "播放器尚未提供歌曲信息。", "lines": []}
    try:
        path = audio_path(request)
        results = [parse_lrc(text) for text in embedded_lyrics(path)]
        # Prefer a timed field over plain text when both tags are embedded.
        result = next((value for value in results if value["status"] == "synced"), None)
        result = result or next((value for value in results if value["status"] == "plain"), None)
        if result:
            return dict(result, path=str(path), source="embedded", message="")
        return {"status": "empty", "reason": "no_embedded_lyrics", "message": "这首歌没有可读取的内嵌歌词。", "lines": [], "path": str(path)}
    except (OSError, ValueError, sqlite3.Error) as error:
        return {"status": "error", "reason": "read_failed", "message": "读取失败：" + str(error), "lines": []}


def main():
    try:
        raw = sys.stdin.buffer.read(16385)
        if len(raw) > 16384:
            raise ValueError("Request too large")
        result = load(json.loads(raw))
    except (ValueError, UnicodeError):
        result = {"status": "error", "reason": "invalid_request", "message": "歌词请求格式无效。", "lines": []}
    print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    main()
