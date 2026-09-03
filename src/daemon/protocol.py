#!/usr/bin/env python3
from __future__ import annotations

import argparse
import contextlib
import json
import os
from pathlib import Path
import socket
import sys
import time
import traceback

import introspect


PROTOCOL_VERSION = 1
MAX_IPC_FRAME_SIZE = 32 * 1024 * 1024
WORKER_NETWORK_ENV = "POGO_WORKER_NETWORK"
WORKER_ADDRESS_ENV = "POGO_WORKER_ADDRESS"
WORKER_TOKEN_ENV = "POGO_WORKER_TOKEN"
WORKER_TOKEN_FILE_ENV = "POGO_WORKER_TOKEN_FILE"


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
            self.project_root, self.resolved_settings = introspect.bootstrap(self.project, self.settings_name)
            if self.log_timings:
                print(f"pogo phase django_setup={time.perf_counter() - started:.3f}s", file=sys.stderr)
        started = time.perf_counter()
        snapshot = introspect.build_snapshot(self.project_root, self.resolved_settings)
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
            project_root, settings_name = introspect.bootstrap(args.project, args.settings)
            snapshot = introspect.build_snapshot(project_root, settings_name)
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
