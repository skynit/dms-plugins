import hashlib
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from lookup import search
from build_dictionary import english_terms


class DictionaryTests(unittest.TestCase):
    def test_chinese_to_english(self):
        result = search({"text": "你好"})
        self.assertEqual(result["direction"], "zh")
        self.assertEqual(result["status"], "ready")
        self.assertIn("hello", result["entries"][0]["translation"])

    def test_english_to_chinese(self):
        result = search({"text": "computer"})
        self.assertEqual(result["direction"], "en")
        self.assertIn("电脑", [entry["translation"] for entry in result["entries"]])

    def test_traditional_matches_simplified(self):
        self.assertEqual(search({"text": "電腦"})["entries"], search({"text": "电脑"})["entries"])

    def test_phrase(self):
        result = search({"text": "good morning"})
        self.assertIn("早上好", [entry["translation"] for entry in result["entries"]])

    def test_normalization(self):
        self.assertEqual(search({"text": "  ＨＥＬＬＯ！ "})["entries"], search({"text": "hello"})["entries"])
        self.assertEqual(search({"text": "good   morning!"})["entries"], search({"text": "good morning"})["entries"])

    def test_unrecorded_sentence_is_not_word_substitution(self):
        result = search({"text": "Please open the violet window for my computer."})
        self.assertEqual(result["reason"], "not_found")
        self.assertEqual(result["entries"], [])

    def test_suggestions_are_separate(self):
        result = search({"text": "translat"})
        self.assertEqual(result["status"], "empty")
        self.assertEqual(result["entries"], [])
        self.assertIn("translate", result["suggestions"])
        self.assertLessEqual(len(result["suggestions"]), 8)

    def test_forced_direction(self):
        result = search({"text": "computer", "direction": "zh"})
        self.assertEqual(result["direction"], "zh")
        self.assertEqual(result["status"], "empty")

    def test_invalid_requests(self):
        for request in (None, [], {"text": 123}, {}):
            with self.subTest(request=request):
                self.assertEqual(search(request)["reason"], "invalid_request")
        self.assertEqual(search({"text": "hello", "direction": "fr"})["reason"], "invalid_direction")
        self.assertEqual(search({"text": "词" * 201})["reason"], "input_too_long")

    def test_empty_and_unsupported(self):
        self.assertEqual(search({"text": "  "})["status"], "idle")
        self.assertEqual(search({"text": "🙂"})["reason"], "unsupported_input")

    def test_sql_and_shell_input_is_literal(self):
        for query in ("' OR 1=1 --", "computer%", "computer_", "$(touch /tmp/translate-injection)"):
            with self.subTest(query=query):
                result = search({"text": query})
                self.assertEqual(result["entries"], [])
                self.assertEqual(result["suggestions"], [])

    def test_missing_and_corrupt_dictionary(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "missing.sqlite3"
            self.assertEqual(search({"text": "hello"}, path)["reason"], "dictionary_unavailable")
            self.assertFalse(path.exists())
            path.write_bytes(b"corrupt")
            self.assertEqual(search({"text": "hello"}, path)["reason"], "dictionary_unavailable")

    def test_many_matches_are_bounded(self):
        result = search({"text": "bright"})
        self.assertEqual(len(result["entries"]), 20)
        self.assertTrue(result["truncated"])

    def test_english_aliases(self):
        self.assertIn("thank", english_terms("to thank"))
        self.assertIn("computer", english_terms("computer (device)"))
        self.assertIn("cat", english_terms("a cat"))
        self.assertEqual(english_terms("CL:臺|台[tai2]"), set())
        self.assertEqual(english_terms("variant of 電腦|电脑[dian4 nao3]"), set())

    def test_bundled_integrity(self):
        manifest = json.loads((ROOT / "data/manifest.json").read_text())
        for name, key in (("cedict.txt.gz", "source_sha256"), ("dictionary.sqlite3", "database_sha256")):
            self.assertEqual(hashlib.sha256((ROOT / "data" / name).read_bytes()).hexdigest(), manifest[key])
        with sqlite3.connect((ROOT / "data/dictionary.sqlite3").as_uri() + "?mode=ro", uri=True) as db:
            self.assertEqual(db.execute("PRAGMA quick_check").fetchone()[0], "ok")
            self.assertEqual(db.execute("SELECT count(*) FROM entries").fetchone()[0], int(manifest["entries"]))
            self.assertGreater(int(manifest["entries"]), 100000)

    def test_cli_protocol(self):
        for payload, expected in ((b'{"text":"computer"}', "ready"), (b"invalid json", "error"), (b"x" * 16385, "error")):
            completed = subprocess.run([sys.executable, str(ROOT / "lookup.py")], input=payload, capture_output=True, check=True)
            self.assertEqual(json.loads(completed.stdout)["status"], expected)
            self.assertEqual(completed.stderr, b"")


if __name__ == "__main__":
    unittest.main()
