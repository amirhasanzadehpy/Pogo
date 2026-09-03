#!/usr/bin/env python3
from __future__ import annotations

import argparse
import ast
import contextlib
import hashlib
import importlib
import inspect
import itertools
import io
import json
import os
from pathlib import Path
import socket
import sys
import time
import tokenize
import traceback


SCHEMA_VERSION = 3
LOOKUP_TRANSFORM_MAX_DEPTH = 2
MAX_LOOKUP_PATHS = 512
PROTOCOL_VERSION = 1
MAX_IPC_FRAME_SIZE = 32 * 1024 * 1024
MAX_IMPORTED_MODULES = 32768
MAX_SCHEMA_SOURCE_MODULES = 4096
MAX_SCHEMA_SOURCE_PATH_BYTES = 1024 * 1024
WORKER_NETWORK_ENV = "POGO_WORKER_NETWORK"
WORKER_ADDRESS_ENV = "POGO_WORKER_ADDRESS"
WORKER_TOKEN_ENV = "POGO_WORKER_TOKEN"
WORKER_TOKEN_FILE_ENV = "POGO_WORKER_TOKEN_FILE"

# These public QuerySet methods return a cloned QuerySet and can safely retain
# Pogo's model/path inference for the next call in a literal chain.
QUERYSET_CHAINABLE_METHODS = frozenset(
    {
        "alias",
        "all",
        "annotate",
        "complex_filter",
        "defer",
        "difference",
        "distinct",
        "exclude",
        "extra",
        "filter",
        "intersection",
        "none",
        "only",
        "order_by",
        "prefetch_related",
        "reverse",
        "select_for_update",
        "select_related",
        "union",
        "using",
        "values",
        "values_list",
    }
)


def class_path(value):
    cls = value if inspect.isclass(value) else type(value)
    return f"{cls.__module__}.{cls.__qualname__}"


def model_label(model):
    return model._meta.label


class SourceIndex:
    def __init__(self):
        self._files = {}
        self._class_nodes = {}
        self._classes = {}
        self._digests = {}

    def class_info(self, cls):
        cached = self._classes.get(cls)
        if cached is not None:
            return cached

        path = inspect.getsourcefile(cls)
        if path is None:
            info = {"path": None, "node": None, "range": None, "assignments": {}, "methods": {}}
            self._classes[cls] = info
            return info

        path = str(Path(path).resolve())
        tree = self._parse(path)
        candidates = self._class_nodes[path].get(cls.__name__, ())
        if len(candidates) == 1:
            node = candidates[0]
        elif candidates:
            try:
                line_hint = inspect.getsourcelines(cls)[1]
            except (OSError, TypeError):
                line_hint = 0
            node = min(candidates, key=lambda item: abs(item.lineno - line_hint))
        else:
            node = None
        assignments = {}
        methods = {}
        if node is not None:
            for child in node.body:
                if isinstance(child, (ast.Assign, ast.AnnAssign)):
                    targets = child.targets if isinstance(child, ast.Assign) else [child.target]
                    for target in targets:
                        if isinstance(target, ast.Name):
                            assignments[target.id] = child
                elif isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    methods[child.name] = child
        info = {
            "path": path,
            "node": node,
            "range": self.node_range(path, node),
            "assignments": assignments,
            "methods": methods,
        }
        self._classes[cls] = info
        return info

    def assignment_range(self, cls, name):
        info = self.class_info(cls)
        return self.node_range(info["path"], info["assignments"].get(name))

    def method_range(self, cls, name):
        info = self.class_info(cls)
        return self.node_range(info["path"], info["methods"].get(name))

    def named_call_range(self, cls, assignment_name, object_name):
        info = self.class_info(cls)
        node = info["assignments"].get(assignment_name)
        if node is None:
            meta = next(
                (child for child in (info["node"].body if info["node"] else []) if isinstance(child, ast.ClassDef) and child.name == "Meta"),
                None,
            )
            if meta is not None:
                for child in meta.body:
                    if isinstance(child, ast.Assign) and any(isinstance(target, ast.Name) and target.id == assignment_name for target in child.targets):
                        node = child
                        break
        if node is None:
            return None
        for candidate in ast.walk(node):
            if not isinstance(candidate, ast.Call):
                continue
            for keyword in candidate.keywords:
                if keyword.arg == "name" and isinstance(keyword.value, ast.Constant) and keyword.value.value == object_name:
                    return self.node_range(info["path"], candidate)
        return None

    def node_range(self, path, node):
        if path is None or node is None or not hasattr(node, "end_lineno"):
            return None
        start_line = node.lineno
        start_column = node.col_offset
        decorators = getattr(node, "decorator_list", ())
        if decorators:
            first = min(decorators, key=lambda item: (item.lineno, item.col_offset))
            start_line = first.lineno
            start_column = first.col_offset
        return {
            "file_path": path,
            "source_digest": self.file_digest(path),
            "start": {"line": start_line, "column": start_column},
            "end": {"line": node.end_lineno, "column": node.end_col_offset},
        }

    def file_digest(self, path):
        cached = self._digests.get(path)
        if cached is None:
            self._parse(path)
            cached = self._digests[path]
        return cached

    def _parse(self, path):
        cached = self._files.get(path)
        if cached is not None:
            return cached
        with open(path, "rb") as source_file:
            source = source_file.read()
        encoding, _ = tokenize.detect_encoding(io.BytesIO(source).readline)
        tree = ast.parse(source.decode(encoding), filename=path)
        self._files[path] = tree
        class_nodes = {}
        for node in ast.walk(tree):
            if isinstance(node, ast.ClassDef):
                class_nodes.setdefault(node.name, []).append(node)
        self._class_nodes[path] = class_nodes
        self._digests[path] = hashlib.sha256(source).hexdigest()
        return tree

    def source_paths(self):
        return set(self._files)


