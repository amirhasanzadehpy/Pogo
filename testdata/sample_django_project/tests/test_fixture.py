import io
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest


FIXTURE_ROOT = Path(__file__).resolve().parents[1]
if str(FIXTURE_ROOT) not in sys.path:
    sys.path.insert(0, str(FIXTURE_ROOT))

os.environ["DJANGO_SETTINGS_MODULE"] = "sample_project.settings"
os.environ["POGO_FIXTURE_DB"] = ":memory:"

import django


django.setup()

from django.apps import apps
from django.core.management import call_command
from django.db import models

from myapp.models import (
    Author,
    Book,
    BookQuerySet,
    Node,
    Profile,
    Publication,
    SpecialEdition,
    Store,
    TimeStampedModel,
)
from myapp import aliased_query_examples, query_examples


class FixtureTests(unittest.TestCase):
    def test_boot_and_registry(self):
        self.assertTrue(apps.ready)
        registered = {
            model.__name__ for model in apps.get_app_config("myapp").get_models()
        }
        self.assertEqual(
            registered,
            {
                "Author",
                "Profile",
                "Book",
                "Store",
                "Publication",
                "SpecialEdition",
                "Node",
            },
        )
        self.assertTrue(TimeStampedModel._meta.abstract)
        self.assertNotIn(TimeStampedModel, apps.get_models())

    def test_reverse_query_and_accessor_names(self):
        cases = [
            (Author, "book", "books"),
            (Author, "profile", "profile"),
            (Book, "store", "store_set"),
            (Node, "node", "node_set"),
            (Publication, "specialedition", "specialedition"),
        ]
        for model, query_name, accessor_name in cases:
            with self.subTest(model=model.__name__, query_name=query_name):
                relation = model._meta.get_field(query_name)
                self.assertEqual(relation.name, query_name)
                self.assertEqual(relation.get_accessor_name(), accessor_name)
                self.assertTrue(hasattr(model, accessor_name))

        self.assertFalse(hasattr(Author, "book"))
        self.assertFalse(hasattr(Book, "store"))
        self.assertFalse(hasattr(Node, "node"))

    def test_queryset_and_managers(self):
        self.assertEqual(
            [manager.name for manager in Book._meta.local_managers],
            ["objects", "catalog"],
        )
        self.assertEqual(Book._default_manager.name, "objects")
        self.assertIsInstance(Book.objects.active(), BookQuerySet)
        self.assertIsInstance(Book.objects.published(), BookQuerySet)
        self.assertIsInstance(Book.catalog.featured(), models.QuerySet)
        self.assertTrue(hasattr(Book.objects, "active"))
        self.assertTrue(hasattr(Book.objects, "published"))
        self.assertTrue(hasattr(Book.catalog, "featured"))
        self.assertFalse(hasattr(Book.objects, "featured"))
        self.assertFalse(hasattr(Book.catalog, "active"))

    def test_inheritance_and_field_metadata(self):
        for model in (Author, Profile, Book, Store, Publication, Node):
            with self.subTest(model=model.__name__):
                self.assertIsNotNone(model._meta.get_field("created_at"))
                self.assertIsNotNone(model._meta.get_field("updated_at"))

        parent_link = SpecialEdition._meta.parents[Publication]
        self.assertEqual(parent_link.name, "publication_ptr")
        self.assertTrue(parent_link.remote_field.parent_link)
        self.assertTrue(parent_link.primary_key)
        self.assertTrue(parent_link.auto_created)

        self.assertTrue(Book._meta.get_field("published_at").null)
        self.assertTrue(Node._meta.get_field("parent").null)
        self.assertIs(Book._meta.get_field("metadata").default, dict)
        self.assertIsInstance(Book._meta.get_field("page_count"), models.IntegerField)
        self.assertIsInstance(Book._meta.get_field("summary"), models.TextField)
        self.assertTrue(Book._meta.get_field("is_active").db_index)
        self.assertTrue(Author._meta.get_field("name").db_index)
        self.assertTrue(Store._meta.get_field("name").unique)
        for model in (Author, Profile, Book, Store, Publication, SpecialEdition, Node):
            declared_fields = [
                *model._meta.local_fields,
                *model._meta.local_many_to_many,
            ]
            for field in declared_fields:
                if field.auto_created:
                    continue
                with self.subTest(model=model.__name__, field=field.name):
                    self.assertTrue(field.help_text)
        self.assertIn(
            "book_author_pub_idx",
            {index.name for index in Book._meta.indexes},
        )

    def test_system_checks(self):
        output = io.StringIO()
        call_command("check", stdout=output)
        self.assertIn("System check identified no issues", output.getvalue())
        print(output.getvalue().strip())

    def test_migration_consistency(self):
        output = io.StringIO()
        call_command(
            "makemigrations",
            "myapp",
            check=True,
            dry_run=True,
            stdout=output,
        )
        self.assertIn("No changes detected", output.getvalue())
        print(output.getvalue().strip())

    def test_representative_queries_construct(self):
        author = Author(pk=1, name="Ada")
        book = Book(pk=1, author=author, title="Language Servers")
        node = Node(pk=1, name="child")

        querysets = [
            *query_examples.book_queries(author),
            *query_examples.implicit_reverse_queries(book),
            *query_examples.recursive_queries(node),
            query_examples.inheritance_queries(),
        ]
        qualified, aliased, author_id = (
            aliased_query_examples.qualified_and_aliased_queries()
        )
        querysets.extend((qualified, aliased))

        self.assertIsNone(author_id)
        for queryset in querysets:
            with self.subTest(query=str(queryset.query)):
                self.assertIsInstance(queryset, models.QuerySet)

    def test_temporary_database_migration(self):
        with tempfile.TemporaryDirectory(prefix="pogo-fixture-") as temp_dir:
            database_path = Path(temp_dir) / "fixture.sqlite3"
            environment = os.environ.copy()
            environment["DJANGO_SETTINGS_MODULE"] = "sample_project.settings"
            environment["POGO_FIXTURE_DB"] = str(database_path)
            environment["PYTHONDONTWRITEBYTECODE"] = "1"
            subprocess.run(
                [
                    sys.executable,
                    str(FIXTURE_ROOT / "manage.py"),
                    "migrate",
                    "--noinput",
                    "--verbosity",
                    "1",
                ],
                cwd=FIXTURE_ROOT,
                env=environment,
                check=True,
            )

            with sqlite3.connect(database_path) as connection:
                migrations = connection.execute(
                    "SELECT app, name FROM django_migrations"
                ).fetchall()
                tables = {
                    row[0]
                    for row in connection.execute(
                        "SELECT name FROM sqlite_master WHERE type = 'table'"
                    )
                }

            self.assertIn(("myapp", "0001_initial"), migrations)
            self.assertTrue(
                {
                    "myapp_author",
                    "myapp_profile",
                    "myapp_book",
                    "myapp_store",
                    "myapp_store_books",
                    "myapp_publication",
                    "myapp_specialedition",
                    "myapp_node",
                }.issubset(tables)
            )

    def test_python_sources_compile(self):
        for source_path in FIXTURE_ROOT.rglob("*.py"):
            with self.subTest(source=str(source_path.relative_to(FIXTURE_ROOT))):
                compile(source_path.read_text(encoding="utf-8"), str(source_path), "exec")


if __name__ == "__main__":
    unittest.main()
