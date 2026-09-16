"""Build a distributable bilingual index from the bundled CC-CEDICT snapshot."""

import gzip
import hashlib
import json
import re
import sqlite3
from pathlib import Path

from lookup import normalize


DATA = Path(__file__).with_name("data")
SOURCE = DATA / "cedict.txt.gz"
OUTPUT = DATA / "dictionary.sqlite3"
PATTERN = re.compile(r"^(\S+) (\S+) \[(.*?)\] /(.*)/$")


def english_terms(meaning):
    if meaning.startswith(("CL:", "see ", "variant of ", "old variant of ", "surname ")):
        return set()
    # Notes clarify senses but are not part of a word's spelling.
    plain = re.sub(r"\([^()]*\)", "", meaning)
    plain = re.sub(r"\([^()]*\)", "", plain)
    if re.search(r"[\u3400-\u9fff]|\[", plain):
        return set()
    terms = {normalize(plain)}
    for part in plain.split(";"):
        term = normalize(part)
        terms.add(term)
        if term.startswith("to "):
            terms.add(term[3:])
        if term.startswith(("a ", "an ")):
            terms.add(term.split(" ", 1)[1])
    return {term for term in terms if term and len(term) <= 200}


def build():
    temporary = OUTPUT.with_suffix(".tmp")
    if temporary.exists():
        temporary.unlink()
    count = 0
    source_date = ""
    with sqlite3.connect(temporary) as db:
        db.executescript("""
            CREATE TABLE entries (id INTEGER PRIMARY KEY, traditional TEXT NOT NULL,
                simplified TEXT NOT NULL, pinyin TEXT NOT NULL, meanings TEXT NOT NULL);
            CREATE TABLE terms (language TEXT NOT NULL, term TEXT NOT NULL,
                entry_id INTEGER NOT NULL, rank INTEGER NOT NULL,
                PRIMARY KEY(language, term, entry_id)) WITHOUT ROWID;
            CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
        """)
        with gzip.open(SOURCE, "rt", encoding="utf-8") as source:
            for line in source:
                if line.startswith("#! date="):
                    source_date = line.strip().split("=", 1)[1]
                if line.startswith("#") or not line.strip():
                    continue
                match = PATTERN.match(line.rstrip())
                if not match:
                    raise ValueError("Malformed CC-CEDICT entry")
                traditional, simplified, pinyin, definition = match.groups()
                meanings = definition.split("/")
                count += 1
                db.execute("INSERT INTO entries VALUES (?, ?, ?, ?, ?)",
                           (count, traditional, simplified, pinyin, json.dumps(meanings, ensure_ascii=False)))
                for word in {normalize(traditional), normalize(simplified)}:
                    db.execute("INSERT INTO terms VALUES ('zh', ?, ?, 0)", (word, count))
                aliases = {}
                for meaning in meanings:
                    for term in english_terms(meaning):
                        rank = 0 if term == normalize(meaning) else 1
                        aliases[term] = min(rank, aliases.get(term, rank))
                db.executemany("INSERT INTO terms VALUES ('en', ?, ?, ?)",
                               ((term, count, rank) for term, rank in aliases.items()))
        metadata = {"name": "CC-CEDICT", "entries": str(count), "source_date": source_date,
                    "source_sha256": hashlib.sha256(SOURCE.read_bytes()).hexdigest(),
                    "license": "CC-BY-SA-4.0", "index_version": "1"}
        db.executemany("INSERT INTO metadata VALUES (?, ?)", metadata.items())
        db.commit()
        db.execute("VACUUM")
    temporary.replace(OUTPUT)
    metadata["database_sha256"] = hashlib.sha256(OUTPUT.read_bytes()).hexdigest()
    (DATA / "manifest.json").write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(metadata, indent=2))


if __name__ == "__main__":
    build()