def resolve_settings(project_root, explicit_settings):
    if explicit_settings:
        return explicit_settings
    environment_settings = os.environ.get("DJANGO_SETTINGS_MODULE")
    if environment_settings:
        return environment_settings

    manage_path = project_root / "manage.py"
    if manage_path.is_file():
        with tokenize.open(manage_path) as manage_file:
            tree = ast.parse(manage_file.read(), filename=str(manage_path))
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call) or len(node.args) < 2:
                continue
            function = node.func
            if not isinstance(function, ast.Attribute) or function.attr != "setdefault":
                continue
            first, second = node.args[:2]
            if (
                isinstance(first, ast.Constant)
                and first.value == "DJANGO_SETTINGS_MODULE"
                and isinstance(second, ast.Constant)
                and isinstance(second.value, str)
            ):
                return second.value

    candidates = sorted(
        path.relative_to(project_root).with_suffix("").as_posix().replace("/", ".")
        for path in project_root.glob("*/settings.py")
        if not path.name.startswith(".")
    )
    if len(candidates) == 1:
        return candidates[0]
    if not candidates:
        raise RuntimeError("could not resolve Django settings; pass --settings")
    raise RuntimeError(f"multiple Django settings modules found; pass --settings ({', '.join(candidates)})")


def bootstrap(project, settings_name):
    project_root = Path(project).expanduser().resolve()
    if not project_root.is_dir():
        raise RuntimeError(f"project root is not a directory: {project_root}")
    resolved_settings = resolve_settings(project_root, settings_name)
    project_text = str(project_root)
    if not sys.path or sys.path[0] != project_text:
        sys.path.insert(0, project_text)
    os.environ["DJANGO_SETTINGS_MODULE"] = resolved_settings

    import django

    django.setup()
    return project_root, resolved_settings


def resolve_field_source(field, current_model, source_index):
    from django.db import models

    owner = getattr(field, "model", None) or current_model
    candidates = []
    for root in (owner, current_model):
        if not inspect.isclass(root):
            continue
        for candidate in root.__mro__:
            if candidate is models.Model or not issubclass(candidate, models.Model):
                continue
            if candidate not in candidates:
                candidates.append(candidate)
    for candidate in candidates:
        local_fields = [
            *candidate._meta.local_fields,
            *candidate._meta.local_many_to_many,
            *candidate._meta.private_fields,
        ]
        if not any(local.name == field.name for local in local_fields):
            continue
        source_range = source_index.assignment_range(candidate, field.name)
        if source_range is not None:
            return candidate, source_range

    fallback_owner = owner if inspect.isclass(owner) and issubclass(owner, models.Model) else current_model
    return fallback_owner, source_index.class_info(fallback_owner)["range"]


def relation_cardinality(field):
    if getattr(field, "many_to_many", False):
        return "many-to-many"
    if getattr(field, "many_to_one", False):
        return "many-to-one"
    if getattr(field, "one_to_many", False):
        return "one-to-many"
    if getattr(field, "one_to_one", False):
        return "one-to-one"
    return None


def safe_call(value, default=None):
    try:
        return value()
    except (AttributeError, TypeError, ValueError):
        return default


def registered_lookups(node):
    try:
        return node.get_lookups()
    except (AttributeError, TypeError):
        return {}


def transform_for(node, name):
    try:
        transform = node.get_transform(name)
    except (AttributeError, TypeError):
        transform = None
    if transform is not None:
        return transform
    output_field = getattr(node, "output_field", None)
    if output_field is not None and output_field is not node:
        try:
            return output_field.get_transform(name)
        except (AttributeError, TypeError):
            return None
    return None


def node_lookups(node):
    output_field = getattr(node, "output_field", None)
    lookups = registered_lookups(output_field) if output_field is not None and output_field is not node else {}
    own_lookups = registered_lookups(node)
    if not lookups:
        return own_lookups
    if not own_lookups:
        return lookups
    lookups = dict(lookups)
    lookups.update(own_lookups)
    return lookups


def unsupported_lookup_names(node, connection):
    from django.db.models import JSONField

    output_field = getattr(node, "output_field", node)
    if isinstance(output_field, JSONField) and not getattr(connection.features, "supports_json_field_contains", False):
        return {"contains", "contained_by"}
    return set()


