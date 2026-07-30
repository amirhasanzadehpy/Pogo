import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest

import introspect


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
SCRIPT_PATH = Path(__file__).with_name("introspect.py")
PROJECT_ROOT = REPOSITORY_ROOT / "testdata" / "sample_django_project"
MODELS_PATH = str((PROJECT_ROOT / "myapp" / "models.py").resolve())


class IntrospectionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.first = run_dump()
        cls.second = run_dump()
        cls.pretty = run_dump("--pretty")
        cls.snapshot = json.loads(cls.first.stdout)

    def test_compact_and_pretty_output_are_deterministic(self):
        self.assertEqual(self.first.returncode, 0, self.first.stderr)
        self.assertEqual(self.second.returncode, 0, self.second.stderr)
        self.assertEqual(self.pretty.returncode, 0, self.pretty.stderr)
        self.assertEqual(self.first.stderr, "")
        self.assertEqual(self.second.stderr, "")
        self.assertEqual(self.pretty.stderr, "")
        self.assertEqual(self.first.stdout, self.second.stdout)
        self.assertEqual(self.first.stdout.count("\n"), 1)
        self.assertTrue(self.first.stdout.endswith("\n"))
        self.assertEqual(json.loads(self.pretty.stdout), self.snapshot)
        explicit = run_dump("--compact", "--settings", "sample_project.settings")
        self.assertEqual(explicit.returncode, 0, explicit.stderr)
        self.assertEqual(explicit.stdout, self.first.stdout)

    def test_schema_shape_and_model_inventory(self):
        snapshot = self.snapshot
        self.assertEqual(
            list(snapshot),
            [
                "schema_version",
                "position_encoding",
                "lookup_transform_max_depth",
                "lookup_path_max_count",
                "schema_sources",
                "apps",
            ],
        )
        self.assertEqual(snapshot["schema_version"], 1)
        self.assertEqual(snapshot["position_encoding"], "utf-8-bytes")
        self.assertEqual(snapshot["lookup_transform_max_depth"], 2)
        self.assertEqual(snapshot["lookup_path_max_count"], 512)
        self.assertEqual(list(snapshot["apps"]), ["myapp"])
        app = snapshot["apps"]["myapp"]
        self.assertEqual(set(app), {"label", "import_name", "root_path", "models"})
        self.assertEqual(app["label"], "myapp")
        self.assertEqual(app["import_name"], "myapp")
        self.assertTrue(Path(app["root_path"]).is_absolute())
        self.assertEqual(
            list(app["models"]),
            ["Author", "Book", "Node", "Profile", "Publication", "SpecialEdition", "Store"],
        )
        self.assertEqual(
            snapshot["schema_sources"],
            [
                MODELS_PATH,
                str((PROJECT_ROOT / "sample_project" / "settings.py").resolve()),
            ],
        )

        model_keys = {
            "canonical_label",
            "module",
            "qualname",
            "file_path",
            "line_number",
            "source_range",
            "docstring",
            "abstract",
            "proxy",
            "managed",
            "swapped",
            "has_abstract_parent",
            "multi_table_child",
            "parents",
            "default_manager",
            "base_manager",
            "custom_managers",
            "managers",
            "queryset_methods",
            "indexes",
            "constraints",
            "fields",
        }
        field_keys = {
            "type",
            "is_relation",
            "related_model",
            "runtime_related_model",
            "lookups",
            "unsupported_lookups",
            "help_text",
            "name",
            "attname",
            "db_column",
            "db_type",
            "internal_type",
            "null",
            "db_index",
            "unique",
            "primary_key",
            "effective_primary_key",
            "concrete",
            "auto_created",
            "relation_cardinality",
            "relation_direction",
            "accessor_name",
            "query_name",
            "source_model",
            "source_model_abstract",
            "source_range",
            "parent_link",
            "transforms",
            "lookup_paths",
            "lookup_paths_truncated",
        }
        manager_keys = {
            "name",
            "owner_class",
            "queryset_class",
            "default",
            "local",
            "auto_created",
            "source_range",
            "methods",
        }
        method_keys = {
            "name",
            "owner_class",
            "signature",
            "docstring",
            "source_range",
            "chainable",
            "assumed_chainable",
        }
        known_models = {
            model["canonical_label"]
            for application in snapshot["apps"].values()
            for model in application["models"].values()
        }
        for model in app["models"].values():
            self.assertEqual(set(model), model_keys)
            self.assertEqual(model["file_path"], MODELS_PATH)
            self.assertEqual(model["line_number"], model["source_range"]["start"]["line"])
            self.assertEqual(list(model["fields"]), sorted(model["fields"]))
            for parent in model["parents"]:
                if not parent["abstract"]:
                    self.assertIn(parent["canonical_label"], known_models)
            self.assertEqual(set(model["base_manager"]), {"name", "owner_class"})
            for manager in model["managers"]:
                self.assertEqual(set(manager), manager_keys)
                for method in manager["methods"]:
                    self.assertEqual(set(method), method_keys)
            for method in model["queryset_methods"]:
                self.assertEqual(set(method), method_keys)
            for field in model["fields"].values():
                self.assertEqual(set(field), field_keys)
                self.assertEqual(field["lookups"], sorted(field["lookups"]))
                self.assertLessEqual(
                    max((len(path["transforms"]) for path in field["lookup_paths"]), default=0),
                    snapshot["lookup_transform_max_depth"],
                )
                self.assertLessEqual(len(field["lookup_paths"]), snapshot["lookup_path_max_count"])
                if field["related_model"] is not None:
                    self.assertIn(field["related_model"], known_models)

    def test_book_author_golden_metadata(self):
        author = models(self.snapshot)["Book"]["fields"]["author"]
        golden = {
            "type": "django.db.models.fields.related.ForeignKey",
            "is_relation": True,
            "related_model": "myapp.Author",
            "help_text": "Author who wrote the book.",
            "name": "author",
            "attname": "author_id",
            "db_column": "author_id",
            "db_type": "bigint",
            "internal_type": "ForeignKey",
            "null": False,
            "db_index": True,
            "unique": False,
            "primary_key": False,
            "concrete": True,
            "auto_created": False,
            "relation_cardinality": "many-to-one",
            "relation_direction": "forward",
            "accessor_name": "books",
            "query_name": "book",
            "source_model": "myapp.Book",
            "source_range": source_range(64, 4, 70, 5),
            "parent_link": False,
        }
        for key, expected in golden.items():
            self.assertEqual(author[key], expected, key)
        self.assertEqual(author["lookups"], ["exact", "gt", "gte", "in", "isnull", "lt", "lte"])

    def test_reverse_relation_names_and_sources(self):
        fixture_models = models(self.snapshot)
        cases = [
            ("Author", "book", "myapp.Book", "one-to-many", "book", "books", "myapp.Book", source_range(64, 4, 70, 5)),
            ("Author", "profile", "myapp.Profile", "one-to-one", "profile", "profile", "myapp.Profile", source_range(31, 4, 37, 5)),
            ("Book", "store", "myapp.Store", "many-to-many", "store", "store_set", "myapp.Store", source_range(117, 4, 121, 5)),
            ("Node", "node", "myapp.Node", "one-to-many", "node", "node_set", "myapp.Node", source_range(142, 4, 148, 5)),
            (
                "Publication",
                "specialedition",
                "myapp.SpecialEdition",
                "one-to-one",
                "specialedition",
                "specialedition",
                "myapp.SpecialEdition",
                source_range(131, 0, 134, 5),
            ),
        ]
        for model_name, field_name, related, cardinality, query, accessor, source_model, expected_range in cases:
            with self.subTest(model=model_name, field=field_name):
                field = fixture_models[model_name]["fields"][field_name]
                self.assertTrue(field["is_relation"])
                self.assertEqual(field["relation_direction"], "reverse")
                self.assertEqual(field["related_model"], related)
                self.assertEqual(field["relation_cardinality"], cardinality)
                self.assertEqual(field["query_name"], query)
                self.assertEqual(field["accessor_name"], accessor)
                self.assertEqual(field["source_model"], source_model)
                self.assertEqual(field["source_range"], expected_range)

    def test_abstract_and_multi_table_inheritance_sources(self):
        fixture_models = models(self.snapshot)
        book = fixture_models["Book"]
        self.assertTrue(book["has_abstract_parent"])
        self.assertEqual(book["fields"]["created_at"]["source_model"], "myapp.TimeStampedModel")
        self.assertTrue(book["fields"]["created_at"]["source_model_abstract"])
        self.assertEqual(book["fields"]["created_at"]["source_range"], source_range(8, 4, 12, 5))

        special = fixture_models["SpecialEdition"]
        self.assertTrue(special["multi_table_child"])
        self.assertEqual(special["parents"][0]["canonical_label"], "myapp.Publication")
        self.assertEqual(special["parents"][0]["parent_link"], "publication_ptr")
        self.assertEqual(special["fields"]["title"]["source_model"], "myapp.Publication")
        self.assertEqual(special["fields"]["title"]["source_range"], source_range(125, 4, 128, 5))
        self.assertEqual(special["fields"]["created_at"]["source_model"], "myapp.TimeStampedModel")

        pointer = special["fields"]["publication_ptr"]
        self.assertTrue(pointer["parent_link"])
        self.assertTrue(pointer["primary_key"])
        self.assertTrue(pointer["effective_primary_key"])
        self.assertTrue(pointer["auto_created"])
        self.assertEqual(pointer["related_model"], "myapp.Publication")
        self.assertEqual(pointer["source_range"], special["source_range"])

    def test_managers_queryset_methods_and_safe_signatures(self):
        book = models(self.snapshot)["Book"]
        self.assertEqual(book["custom_managers"], ["catalog", "objects"])
        managers_by_name = {manager["name"]: manager for manager in book["managers"]}
        self.assertEqual(list(managers_by_name), ["catalog", "objects"])
        self.assertEqual(managers_by_name["objects"]["queryset_class"], "myapp.models.BookQuerySet")
        self.assertTrue(managers_by_name["objects"]["default"])
        self.assertEqual(managers_by_name["objects"]["methods"], [])
        self.assertEqual(book["default_manager"], "objects")
        self.assertEqual(book["base_manager"], {"name": "_base_manager", "owner_class": "django.db.models.manager.Manager"})

        featured = managers_by_name["catalog"]["methods"][0]
        self.assertEqual(
            {key: featured[key] for key in ("name", "owner_class", "signature", "docstring", "chainable", "assumed_chainable")},
            {
                "name": "featured",
                "owner_class": "myapp.models.BookManager",
                "signature": "() -> \"models.QuerySet['Book']\"",
                "docstring": "Return books marked as featured in their JSON metadata.",
                "chainable": True,
                "assumed_chainable": False,
            },
        )
        self.assertEqual(featured["source_range"], source_range(58, 4, 60, 66))

        methods_by_name = {method["name"]: method for method in book["queryset_methods"]}
        self.assertEqual(list(methods_by_name), ["active", "published"])
        self.assertEqual(methods_by_name["active"]["signature"], "()")
        self.assertTrue(methods_by_name["active"]["chainable"])
        self.assertTrue(methods_by_name["active"]["assumed_chainable"])
        self.assertEqual(methods_by_name["active"]["source_range"], source_range(45, 4, 47, 42))
        self.assertEqual(methods_by_name["published"]["source_range"], source_range(49, 4, 54, 9))

    def test_datetime_and_json_lookup_paths(self):
        fields = models(self.snapshot)["Book"]["fields"]
        published = fields["published_at"]
        self.assertIn("date", published["transforms"])
        self.assertIn("year", published["transforms"])
        path_map = {tuple(path["transforms"]): path for path in published["lookup_paths"]}
        self.assertIn("gte", path_map[()]["lookups"])
        self.assertIn("gte", path_map[("date",)]["lookups"])
        self.assertIn("gte", path_map[("date", "year")]["lookups"])
        self.assertIn("gte", path_map[("year",)]["lookups"])

        metadata = fields["metadata"]
        self.assertNotIn("contains", metadata["lookups"])
        self.assertEqual(metadata["unsupported_lookups"], ["contained_by", "contains"])
        self.assertIn("has_key", metadata["lookups"])
        json_paths = {tuple(path["transforms"]): path for path in metadata["lookup_paths"]}
        self.assertEqual(json_paths[("*",)]["kinds"], ["key_transform"])
        self.assertIn("exact", json_paths[("*",)]["lookups"])
        self.assertEqual(json_paths[("*", "*")]["kinds"], ["key_transform", "key_transform"])
        self.assertIn("icontains", json_paths[("*", "*")]["lookups"])
        self.assertFalse(metadata["lookup_paths_truncated"])

    def test_indexes_constraints_and_database_metadata(self):
        fixture_models = models(self.snapshot)
        book = fixture_models["Book"]
        self.assertEqual(
            book["indexes"],
            [
                {
                    "name": "book_author_pub_idx",
                    "fields": [
                        {"name": "author", "order": "asc"},
                        {"name": "published_at", "order": "asc"},
                    ],
                    "expressions": [],
                    "condition": None,
                    "include": [],
                    "opclasses": [],
                    "db_tablespace": None,
                    "source_range": source_range(104, 12, 107, 13),
                }
            ],
        )
        self.assertTrue(all(model["constraints"] == [] for model in fixture_models.values()))
        self.assertEqual(book["fields"]["published_at"]["db_type"], "datetime")
        self.assertEqual(book["fields"]["metadata"]["db_type"], "text")
        self.assertEqual(book["fields"]["page_count"]["db_type"], "integer")
        self.assertIsNone(book["fields"]["store"]["db_type"])

    def test_bootstrap_errors_do_not_write_json_stdout(self):
        with tempfile.TemporaryDirectory(prefix="pogo-invalid-project-") as temporary:
            result = run_command("--project", temporary)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("introspection error:", result.stderr)

        reserved = run_dump("--connect")
        self.assertNotEqual(reserved.returncode, 0)
        self.assertEqual(reserved.stdout, "")
        self.assertIn("worker endpoint environment is incomplete", reserved.stderr)

    def test_worker_protocol_recovers_from_payload_errors(self):
        server, worker = socket.socketpair()
        result = []

        def serve():
            try:
                result.append(introspect.serve_connection(worker, str(PROJECT_ROOT), "sample_project.settings"))
            finally:
                worker.close()

        thread = threading.Thread(target=serve)
        thread.start()
        reader = introspect.FrameReader(server)
        server.sendall(b'{"broken"\n')
        parse_error = introspect.decode_object(reader.read())
        self.assertEqual(parse_error["error"]["code"], "parse_error")
        self.assertIsNone(parse_error["id"])

        introspect.write_message(
            server,
            {"protocol_version": 1, "id": "1", "method": "unknown", "params": {}},
        )
        unknown = introspect.decode_object(reader.read())
        self.assertEqual(unknown["error"]["code"], "method_not_found")

        introspect.write_message(
            server,
            {"protocol_version": 1, "id": "2", "method": "worker/ping", "params": {}},
        )
        pong = introspect.decode_object(reader.read())
        self.assertEqual(pong["result"], {"pong": True})

        introspect.write_message(
            server,
            {"protocol_version": 1, "id": "3", "method": "worker/shutdown", "params": {}},
        )
        shutdown = introspect.decode_object(reader.read())
        self.assertIsNone(shutdown["result"])
        server.close()
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(result, [0])

    def test_worker_rejects_wrong_version_and_oversized_frames(self):
        server, worker = socket.socketpair()
        result = []

        def serve_version():
            result.append(introspect.serve_connection(worker, str(PROJECT_ROOT), "sample_project.settings"))
            worker.close()

        thread = threading.Thread(target=serve_version)
        thread.start()
        reader = introspect.FrameReader(server)
        introspect.write_message(
            server,
            {"protocol_version": 2, "id": "1", "method": "worker/ping", "params": {}},
        )
        response = introspect.decode_object(reader.read())
        self.assertEqual(response["error"]["code"], "protocol_version_mismatch")
        self.assertIsNone(reader.read())
        thread.join(timeout=2)
        self.assertEqual(result, [1])
        server.close()

        server, worker = socket.socketpair()
        failures = []

        def serve_oversized():
            try:
                introspect.serve_connection(worker, str(PROJECT_ROOT), "sample_project.settings")
            except introspect.FrameError as error:
                failures.append(str(error))
            finally:
                worker.close()

        thread = threading.Thread(target=serve_oversized)
        thread.start()
        server.sendall(b"x" * (introspect.MAX_IPC_FRAME_SIZE + 1))
        server.close()
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(failures, ["IPC frame exceeds maximum size"])

    def test_worker_bounds_error_ids_and_caches_schema(self):
        oversized_id = "x" * 1024
        with self.assertRaises(introspect.PayloadError) as raised:
            introspect.validate_request(
                {"protocol_version": 1, "id": oversized_id, "method": "worker/ping", "params": {}}
            )
        self.assertIsNone(raised.exception.request_id)
        fallback = introspect.error_response(None, "response_too_large", "worker response exceeds maximum size")
        self.assertLess(len(introspect.encode_message(fallback)), 256)

        state = introspect.WorkerState(str(PROJECT_ROOT), "sample_project.settings")
        state.project_root = PROJECT_ROOT
        state.resolved_settings = "sample_project.settings"
        original_builder = introspect.build_snapshot
        calls = []

        def fake_builder(project_root, settings_name):
            calls.append((project_root, settings_name))
            return {"schema_version": 1, "apps": {}}

        introspect.build_snapshot = fake_builder
        try:
            first = state.dump_schema()
            second = state.dump_schema()
        finally:
            introspect.build_snapshot = original_builder
        self.assertIs(first, second)
        self.assertEqual(len(calls), 1)

    def test_oversized_schema_returns_correlated_structured_error(self):
        class OversizedState:
            def dump_schema(self):
                return {"value": "x" * introspect.MAX_IPC_FRAME_SIZE}

        server, worker = socket.socketpair()
        result = []

        def serve():
            result.append(
                introspect.serve_connection(
                    worker,
                    str(PROJECT_ROOT),
                    "sample_project.settings",
                    worker_state=OversizedState(),
                )
            )
            worker.close()

        thread = threading.Thread(target=serve)
        thread.start()
        reader = introspect.FrameReader(server)
        introspect.write_message(
            server,
            {"protocol_version": 1, "id": "bounded-id", "method": "schema/load", "params": {}},
        )
        response = introspect.decode_object(reader.read())
        self.assertEqual(response["id"], "bounded-id")
        self.assertEqual(response["error"]["code"], "response_too_large")
        introspect.write_message(
            server,
            {"protocol_version": 1, "id": "2", "method": "worker/shutdown", "params": {}},
        )
        self.assertEqual(introspect.decode_object(reader.read())["id"], "2")
        server.close()
        thread.join(timeout=2)
        self.assertEqual(result, [0])

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "Unix sockets are unavailable")
    def test_authenticated_worker_subprocess_loads_schema_and_shuts_down(self):
        with tempfile.TemporaryDirectory(prefix="pogo-python-worker-") as temporary:
            socket_path = str(Path(temporary) / "worker.sock")
            token = "test-token-not-for-logs"
            token_path = Path(temporary) / "token"
            token_path.write_text(token, encoding="ascii")
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(socket_path)
            listener.listen(1)
            environment = os.environ.copy()
            environment.pop("DJANGO_SETTINGS_MODULE", None)
            environment.update(
                {
                    introspect.WORKER_NETWORK_ENV: "unix",
                    introspect.WORKER_ADDRESS_ENV: socket_path,
                    introspect.WORKER_TOKEN_FILE_ENV: str(token_path),
                    "PYTHONDONTWRITEBYTECODE": "1",
                }
            )
            process = subprocess.Popen(
                [
                    sys.executable,
                    str(SCRIPT_PATH),
                    "--project",
                    str(PROJECT_ROOT),
                    "--settings",
                    "sample_project.settings",
                    "--connect",
                ],
                cwd=REPOSITORY_ROOT,
                env=environment,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            connection, _ = listener.accept()
            reader = introspect.FrameReader(connection)
            hello = introspect.decode_object(reader.read())
            self.assertEqual(hello, {"protocol_version": 1, "type": "hello", "token": token})
            self.assertFalse(token_path.exists())

            introspect.write_message(
                connection,
                {"protocol_version": 1, "id": "1", "method": "worker/ping", "params": {}},
            )
            self.assertEqual(introspect.decode_object(reader.read())["result"], {"pong": True})
            introspect.write_message(
                connection,
                {"protocol_version": 1, "id": "2", "method": "schema/load", "params": {}},
            )
            snapshot = introspect.decode_object(reader.read())["result"]
            self.assertEqual(snapshot["schema_version"], 1)
            self.assertEqual(list(snapshot["apps"]["myapp"]["models"]), list(models(self.snapshot)))
            introspect.write_message(
                connection,
                {"protocol_version": 1, "id": "3", "method": "worker/shutdown", "params": {}},
            )
            self.assertIsNone(introspect.decode_object(reader.read())["result"])
            connection.close()
            listener.close()
            stdout, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual(stdout, b"")
            self.assertNotIn(token.encode(), stderr)
            self.assertTrue(Path(socket_path).exists())

    def test_startup_output_proxy_generic_relation_constraints_and_method_safety(self):
        with tempfile.TemporaryDirectory(prefix="pogo-complex-project-") as temporary:
            project = Path(temporary).resolve()
            create_complex_project(project)
            result = run_command("--project", str(project), "--settings", "settings")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.startswith("{"))
        self.assertEqual(result.stdout.count("\n"), 1)
        self.assertIn("settings startup output", result.stderr)
        self.assertIn("models startup output", result.stderr)
        snapshot = json.loads(result.stdout)
        sample_models = snapshot["apps"]["sampleapp"]["models"]
        self.assertNotIn("TargetProxy", sample_models)

        target_relation = sample_models["Reference"]["fields"]["target"]
        self.assertEqual(target_relation["related_model"], "sampleapp.Target")
        self.assertEqual(target_relation["runtime_related_model"], "sampleapp.TargetProxy")
        known = {
            model["canonical_label"]
            for app in snapshot["apps"].values()
            for model in app["models"].values()
        }
        self.assertIn(target_relation["related_model"], known)

        generic = sample_models["TaggedItem"]["fields"]["content_object"]
        self.assertEqual(generic["internal_type"], "GenericForeignKey")
        self.assertTrue(generic["is_relation"])
        self.assertIsNone(generic["related_model"])
        self.assertIsNone(generic["accessor_name"])

        target = sample_models["Target"]
        self.assertEqual(
            [constraint["name"] for constraint in target["constraints"]],
            ["target_name_nonempty", "target_name_unique"],
        )
        self.assertEqual(
            [method["name"] for method in target["queryset_methods"]],
            ["explode", "nullable", "optional", "static_chain", "wrapped"],
        )
        methods_by_name = {method["name"]: method for method in target["queryset_methods"]}
        for method_name in ("nullable", "optional", "wrapped"):
            self.assertFalse(methods_by_name[method_name]["chainable"])
            self.assertFalse(methods_by_name[method_name]["assumed_chainable"])
        dangerous = next(manager for manager in target["managers"] if manager["name"] == "danger")
        self.assertEqual([method["name"] for method in dangerous["methods"]], ["explode"])
        constraints = {constraint["name"]: constraint for constraint in target["constraints"]}
        self.assertEqual(
            set(constraints["target_name_nonempty"]),
            {
                "name",
                "type",
                "fields",
                "expressions",
                "condition",
                "include",
                "opclasses",
                "deferrable",
                "nulls_distinct",
                "violation_error_code",
                "violation_error_message",
                "source_range",
            },
        )
        self.assertEqual(constraints["target_name_nonempty"]["type"], "django.db.models.constraints.CheckConstraint")
        self.assertIn("name__gt", constraints["target_name_nonempty"]["condition"])
        self.assertEqual(constraints["target_name_unique"]["fields"], ["name"])
        self.assertEqual(constraints["target_name_nonempty"]["source_range"]["start"]["line"], 43)
        self.assertEqual(constraints["target_name_nonempty"]["source_range"]["end"]["line"], 43)
        for source in snapshot["schema_sources"]:
            self.assertTrue(Path(source).is_relative_to(project))


