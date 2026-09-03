import gc
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import weakref

import introspect
import protocol


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
SCRIPT_PATH = Path(__file__).with_name("introspect.py")
PROJECT_ROOT = REPOSITORY_ROOT / "testdata" / "sample_django_project"


class ProtocolTests(unittest.TestCase):
    def test_worker_protocol_recovers_from_payload_errors(self):
        server, worker = socket.socketpair()
        result = []

        def serve():
            try:
                result.append(protocol.serve_connection(worker, str(PROJECT_ROOT), "sample_project.settings"))
            finally:
                worker.close()

        thread = threading.Thread(target=serve)
        thread.start()
        reader = protocol.FrameReader(server)
        server.sendall(b'{"broken"\n')
        parse_error = protocol.decode_object(reader.read())
        self.assertEqual(parse_error["error"]["code"], "parse_error")
        self.assertIsNone(parse_error["id"])

        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "1", "method": "unknown", "params": {}},
        )
        unknown = protocol.decode_object(reader.read())
        self.assertEqual(unknown["error"]["code"], "method_not_found")

        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "2", "method": "worker/ping", "params": {}},
        )
        pong = protocol.decode_object(reader.read())
        self.assertEqual(pong["result"], {"pong": True})

        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "3", "method": "worker/shutdown", "params": {}},
        )
        shutdown = protocol.decode_object(reader.read())
        self.assertIsNone(shutdown["result"])
        server.close()
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(result, [0])

    def test_worker_rejects_wrong_version_and_oversized_frames(self):
        server, worker = socket.socketpair()
        result = []

        def serve_version():
            result.append(protocol.serve_connection(worker, str(PROJECT_ROOT), "sample_project.settings"))
            worker.close()

        thread = threading.Thread(target=serve_version)
        thread.start()
        reader = protocol.FrameReader(server)
        protocol.write_message(
            server,
            {"protocol_version": 2, "id": "1", "method": "worker/ping", "params": {}},
        )
        response = protocol.decode_object(reader.read())
        self.assertEqual(response["error"]["code"], "protocol_version_mismatch")
        self.assertIsNone(reader.read())
        thread.join(timeout=2)
        self.assertEqual(result, [1])
        server.close()

        server, worker = socket.socketpair()
        failures = []

        def serve_oversized():
            try:
                protocol.serve_connection(worker, str(PROJECT_ROOT), "sample_project.settings")
            except protocol.FrameError as error:
                failures.append(str(error))
            finally:
                worker.close()

        thread = threading.Thread(target=serve_oversized)
        thread.start()
        server.sendall(b"x" * (protocol.MAX_IPC_FRAME_SIZE + 1))
        server.close()
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(failures, ["IPC frame exceeds maximum size"])

    def test_worker_bounds_error_ids_without_retaining_schema(self):
        oversized_id = "x" * 1024
        with self.assertRaises(protocol.PayloadError) as raised:
            protocol.validate_request(
                {"protocol_version": 1, "id": oversized_id, "method": "worker/ping", "params": {}}
            )
        self.assertIsNone(raised.exception.request_id)
        fallback = protocol.error_response(None, "response_too_large", "worker response exceeds maximum size")
        self.assertLess(len(protocol.encode_message(fallback)), 256)

        state = protocol.WorkerState(str(PROJECT_ROOT), "sample_project.settings")
        state.project_root = PROJECT_ROOT
        state.resolved_settings = "sample_project.settings"
        original_builder = introspect.build_snapshot
        calls = []

        def fake_builder(project_root, settings_name):
            calls.append((project_root, settings_name))
            return {"schema_version": 3, "apps": {}}

        introspect.build_snapshot = fake_builder
        try:
            first = state.dump_schema()
            second = state.dump_schema()
        finally:
            introspect.build_snapshot = original_builder
        self.assertIsNot(first, second)
        self.assertEqual(len(calls), 2)

    def test_schema_load_error_surfaces_the_underlying_exception_detail(self):
        class FailingState:
            def dump_schema(self):
                raise RuntimeError("multiple Django settings modules found; pass --settings (a.settings, b.settings)")

        response, should_stop = protocol.dispatch_request(
            {"protocol_version": 1, "id": "1", "method": "schema/load", "params": {}},
            FailingState(),
        )
        self.assertTrue(should_stop)
        self.assertEqual(response["error"]["code"], "introspection_failed")
        self.assertIn("RuntimeError", response["error"]["message"])
        self.assertIn("multiple Django settings modules found", response["error"]["message"])

    def test_worker_releases_schema_response_before_waiting_for_next_request(self):
        class Snapshot(dict):
            pass

        class ReleasableState:
            reference = None

            def dump_schema(self):
                snapshot = Snapshot(schema_version=2, apps={})
                self.reference = weakref.ref(snapshot)
                return snapshot

        state = ReleasableState()
        server, worker = socket.socketpair()
        result = []

        def serve():
            result.append(
                protocol.serve_connection(
                    worker,
                    str(PROJECT_ROOT),
                    "sample_project.settings",
                    worker_state=state,
                )
            )
            worker.close()

        thread = threading.Thread(target=serve)
        thread.start()
        reader = protocol.FrameReader(server)
        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "load", "method": "schema/load", "params": {}},
        )
        self.assertEqual(protocol.decode_object(reader.read())["id"], "load")
        for _ in range(100):
            gc.collect()
            if state.reference() is None:
                break
            time.sleep(0.01)
        self.assertIsNone(state.reference())
        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "stop", "method": "worker/shutdown", "params": {}},
        )
        self.assertEqual(protocol.decode_object(reader.read())["id"], "stop")
        server.close()
        thread.join(timeout=2)
        self.assertEqual(result, [0])

    def test_oversized_schema_returns_correlated_structured_error(self):
        class OversizedState:
            def dump_schema(self):
                return {"value": "x" * protocol.MAX_IPC_FRAME_SIZE}

        server, worker = socket.socketpair()
        result = []

        def serve():
            result.append(
                protocol.serve_connection(
                    worker,
                    str(PROJECT_ROOT),
                    "sample_project.settings",
                    worker_state=OversizedState(),
                )
            )
            worker.close()

        thread = threading.Thread(target=serve)
        thread.start()
        reader = protocol.FrameReader(server)
        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "bounded-id", "method": "schema/load", "params": {}},
        )
        response = protocol.decode_object(reader.read())
        self.assertEqual(response["id"], "bounded-id")
        self.assertEqual(response["error"]["code"], "response_too_large")
        protocol.write_message(
            server,
            {"protocol_version": 1, "id": "2", "method": "worker/shutdown", "params": {}},
        )
        self.assertEqual(protocol.decode_object(reader.read())["id"], "2")
        server.close()
        thread.join(timeout=2)
        self.assertEqual(result, [0])

    def test_schema_response_can_exceed_legacy_frame_limit(self):
        legacy_maximum = 8 * 1024 * 1024
        measured_quera_schema_size = 14_081_193
        response = protocol.success_response(
            "large-schema",
            {"value": "x" * measured_quera_schema_size},
        )
        payload = protocol.encode_message(response)
        self.assertGreater(len(payload), legacy_maximum)
        self.assertGreater(len(payload), measured_quera_schema_size)
        self.assertLess(len(payload), protocol.MAX_IPC_FRAME_SIZE)

    def test_encode_message_is_compact_utf8_and_bounded(self):
        message = {"text": "héllo <world>", "values": [1, True, None]}
        expected = json.dumps(
            message,
            ensure_ascii=False,
            allow_nan=False,
            separators=(",", ":"),
        ).encode("utf-8") + b"\n"
        self.assertEqual(protocol.encode_message(message), expected)

        with self.assertRaises(ValueError):
            protocol.encode_message({"value": float("nan")})
        with self.assertRaises(protocol.FrameError):
            protocol.encode_message({"value": "x" * protocol.MAX_IPC_FRAME_SIZE})

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "Unix sockets are unavailable")
    def test_authenticated_worker_subprocess_hides_endpoint_environment(self):
        with tempfile.TemporaryDirectory(prefix="pogo-python-worker-") as temporary:
            project = Path(temporary).resolve()
            (project / "settings.py").write_text(
                """import os

worker_variables = sorted(name for name in os.environ if name.startswith("POGO_WORKER_"))
if worker_variables:
    raise RuntimeError(f"worker endpoint environment leaked: {', '.join(worker_variables)}")

print("settings startup output")
SECRET_KEY = "fixture"
INSTALLED_APPS = []
DATABASES = {"default": {"ENGINE": "django.db.backends.sqlite3", "NAME": ":memory:"}}
DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"
""",
                encoding="utf-8",
            )
            socket_path = str(project / "worker.sock")
            token = "file-token-not-for-logs"
            direct_token = "direct-token-not-for-logs"
            token_path = project / "token"
            token_path.write_text(token, encoding="ascii")
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(socket_path)
            listener.listen(1)
            environment = os.environ.copy()
            environment.pop("DJANGO_SETTINGS_MODULE", None)
            environment.update(
                {
                    protocol.WORKER_NETWORK_ENV: "unix",
                    protocol.WORKER_ADDRESS_ENV: socket_path,
                    protocol.WORKER_TOKEN_ENV: direct_token,
                    protocol.WORKER_TOKEN_FILE_ENV: str(token_path),
                    "PYTHONDONTWRITEBYTECODE": "1",
                }
            )
            process = subprocess.Popen(
                [
                    sys.executable,
                    str(SCRIPT_PATH),
                    "--project",
                    str(project),
                    "--settings",
                    "settings",
                    "--connect",
                ],
                cwd=REPOSITORY_ROOT,
                env=environment,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            connection, _ = listener.accept()
            reader = protocol.FrameReader(connection)
            hello = protocol.decode_object(reader.read())
            self.assertEqual(hello, {"protocol_version": 1, "type": "hello", "token": token})
            self.assertFalse(token_path.exists())

            protocol.write_message(
                connection,
                {"protocol_version": 1, "id": "1", "method": "worker/ping", "params": {}},
            )
            self.assertEqual(protocol.decode_object(reader.read())["result"], {"pong": True})
            protocol.write_message(
                connection,
                {"protocol_version": 1, "id": "2", "method": "schema/load", "params": {}},
            )
            snapshot = protocol.decode_object(reader.read())["result"]
            self.assertEqual(snapshot["schema_version"], 3)
            self.assertEqual(snapshot["apps"], {})
            protocol.write_message(
                connection,
                {"protocol_version": 1, "id": "3", "method": "worker/shutdown", "params": {}},
            )
            self.assertIsNone(protocol.decode_object(reader.read())["result"])
            connection.close()
            listener.close()
            stdout, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual(stdout, b"")
            self.assertIn(b"settings startup output", stderr)
            self.assertNotIn(token.encode(), stderr)
            self.assertNotIn(direct_token.encode(), stderr)
            self.assertTrue(Path(socket_path).exists())


if __name__ == "__main__":
    unittest.main()