def classified_names(node, connection):
    from django.db.models.lookups import Lookup, Transform

    unsupported = unsupported_lookup_names(node, connection)
    lookups = []
    transforms = []
    for name, candidate in sorted(node_lookups(node).items()):
        if name in unsupported:
            continue
        try:
            is_lookup = issubclass(candidate, Lookup)
            is_transform = issubclass(candidate, Transform)
        except TypeError:
            continue
        if is_lookup:
            lookups.append(name)
        elif is_transform:
            transforms.append(name)
    return lookups, transforms


def build_lookup_paths(field, connection):
    from django.db.models import JSONField, Value
    from django.db.models.fields.json import KeyTransform

    paths = []
    try:
        expression = Value(None, output_field=field)
    except (TypeError, ValueError):
        expression = field
    queue = [((), (), expression)]
    queue_index = 0
    seen = set()
    truncated = False
    while queue_index < len(queue) and len(paths) < MAX_LOOKUP_PATHS:
        transforms, kinds, node = queue[queue_index]
        queue_index += 1
        key = (transforms, kinds)
        if key in seen:
            continue
        seen.add(key)
        lookups, fixed = classified_names(node, connection)
        paths.append(
            {
                "transforms": list(transforms),
                "kinds": list(kinds),
                "lookups": lookups,
            }
        )
        if len(transforms) >= LOOKUP_TRANSFORM_MAX_DEPTH:
            continue
        for name in fixed:
            factory = transform_for(node, name)
            try:
                child = factory(node)
                getattr(child, "output_field", None)
            except Exception:
                continue
            if len(seen) + len(queue) - queue_index + len(paths) >= MAX_LOOKUP_PATHS:
                truncated = True
                break
            queue.append((transforms + (name,), kinds + ("transform",), child))

        output_field = getattr(node, "output_field", None)
        if isinstance(output_field, JSONField):
            try:
                child = KeyTransform("*", node)
                getattr(child, "output_field", None)
            except Exception:
                child = None
            if child is not None:
                if len(seen) + len(queue) - queue_index + len(paths) >= MAX_LOOKUP_PATHS:
                    truncated = True
                else:
                    queue.append((transforms + ("*",), kinds + ("key_transform",), child))

    if queue_index < len(queue):
        truncated = True
    paths.sort(key=lambda item: (len(item["transforms"]), item["transforms"], item["kinds"]))
    return paths, truncated


def serialize_field(field, current_model, source_index, connection):
    is_reverse = bool(getattr(field, "auto_created", False) and not getattr(field, "concrete", False))
    origin = field.field if is_reverse else field
    source_owner, source_range = resolve_field_source(origin, origin.model if is_reverse else current_model, source_index)
    related_model = getattr(field, "related_model", None)
    runtime_related_label = model_label(related_model) if hasattr(related_model, "_meta") else None
    if hasattr(related_model, "_meta"):
        related_label = model_label(related_model._meta.concrete_model)
    else:
        related_label = None

    if is_reverse:
        name = field.name
        attname = None
        db_column = None
        db_type = None
        help_text = str(getattr(origin, "help_text", "") or "")
        query_name = field.name
        accessor_name = safe_call(field.get_accessor_name)
        parent_link = bool(getattr(field, "parent_link", False))
        internal_type = type(field).__name__
    else:
        name = field.name
        attname = getattr(field, "attname", None)
        db_column = getattr(field, "column", None)
        if getattr(field, "many_to_many", False):
            db_column = None
        try:
            db_type = field.db_type(connection) if db_column is not None else None
        except (AttributeError, TypeError, ValueError):
            db_type = None
        help_text = str(getattr(field, "help_text", "") or "")
        if getattr(field, "is_relation", False):
            query_method = getattr(field, "related_query_name", None)
            query_name = safe_call(query_method) if callable(query_method) else None
            remote_field = getattr(field, "remote_field", None)
            accessor_method = getattr(remote_field, "get_accessor_name", None)
            accessor_name = safe_call(accessor_method) if callable(accessor_method) else None
            parent_link = bool(getattr(remote_field, "parent_link", False))
        else:
            query_name = None
            accessor_name = None
            parent_link = False
        internal_method = getattr(field, "get_internal_type", None)
        internal_type = safe_call(internal_method, type(field).__name__) if callable(internal_method) else type(field).__name__

    unsupported = unsupported_lookup_names(field, connection)
    names = sorted(name for name in registered_lookups(field) if name not in unsupported)
    _, field_transforms = classified_names(field, connection)
    lookup_paths, lookup_paths_truncated = build_lookup_paths(field, connection)
    return {
        "type": class_path(field),
        "is_relation": bool(getattr(field, "is_relation", False)),
        "related_model": related_label,
        "runtime_related_model": runtime_related_label,
        "lookups": names,
        "unsupported_lookups": sorted(unsupported),
        "help_text": help_text,
        "name": name,
        "attname": attname,
        "db_column": db_column,
        "db_type": db_type,
        "internal_type": internal_type,
        "null": bool(getattr(field, "null", False)),
        "db_index": bool(getattr(field, "db_index", False)),
        "unique": bool(getattr(field, "unique", False)),
        "primary_key": bool(getattr(field, "primary_key", False)),
        "effective_primary_key": bool(not is_reverse and field is current_model._meta.pk),
        "concrete": bool(getattr(field, "concrete", False)),
        "auto_created": bool(getattr(field, "auto_created", False)),
        "relation_cardinality": relation_cardinality(field),
        "relation_direction": "reverse" if is_reverse else ("forward" if getattr(field, "is_relation", False) else None),
        "accessor_name": accessor_name,
        "query_name": query_name,
        "source_model": model_label(source_owner),
        "source_model_abstract": bool(source_owner._meta.abstract),
        "source_range": source_range,
        "parent_link": parent_link,
        "transforms": field_transforms,
        "lookup_paths": lookup_paths,
        "lookup_paths_truncated": lookup_paths_truncated,
    }


