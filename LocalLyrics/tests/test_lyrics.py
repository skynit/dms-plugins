import json
import os
import sqlite3
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from lyrics import audio_path, load, parse_lrc, MAX_BYTES


class LyricsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.folder = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.folder.cleanup)
        cls.audio = {}
        for extension, codec in [('flac', 'flac'), ('mp3', 'libmp3lame'), ('m4a', 'aac'), ('ogg', 'libvorbis')]:
            path = Path(cls.folder.name) / ('embedded.' + extension)
            subprocess.run(['ffmpeg', '-v', 'error', '-f', 'lavfi', '-i', 'anullsrc=r=44100:cl=mono',
                            '-t', '0.1', '-c:a', codec, '-metadata', 'title=Embedded test',
                            '-metadata', 'artist=Test artist', '-metadata', 'lyrics=[00:01.00]第一句测试\n[00:03.50]第二句测试',
                            str(path)], check=True, capture_output=True)
            cls.audio[extension] = path
        for name, lyrics in [('plain', '纯文本内嵌歌词测试'), ('missing', None)]:
            path = Path(cls.folder.name) / (name + '.flac')
            command = ['ffmpeg', '-v', 'error', '-f', 'lavfi', '-i', 'anullsrc=r=44100:cl=mono', '-t', '0.1', '-c:a', 'flac']
            if lyrics:
                command += ['-metadata', 'lyrics=' + lyrics]
            subprocess.run(command + [str(path)], check=True, capture_output=True)
            cls.audio[name] = path

    def test_timestamp_precision_and_sort(self):
        result = parse_lrc('[01:02.345]third\n[00:00.2]first\n[00:03:45]second')
        self.assertEqual(result['status'], 'synced')
        self.assertEqual([line['time'] for line in result['lines']], [.2, 3.45, 62.345])

    def test_multiple_timestamps_and_translations(self):
        result = parse_lrc('[00:01][00:05]first\n[00:01]译文\n[00:01]first')
        self.assertEqual(result['lines'], [{'time': 1, 'text': 'first\n译文'}, {'time': 5, 'text': 'first'}])

    def test_offset_applies_to_whole_file(self):
        result = parse_lrc('[00:01]first\n[offset:+1500]')
        self.assertEqual(result['lines'][0]['time'], -.5)
        self.assertEqual(parse_lrc('[offset:-500]\n[00:01]first')['lines'][0]['time'], 1.5)

    def test_empty_timed_line_clears_previous(self):
        self.assertEqual(parse_lrc('[00:01]line\n[00:02]')['lines'][1], {'time': 2, 'text': ''})

    def test_metadata_and_word_timing(self):
        result = parse_lrc('[ar:测试歌手]\n[ti:测试]\n[00:01]<00:01.10>hello <00:01.5>world')
        self.assertEqual(result['lines'], [{'time': 1, 'text': 'hello world'}])

    def test_plain_and_empty(self):
        self.assertEqual(parse_lrc('[ar:Artist]\nplain text')['status'], 'plain')
        self.assertEqual(parse_lrc('[ar:Artist]\n\n')['status'], 'empty')

    def test_real_embedded_formats(self):
        for extension in ['flac', 'mp3', 'm4a', 'ogg']:
            with self.subTest(extension=extension):
                result = load({'url': self.audio[extension].as_uri()})
                self.assertEqual(result['status'], 'synced', result)
                self.assertEqual(result['source'], 'embedded')
                self.assertEqual(result['lines'], [{'time': 1, 'text': '第一句测试'}, {'time': 3.5, 'text': '第二句测试'}])

    def test_real_plain_and_absent_tags(self):
        self.assertEqual(load({'url': self.audio['plain'].as_uri()})['status'], 'plain')
        self.assertEqual(load({'url': self.audio['missing'].as_uri()})['reason'], 'no_embedded_lyrics')

    def test_local_url_and_network_rejection(self):
        self.assertEqual(audio_path({'url': self.audio['flac'].as_uri()}), self.audio['flac'])
        self.assertEqual(audio_path({'url': 'file:///tmp/a%20b.flac'}), Path('/tmp/a b.flac'))
        for url in ['https://example.com/song.flac', 'file://remote/song.flac', 'ftp://example.com/song.mp3']:
            with patch('lyrics.subprocess.run') as probe:
                self.assertEqual(load({'title': 'Song', 'url': url})['status'], 'error')
                probe.assert_not_called()

    def test_termusic_lookup_and_ambiguous_versions(self):
        with tempfile.TemporaryDirectory() as folder, patch.dict(os.environ, {'XDG_CONFIG_HOME': folder}):
            library = Path(folder) / 'termusic/library2.db'
            library.parent.mkdir()
            with sqlite3.connect(library) as db:
                db.executescript('CREATE TABLE tracks(id INTEGER, file_dir TEXT, file_stem TEXT, file_ext TEXT, duration INTEGER, album INTEGER); CREATE TABLE tracks_metadata(track INTEGER, title TEXT, artist_display TEXT); CREATE TABLE albums(id INTEGER, title TEXT);')
                file = self.audio['flac']
                db.execute('INSERT INTO tracks VALUES(1,?,?,?,?,1)', (str(file.parent), file.stem, 'flac', 180))
                db.execute('INSERT INTO tracks_metadata VALUES(1,?,?)', ('Song', 'Artist'))
                db.execute('INSERT INTO albums VALUES(1,?)', ('Album A',))
            request = {'player': 'org.mpris.MediaPlayer2.termusic', 'title': 'Song', 'artist': 'Artist'}
            self.assertEqual(load(request)['status'], 'synced')
            with sqlite3.connect(library) as db:
                db.execute('INSERT INTO tracks VALUES(2,?,?,?,?,2)', (str(file.parent), 'other', 'flac', 210))
                db.execute('INSERT INTO tracks_metadata VALUES(2,?,?)', ('Song', 'Artist'))
                db.execute('INSERT INTO albums VALUES(2,?)', ('Album B',))
            self.assertIn('多个同名版本', load(request)['message'])
            self.assertEqual(audio_path(dict(request, album='Album A')), file)
            self.assertEqual(audio_path(dict(request, duration=180)), file)
            self.assertEqual(load(dict(request, title="' OR 1=1 --"))['status'], 'error')
            self.assertEqual(load(dict(request, artist='Other artist'))['status'], 'error')

    def test_missing_termusic_library_and_other_players(self):
        with tempfile.TemporaryDirectory() as folder, patch.dict(os.environ, {'XDG_CONFIG_HOME': folder}):
            self.assertIn('媒体库', load({'player': 'termusic', 'title': 'Song', 'artist': 'Artist'})['message'])
            self.assertFalse((Path(folder)/'termusic/library2.db').exists())
        self.assertIn('未提供本地歌曲路径', load({'player': 'other', 'title': 'Song'})['message'])

    def test_timed_tag_preferred_and_stream_tags(self):
        data = {'format': {'tags': {'LYRICS': 'plain text'}}, 'streams': [{'tags': {'lyrics-eng': '[00:02]timed'}}]}
        with patch('lyrics.subprocess.run', return_value=subprocess.CompletedProcess([], 0, json.dumps(data).encode())):
            self.assertEqual(load({'url': self.audio['flac'].as_uri()})['lines'][0]['time'], 2)

    def test_probe_failure_timeout_and_missing_dependency(self):
        for error in [FileNotFoundError(), subprocess.TimeoutExpired('ffprobe', 6), subprocess.CalledProcessError(1, 'ffprobe')]:
            with patch('lyrics.subprocess.run', side_effect=error):
                self.assertEqual(load({'url': self.audio['flac'].as_uri()})['status'], 'error')
        with tempfile.TemporaryDirectory() as folder:
            self.assertEqual(load({'url': (Path(folder)/'missing.flac').as_uri()})['status'], 'error')

    def test_lyrics_and_line_limits(self):
        metadata = {'format': {'tags': {'lyrics': 'x' * (MAX_BYTES + 1)}}}
        with patch('lyrics.subprocess.run', return_value=subprocess.CompletedProcess([], 0, json.dumps(metadata).encode())):
            self.assertEqual(load({'url': self.audio['flac'].as_uri()})['status'], 'error')
        with self.assertRaises(ValueError):
            parse_lrc('\n'.join(f'[{i}:00]line' for i in range(5001)))

    def test_invalid_requests(self):
        for request in [None, [], {'title': 1}, {'player': None}, {'duration': 'bad'}]:
            self.assertEqual(load(request)['reason'], 'invalid_request')
        self.assertEqual(load({})['reason'], 'no_track')

    def test_cli_json_protocol(self):
        for data in [b'{"title": 1}', b'bad json', b'x' * 16385]:
            result = subprocess.run([sys.executable, str(ROOT/'lyrics.py')], input=data, capture_output=True, check=True)
            self.assertEqual(json.loads(result.stdout)['status'], 'error')
            self.assertEqual(result.stderr, b'')

    def test_timeline_boundaries_seek_and_offset(self):
        script = """
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[1], 'utf8'), ctx);
const lines = [{time: 1}, {time: 3.5}, {time: 8}];
assert.equal(ctx.lineAt(lines, 0, 0), -1);
assert.equal(ctx.lineAt(lines, 1, 0), 0);
assert.equal(ctx.lineAt(lines, 3.499, 0), 0);
assert.equal(ctx.lineAt(lines, 3.5, 0), 1);
assert.equal(ctx.lineAt(lines, 100, 0), 2);
assert.equal(ctx.lineAt(lines, 2, 0), 0);
assert.equal(ctx.lineAt(lines, 3.5, 500), 0);
assert.equal(ctx.lineAt(lines, 3, -500), 1);
assert.equal(ctx.lineAt([], 5, 0), -1);
assert.equal(ctx.clock(-1), '--:--');
assert.equal(ctx.clock(65.9), '1:05');
"""
        subprocess.run(['node', '-e', script, str(ROOT/'Timeline.js')], check=True)


if __name__ == '__main__':
    unittest.main()