def models(snapshot):
    return snapshot["apps"]["myapp"]["models"]


def source_range(start_line, start_column, end_line, end_column):
    return {
        "file_path": MODELS_PATH,
        "start": {"line": start_line, "column": start_column},
        "end": {"line": end_line, "column": end_column},
    }


def run_dump(*extra_arguments):
    return run_command("--project", str(PROJECT_ROOT), *extra_arguments)


def run_command(*arguments):
    environment = os.environ.copy()
    environment.pop("DJANGO_SETTINGS_MODULE", None)
    environment["PYTHONDONTWRITEBYTECODE"] = "1"
    return subprocess.run(
        [sys.executable, str(SCRIPT_PATH), *arguments],
        cwd=REPOSITORY_ROOT,
        env=environment,
        text=True,
        capture_output=True,
        timeout=20,
        check=False,
    )


def create_complex_project(project):
    app = project / "sampleapp"
    app.mkdir()
    (app / "__init__.py").write_text("", encoding="utf-8")
    (project / "settings.py").write_text(
        "\n".join(
            [
                'print("settings startup output")',
                'SECRET_KEY = "fixture"',
                'INSTALLED_APPS = ["django.contrib.contenttypes", "sampleapp"]',
                'DATABASES = {"default": {"ENGINE": "django.db.backends.sqlite3", "NAME": ":memory:"}}',
                'DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"',
                "",
            ]
        ),
        encoding="utf-8",
    )
    (app / "models.py").write_text(
        """from __future__ import annotations

print("models startup output")

from django.contrib.contenttypes.fields import GenericForeignKey
from django.contrib.contenttypes.models import ContentType
from django.db import models
from django.db.models import Q
import django
import typing


class DangerousQuerySet(models.QuerySet):
    def explode(self):
        raise RuntimeError("queryset method was executed")

    @staticmethod
    def static_chain() -> models.QuerySet:
        raise RuntimeError("static queryset method was executed")

    def nullable(self) -> models.QuerySet | None:
        raise RuntimeError("nullable queryset method was executed")

    def wrapped(self) -> list[models.QuerySet]:
        raise RuntimeError("wrapped queryset method was executed")

    def optional(self) -> typing.Optional[models.QuerySet]:
        raise RuntimeError("optional queryset method was executed")


class DangerousManager(models.Manager):
    def explode(self) -> models.QuerySet["Target"]:
        raise RuntimeError("manager method was executed")


class Target(models.Model):
    name = models.CharField(max_length=100)
    objects = DangerousQuerySet.as_manager()
    danger = DangerousManager()

    class Meta:
        constraints = [
            models.CheckConstraint(**({"condition": Q(name__gt="")} if django.VERSION >= (5, 1) else {"check": Q(name__gt="")}), name="target_name_nonempty"),
            models.UniqueConstraint(fields=["name"], name="target_name_unique"),
        ]


class TargetProxy(Target):
    class Meta:
        proxy = True


class Reference(models.Model):
    target = models.ForeignKey(TargetProxy, on_delete=models.CASCADE)


class TaggedItem(models.Model):
    content_type = models.ForeignKey(ContentType, on_delete=models.CASCADE)
    object_id = models.PositiveIntegerField()
    content_object = GenericForeignKey()
""",
        encoding="utf-8",
    )


if __name__ == "__main__":
    unittest.main()