def safe_signature(function):
    try:
        signature = inspect.signature(function, follow_wrapped=False, eval_str=False)
    except (TypeError, ValueError):
        return None
    parameters = list(signature.parameters.values())
    if parameters and parameters[0].name in {"self", "cls"}:
        signature = signature.replace(parameters=parameters[1:])
    rendered = str(signature)
    # Callable defaults can render with a process-specific memory address.
    if " at 0x" in rendered:
        return None
    return rendered


def classify_chainable_annotation(annotation):
    import types
    import typing

    if annotation is inspect.Signature.empty or annotation is typing.Any:
        return None
    if isinstance(annotation, str):
        try:
            return classify_annotation_node(ast.parse(annotation, mode="eval").body)
        except SyntaxError:
            return None
    origin = typing.get_origin(annotation)
    if origin in {typing.Union, types.UnionType}:
        classifications = [classify_chainable_annotation(item) for item in typing.get_args(annotation)]
        if any(value is False for value in classifications):
            return False
        return True if classifications and all(value is True for value in classifications) else None
    if annotation is None or annotation is type(None):
        return False
    text = annotation if isinstance(annotation, str) else getattr(annotation, "__qualname__", str(annotation))
    normalized = str(text).replace("typing.", "")
    if "QuerySet" in normalized or "Manager" in normalized or normalized in {"Self", "self"}:
        return True
    scalar_names = {
        "None",
        "bool",
        "bytes",
        "dict",
        "float",
        "int",
        "list",
        "set",
        "str",
        "tuple",
    }
    if normalized.split("[")[0] in scalar_names or "| None" in normalized or "Optional[" in normalized:
        return False
    return None


def annotation_node_name(node):
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        prefix = annotation_node_name(node.value)
        return f"{prefix}.{node.attr}" if prefix else node.attr
    return None


def classify_annotation_node(node):
    if isinstance(node, ast.Constant):
        if node.value is None:
            return False
        if isinstance(node.value, str):
            try:
                return classify_annotation_node(ast.parse(node.value, mode="eval").body)
            except SyntaxError:
                return None
        return False
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr):
        values = [classify_annotation_node(node.left), classify_annotation_node(node.right)]
        if any(value is False for value in values):
            return False
        return True if all(value is True for value in values) else None
    if isinstance(node, ast.Subscript):
        outer = (annotation_node_name(node.value) or "").split(".")[-1]
        if outer in {"QuerySet", "Manager"}:
            return True
        if outer in {"Optional", "list", "dict", "set", "tuple", "Sequence", "Iterable"}:
            return False
        elements = node.slice.elts if isinstance(node.slice, ast.Tuple) else [node.slice]
        if outer == "Union":
            values = [classify_annotation_node(element) for element in elements]
            if any(value is False for value in values):
                return False
            return True if values and all(value is True for value in values) else None
        if outer == "Annotated" and elements:
            return classify_annotation_node(elements[0])
        return None
    name = (annotation_node_name(node) or "").split(".")[-1]
    if name in {"QuerySet", "Manager", "Self"}:
        return True
    if name in {"None", "NoneType", "bool", "bytes", "dict", "float", "int", "list", "set", "str", "tuple"}:
        return False
    return None


def annotation_chainability(function, assume_queryset):
    annotation = inspect.signature(function, follow_wrapped=False, eval_str=False).return_annotation
    classification = classify_chainable_annotation(annotation)
    if classification is None:
        return (True, True) if assume_queryset else (False, False)
    return classification, False


