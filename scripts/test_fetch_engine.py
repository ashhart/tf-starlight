import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tarfile
import tempfile
import threading
import unittest


SOURCE = Path(__file__).with_name("fetch-engine.sh")


def archive(version="1.2.3"):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as tar:
        payload = f"#!/bin/sh\nprintf 'TensorFold {version}\\n'\n".encode()
        entry = tarfile.TarInfo("tensorfold/bin/tensorfold-native")
        entry.mode, entry.size = 0o755, len(payload)
        tar.addfile(entry, io.BytesIO(payload))
    return output.getvalue()


class FetchEngineTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="fetch-fixture-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / "scripts").mkdir()
        shutil.copyfile(SOURCE, self.root / "scripts/fetch-engine.sh")
        self.output, self.temporary = self.root / "output", self.root / "temporary"
        self.temporary.mkdir()
        self.payload = archive()
        self.actions, self.paths = [200], []
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                index = len(fixture.paths)
                fixture.paths.append(self.path)
                action = fixture.actions[min(index, len(fixture.actions) - 1)]
                body = fixture.payload if action in (200, "cut") else b"fixture HTTP failure"
                self.send_response(200 if action == "cut" else action)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body[:32] if action == "cut" else body)
                self.wfile.flush()
                if action == "cut":
                    self.connection.shutdown(socket.SHUT_RDWR)
                self.close_connection = True

            def log_message(self, *_args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.start()
        self.addCleanup(self.stop_server)
        self.pin()
        self.adapter()

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.assertFalse(self.thread.is_alive())

    def pin(self, digest=None, available=True):
        pin = {"repository": "fixture/engine", "version": "1.2.3", "archives": {}}
        if available:
            pin["archives"]["fixture"] = digest or hashlib.sha256(self.payload).hexdigest()
        (self.root / "engine.json").write_text(json.dumps(pin))

    def adapter(self):
        real_curl = shutil.which("curl")
        self.assertIsNotNone(real_curl)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        adapter = self.bin / "curl"
        adapter.write_text("#!/usr/bin/env python3\n" + '''import json,os,subprocess,sys
from pathlib import Path
from urllib.parse import urlsplit
args=sys.argv[1:]
url=urlsplit(args[-1])
expected='/fixture/engine/releases/download/v1.2.3/tensorfold-1.2.3-fixture.tar.gz'
if url.scheme!='https' or url.netloc!='github.com' or url.path!=expected or url.query:
    raise SystemExit('pinned fetch tried an unexpected URL or API lookup')
Path(os.environ['FIXTURE_INVOCATION']).write_text(json.dumps(args))
args[-1]=os.environ['FIXTURE_ENDPOINT']+url.path
raise SystemExit(subprocess.call([os.environ['FIXTURE_CURL'],'--noproxy','*',*args]))
''')
        adapter.chmod(0o755)
        self.env = os.environ | {"PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                                 "FIXTURE_CURL": real_curl,
                                 "FIXTURE_ENDPOINT": f"http://127.0.0.1:{self.server.server_port}",
                                 "FIXTURE_INVOCATION": str(self.root / "invocation.json"),
                                 "GH_TOKEN": "", "TMPDIR": str(self.temporary) + os.sep}

    def fetch(self, success, attempts):
        result = subprocess.run(["sh", str(self.root / "scripts/fetch-engine.sh"), "fixture", str(self.output)],
                                env=self.env, capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode == 0, success, result.stderr)
        self.assertEqual(len(self.paths), attempts)
        self.assertTrue(all(path.endswith("tensorfold-1.2.3-fixture.tar.gz") for path in self.paths))
        self.assertEqual(list(self.temporary.iterdir()), [], "archive was not removed")
        if success:
            self.assertEqual(result.stdout.strip(), str(self.output / "bin/tensorfold-native"))
            self.assertEqual(subprocess.check_output([self.output / "bin/tensorfold-native", "--version"],
                                                    text=True).strip(), "TensorFold 1.2.3")
        return result

    def test_complete_success_uses_direct_pin_and_verifies_binary(self):
        self.fetch(True, 1)

    def test_transient_504_recovers(self):
        self.actions = [504, 200]
        self.fetch(True, 2)

    def test_transient_503_recovers(self):
        self.actions = [503, 503, 200]
        self.fetch(True, 3)

    def test_cut_body_is_replaced_by_complete_verified_retry(self):
        self.actions = ["cut", 200]
        self.fetch(True, 2)

    def test_exhaustion_stops_after_four_attempts(self):
        self.actions = [504]
        self.fetch(False, 4)
        self.assertFalse(self.output.exists())

    def test_missing_asset_is_bounded_and_never_extracted(self):
        self.actions = [404]
        self.fetch(False, 4)
        self.assertFalse(self.output.exists())

    def test_missing_pinned_digest_never_contacts_network(self):
        self.pin(available=False)
        self.fetch(False, 0)
        self.assertFalse(self.output.exists())

    def test_wrong_complete_hash_fails_before_extraction(self):
        self.pin(digest="0" * 64)
        result = self.fetch(False, 1)
        self.assertIn("does not match SHA-256", result.stderr)
        self.assertFalse(self.output.exists())

    def test_wrong_binary_version_fails_after_valid_hash(self):
        self.payload = archive(version="9.9.9")
        self.pin()
        result = self.fetch(False, 1)
        self.assertIn("is not TensorFold 1.2.3", result.stderr)


if __name__ == "__main__":
    unittest.main()