def custom_methods(cls, source_index, assume_queryset):
    methods = []
    seen = set()
    for owner in cls.__mro__:
        if owner.__module__.startswith("django.") and owner.__name__ != "QuerySet":
            continue
        for name, raw in vars(owner).items():
            if isinstance(raw, (staticmethod, classmethod)):
                raw = raw.__func__
            if (
                (owner.__module__ == "django.db.models.query" and name.startswith("_"))
                or (name.startswith("_") and getattr(raw, "queryset_only", None) is not False)
                or name in seen
                or not inspect.isfunction(raw)
            ):
                continue
            seen.add(name)
            if owner.__name__ == "QuerySet" and owner.__module__ == "django.db.models.query":
                chainable, assumed = name in QUERYSET_CHAINABLE_METHODS, False
            else:
                try:
                    chainable, assumed = annotation_chainability(raw, assume_queryset)
                except (TypeError, ValueError):
                    chainable, assumed = (True, True) if assume_queryset else (False, False)
            docstring = inspect.cleandoc(raw.__doc__) if raw.__doc__ else None
            methods.append(
                {
                    "name": name,
                    "owner_class": class_path(owner),
                    "signature": safe_signature(raw),
                    "docstring": docstring,
                    "source_range": source_index.method_range(owner, name) or source_index.class_info(owner)["range"],
                    "chainable": chainable,
                    "assumed_chainable": assumed,
                }
            )
    methods.sort(key=lambda item: (item["name"], item["owner_class"]))
    return methods


def queryset_method_available_on_manager(manager, name):
    member = inspect.getattr_static(type(manager), name, None)
    return callable(member)


def manager_source_range(model, manager, source_index):
    for owner in model.__mro__:
        if not hasattr(owner, "_meta"):
            continue
        if any(local.name == manager.name for local in owner._meta.local_managers):
            source_range = source_index.assignment_range(owner, manager.name)
            return source_range or source_index.class_info(owner)["range"]
    return source_index.class_info(model)["range"]


def serialize_managers(model, source_index, registry):
    managers = []
    queryset_method_keys = set()
    local_names = {manager.name for manager in model._meta.local_managers}
    for manager in sorted(model._meta.managers, key=lambda item: (item.name, class_path(item))):
        queryset_class = getattr(manager, "_queryset_class", None)
        manager_methods = custom_methods(type(manager), source_index, False)
        current_queryset_methods = custom_methods(queryset_class, source_index, True) if queryset_class else []
        for method in current_queryset_methods:
            key = (method["name"], method["owner_class"])
            queryset_method_keys.add(key)
            registry.setdefault(key, method)
        managers.append(
            {
                "name": manager.name,
                "owner_class": class_path(manager),
                "queryset_class": class_path(queryset_class) if queryset_class else None,
                "default": manager is model._default_manager,
                "local": manager.name in local_names,
                "auto_created": bool(getattr(manager, "auto_created", False)),
                "source_range": manager_source_range(model, manager, source_index),
                "methods": manager_methods,
                "queryset_methods": [
                    {
                        "method": {"name": method["name"], "owner_class": method["owner_class"]},
                        "available_on_manager": queryset_method_available_on_manager(manager, method["name"]),
                    }
                    for method in current_queryset_methods
                ],
            }
        )
    method_refs = [{"name": name, "owner_class": owner_class} for name, owner_class in sorted(queryset_method_keys)]
    return managers, method_refs


def serialize_parents(model, source_index):
    from django.db import models

    parents = []
    for parent in model.__bases__:
        if not inspect.isclass(parent) or not issubclass(parent, models.Model) or parent is models.Model:
            continue
        link = model._meta.parents.get(parent)
        parents.append(
            {
                "canonical_label": model_label(parent),
                "class_path": class_path(parent),
                "abstract": bool(parent._meta.abstract),
                "proxy": bool(parent._meta.proxy),
                "parent_link": link.name if link is not None else None,
                "source_range": source_index.class_info(parent)["range"],
            }
        )
    parents.sort(key=lambda item: (item["canonical_label"], item["class_path"]))
    return parents


def serialize_indexes(model, source_index):
    indexes = []
    for index in sorted(model._meta.indexes, key=lambda item: item.name):
        fields = [
            {"name": name, "order": "desc" if order == "DESC" else "asc"}
            for name, order in getattr(index, "fields_orders", ())
        ]
        indexes.append(
            {
                "name": index.name,
                "fields": fields,
                "expressions": [str(expression) for expression in getattr(index, "expressions", ())],
                "condition": str(index.condition) if getattr(index, "condition", None) is not None else None,
                "include": list(getattr(index, "include", ()) or ()),
                "opclasses": list(getattr(index, "opclasses", ()) or ()),
                "db_tablespace": getattr(index, "db_tablespace", None) or None,
                "source_range": source_index.named_call_range(model, "indexes", index.name) or source_index.class_info(model)["range"],
            }
        )
    return indexes


def serialize_constraints(model, source_index):
    constraints = []
    for constraint in sorted(model._meta.constraints, key=lambda item: item.name):
        if hasattr(constraint, "condition"):
            condition = constraint.condition
        else:
            condition = getattr(constraint, "check", None)
        constraints.append(
            {
                "name": constraint.name,
                "type": class_path(constraint),
                "fields": list(getattr(constraint, "fields", ()) or ()),
                "expressions": [str(expression) for expression in getattr(constraint, "expressions", ())],
                "condition": str(condition) if condition is not None else None,
                "include": list(getattr(constraint, "include", ()) or ()),
                "opclasses": list(getattr(constraint, "opclasses", ()) or ()),
                "deferrable": str(getattr(constraint, "deferrable", None)) if getattr(constraint, "deferrable", None) is not None else None,
                "nulls_distinct": getattr(constraint, "nulls_distinct", None),
                "violation_error_code": str(getattr(constraint, "violation_error_code", None)) if getattr(constraint, "violation_error_code", None) is not None else None,
                "violation_error_message": str(getattr(constraint, "violation_error_message", None)) if getattr(constraint, "violation_error_message", None) is not None else None,
                "source_range": source_index.named_call_range(model, "constraints", constraint.name) or source_index.class_info(model)["range"],
            }
        )
    return constraints


def serialize_model(model, source_index, connection, registry):
    from django.db import models

    model_info = source_index.class_info(model)
    fields = {}
    for field in sorted(model._meta.get_fields(include_parents=True, include_hidden=False), key=lambda item: item.name):
        fields[field.name] = serialize_field(field, model, source_index, connection)
    managers, queryset_methods = serialize_managers(model, source_index, registry)
    custom_manager_names = sorted(
        manager.name for manager in model._meta.local_managers if not getattr(manager, "auto_created", False)
    )
    has_abstract_parent = any(
        inspect.isclass(parent)
        and issubclass(parent, models.Model)
        and parent is not models.Model
        and parent._meta.abstract
        for parent in model.__mro__[1:]
    )
    source_range = model_info["range"]
    return {
        "canonical_label": model_label(model),
        "module": model.__module__,
        "qualname": model.__qualname__,
        "file_path": model_info["path"],
        "line_number": source_range["start"]["line"] if source_range else None,
        "source_range": source_range,
        "docstring": inspect.cleandoc(model.__doc__) if model.__doc__ else None,
        "abstract": bool(model._meta.abstract),
        "proxy": bool(model._meta.proxy),
        "managed": bool(model._meta.managed),
        "swapped": model._meta.swapped is not None,
        "has_abstract_parent": has_abstract_parent,
        "multi_table_child": any(link is not None for link in model._meta.parents.values()),
        "parents": serialize_parents(model, source_index),
        "default_manager": model._default_manager.name,
        "base_manager": {
            "name": model._base_manager.name,
            "owner_class": class_path(model._base_manager),
        },
        "custom_managers": custom_manager_names,
        "managers": managers,
        "queryset_methods": queryset_methods,
        "indexes": serialize_indexes(model, source_index),
        "constraints": serialize_constraints(model, source_index),
        "fields": fields,
    }


def imported_project_sources(project_root, modules):
    root = Path(project_root).resolve()
    sources = set()
    complete = True
    try:
        candidates = list(itertools.islice(iter(modules.values()), MAX_IMPORTED_MODULES + 1))
    except (AttributeError, RuntimeError, TypeError, ValueError):
        return sources, False
    if len(candidates) > MAX_IMPORTED_MODULES:
        candidates = candidates[:MAX_IMPORTED_MODULES]
        complete = False
    total_path_bytes = 0
    for module in candidates:
        try:
            module_path = getattr(module, "__file__", None)
        except Exception:
            complete = False
            continue
        if not isinstance(module_path, (str, os.PathLike)) or isinstance(module_path, bytes):
            if module_path is not None:
                complete = False
            continue
        try:
            resolved_path = Path(module_path).resolve()
            if not resolved_path.is_file() or not resolved_path.is_relative_to(root):
                continue
            path = str(resolved_path)
        except (OSError, RuntimeError, TypeError, ValueError):
            complete = False
            continue
        path_bytes = len(path.encode("utf-8"))
        if path_bytes > MAX_SCHEMA_SOURCE_PATH_BYTES - total_path_bytes:
            complete = False
            continue
        total_path_bytes += path_bytes
        sources.add(path)
        if len(sources) > MAX_SCHEMA_SOURCE_MODULES:
            complete = False
            sources.remove(path)
            break
    return sources, complete


def bounded_schema_sources(root, sources, complete):
    result = []
    total_path_bytes = 0
    try:
        candidates = sorted(sources)
    except (RuntimeError, TypeError, ValueError):
        return result, False
    for path in candidates:
        if len(result) >= MAX_SCHEMA_SOURCE_MODULES:
            complete = False
            break
        try:
            resolved = Path(path).resolve()
            if not resolved.is_relative_to(root):
                continue
            text = str(resolved)
        except (OSError, RuntimeError, TypeError, ValueError):
            complete = False
            continue
        path_bytes = len(text.encode("utf-8"))
        if path_bytes > MAX_SCHEMA_SOURCE_PATH_BYTES - total_path_bytes:
            complete = False
            continue
        total_path_bytes += path_bytes
        result.append(text)
    return result, complete


def build_snapshot(project_root, settings_name):
    from django.apps import apps
    from django.db import connections

    source_index = SourceIndex()
    connection = connections["default"]
    queryset_method_registry = {}
    models = sorted(
        (model for model in apps.get_models() if not model._meta.abstract and not model._meta.proxy),
        key=lambda item: item._meta.label_lower,
    )
    if not models:
        print(
            "pogo warning: Django app registry reports 0 concrete models; "
            "verify the resolved settings module points at the intended project",
            file=sys.stderr,
        )
    app_models = {
        config.label: {
            "label": config.label,
            "import_name": config.name,
            "root_path": str(Path(config.path).resolve()),
            "models": {},
        }
        for config in sorted(apps.get_app_configs(), key=lambda item: item.label)
    }
    for model in models:
        config = model._meta.app_config
        app_entry = app_models[config.label]
        serialized = serialize_model(model, source_index, connection, queryset_method_registry)
        app_entry["models"][model.__name__] = serialized

    settings_module = importlib.import_module(settings_name)
    settings_path = inspect.getsourcefile(settings_module)
    sources = source_index.source_paths()
    if settings_path:
        sources.add(str(Path(settings_path).resolve()))
    root = Path(project_root).resolve()
    imported_sources, sources_complete = imported_project_sources(root, sys.modules)
    sources.update(imported_sources)
    project_sources, sources_complete = bounded_schema_sources(root, sources, sources_complete)

    ordered_apps = {}
    for label in sorted(app_models):
        app_entry = app_models[label]
        app_entry["models"] = {name: app_entry["models"][name] for name in sorted(app_entry["models"])}
        ordered_apps[label] = app_entry
    queryset_method_defs = [queryset_method_registry[key] for key in sorted(queryset_method_registry)]
    return {
        "schema_version": SCHEMA_VERSION,
        "position_encoding": "utf-8-bytes",
        "lookup_transform_max_depth": LOOKUP_TRANSFORM_MAX_DEPTH,
        "lookup_path_max_count": MAX_LOOKUP_PATHS,
        "schema_sources": project_sources,
        "schema_sources_complete": sources_complete,
        "queryset_method_defs": queryset_method_defs,
        "apps": ordered_apps,
    }
class FrameError(Exception):
    pass


class PayloadError(Exception):
    def __init__(self, code, message, request_id=None, fatal=False):
        super().__init__(message)
        self.code = code
        self.message = message
        self.request_id = request_id
        self.fatal = fatal


class FrameReader:
    def __init__(self, connection):
        self.connection = connection
        self.buffer = bytearray()

    def read(self):
        while True:
            newline = self.buffer.find(b"\n")
            if newline >= 0:
                if newline == 0:
                    raise FrameError("empty IPC frame")
                if newline > MAX_IPC_FRAME_SIZE:
                    raise FrameError("IPC frame exceeds maximum size")
                payload = bytes(self.buffer[:newline])
                del self.buffer[: newline + 1]
                return payload
            if len(self.buffer) > MAX_IPC_FRAME_SIZE:
                raise FrameError("IPC frame exceeds maximum size")
            chunk = self.connection.recv(min(65536, MAX_IPC_FRAME_SIZE + 1 - len(self.buffer)))
            if not chunk:
                if self.buffer:
                    raise FrameError("truncated IPC frame")
                return None
            self.buffer.extend(chunk)


def encode_message(message):
    encoder = json.JSONEncoder(
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
    )
    payload = encoder.encode(message).encode("utf-8")
    if not payload:
        raise FrameError("empty IPC frame")
    if len(payload) > MAX_IPC_FRAME_SIZE:
        raise FrameError("IPC frame exceeds maximum size")
    return payload + b"\n"


def write_message(connection, message):
    connection.sendall(encode_message(message))


def decode_object(payload):
    def reject_duplicate_keys(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate key: {key}")
            result[key] = value
        return result

    try:
        value = json.loads(
            payload.decode("utf-8"),
            object_pairs_hook=reject_duplicate_keys,
            parse_constant=lambda value: (_ for _ in ()).throw(ValueError(f"invalid constant: {value}")),
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        raise PayloadError("parse_error", "payload is not valid JSON") from error
    if type(value) is not dict:
        raise PayloadError("parse_error", "payload must be a JSON object")
    return value


def validate_request(value):
    candidate_id = value.get("id")
    request_id = (
        candidate_id
        if type(candidate_id) is str and candidate_id and len(candidate_id.encode("utf-8")) <= 128
        else None
    )
    if set(value) != {"protocol_version", "id", "method", "params"}:
        raise PayloadError("invalid_request", "request envelope has invalid fields", request_id)
    version = value["protocol_version"]
    if type(version) is not int or version != PROTOCOL_VERSION:
        raise PayloadError(
            "protocol_version_mismatch",
            "unsupported protocol version",
            request_id,
            fatal=True,
        )
    if type(value["id"]) is not str or not value["id"] or len(value["id"].encode("utf-8")) > 128:
        raise PayloadError("invalid_request", "request id must be a nonempty bounded string")
    if type(value["method"]) is not str or not value["method"]:
        raise PayloadError("invalid_request", "request method must be a nonempty string", value["id"])
    if type(value["params"]) is not dict:
        raise PayloadError("invalid_request", "request params must be an object", value["id"])
    return value


def success_response(request_id, result):
    return {
        "protocol_version": PROTOCOL_VERSION,
        "id": request_id,
        "result": result,
        "error": None,
    }


def error_response(request_id, code, message):
    return {
        "protocol_version": PROTOCOL_VERSION,
        "id": request_id,
        "result": None,
        "error": {"code": code, "message": message},
    }


class WorkerState:
    def __init__(self, project, settings_name, log_timings=False):
        self.project = project
        self.settings_name = settings_name
        self.log_timings = log_timings
        self.project_root = None
        self.resolved_settings = None

    def dump_schema(self):
        if self.project_root is None:
            started = time.perf_counter()
            self.project_root, self.resolved_settings = bootstrap(self.project, self.settings_name)
            if self.log_timings:
                print(f"pogo phase django_setup={time.perf_counter() - started:.3f}s", file=sys.stderr)
        started = time.perf_counter()
        snapshot = build_snapshot(self.project_root, self.resolved_settings)
        if self.log_timings:
            print(f"pogo phase python_snapshot={time.perf_counter() - started:.3f}s", file=sys.stderr)
        return snapshot


def dispatch_request(request, worker_state):
    request_id = request["id"]
    method = request["method"]
    if request["params"]:
        return error_response(request_id, "invalid_params", "params must be an empty object"), False
    if method == "worker/ping":
        return success_response(request_id, {"pong": True}), False
    if method == "worker/shutdown":
        return success_response(request_id, None), True
    if method == "schema/load":
        try:
            return success_response(request_id, worker_state.dump_schema()), False
        except Exception as error:
            traceback.print_exc(file=sys.stderr)
            detail = f"{type(error).__name__}: {error}"
            if len(detail) > 2000:
                detail = detail[:2000] + "..."
            message = f"Django schema introspection failed: {detail}"
            return error_response(request_id, "introspection_failed", message), True
    return error_response(request_id, "method_not_found", "unknown worker method"), False


def serve_connection(connection, project, settings_name, worker_state=None):
    reader = FrameReader(connection)
    if worker_state is None:
        worker_state = WorkerState(project, settings_name, log_timings=True)
    while True:
        payload = reader.read()
        if payload is None:
            return 0
        fatal = False
        try:
            request = validate_request(decode_object(payload))
            response, should_stop = dispatch_request(request, worker_state)
        except PayloadError as error:
            response = error_response(error.request_id, error.code, error.message)
            should_stop = False
            fatal = error.fatal
        try:
            write_message(connection, response)
        except FrameError:
            fallback = error_response(response.get("id"), "response_too_large", "worker response exceeds maximum size")
            write_message(connection, fallback)
        if should_stop:
            return 1 if response["error"] is not None else 0
        if fatal:
            return 1
        response = None


def connect_worker(project, settings_name):
    network = os.environ.pop(WORKER_NETWORK_ENV, None)
    address = os.environ.pop(WORKER_ADDRESS_ENV, None)
    token = os.environ.pop(WORKER_TOKEN_ENV, None)
    token_file = os.environ.pop(WORKER_TOKEN_FILE_ENV, None)
    if token_file:
        try:
            token = Path(token_file).read_text(encoding="ascii")
        finally:
            try:
                Path(token_file).unlink()
            except FileNotFoundError:
                pass
    if network not in {"unix", "tcp"} or not address or not token:
        raise RuntimeError("worker endpoint environment is incomplete")
    family = socket.AF_UNIX if network == "unix" else socket.AF_INET
    destination = address
    if network == "tcp":
        host, port = address.rsplit(":", 1)
        destination = (host, int(port))
    with socket.socket(family, socket.SOCK_STREAM) as connection:
        connection.connect(destination)
        write_message(
            connection,
            {
                "protocol_version": PROTOCOL_VERSION,
                "type": "hello",
                "token": token,
            },
        )
        return serve_connection(connection, project, settings_name)


def parse_args(arguments):
    parser = argparse.ArgumentParser(description="Dump a deterministic Django ORM schema snapshot.")
    parser.add_argument("--project", required=True, help="Django project root")
    parser.add_argument("--settings", help="Django settings module")
    formatting = parser.add_mutually_exclusive_group()
    formatting.add_argument("--pretty", action="store_true", help="write indented JSON")
    formatting.add_argument("--compact", action="store_true", help="write compact JSON (default)")
    parser.add_argument("--connect", action="store_true", help=argparse.SUPPRESS)
    return parser.parse_args(arguments)


def main(arguments=None):
    try:
        args = parse_args(arguments)
        if args.connect:
            if args.pretty or args.compact:
                raise RuntimeError("--pretty and --compact are unavailable with --connect")
            with contextlib.redirect_stdout(sys.stderr):
                return connect_worker(args.project, args.settings)
        with contextlib.redirect_stdout(sys.stderr):
            project_root, settings_name = bootstrap(args.project, args.settings)
            snapshot = build_snapshot(project_root, settings_name)
        if args.pretty:
            output = json.dumps(snapshot, ensure_ascii=False, indent=2)
        else:
            output = json.dumps(snapshot, ensure_ascii=False, separators=(",", ":"))
        sys.stdout.write(output + "\n")
        return 0
    except SystemExit:
        raise
    except Exception as error:
        print(f"introspection error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
