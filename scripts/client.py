#!/usr/bin/env python3
"""Bounded-memory upload, poll, verified download, and acknowledgement."""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import time
from urllib.parse import urlsplit
import uuid


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("file", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--url", default="http://ffmpeg:8080")
    parser.add_argument("--keys", type=Path, required=True)
    parser.add_argument("--consumer", default="rails")
    parser.add_argument("--operation", default="metadata")
    parser.add_argument("--format", default="json")
    parser.add_argument("--parameters", default="{}")
    parser.add_argument("--idempotency-key", default=None)
    args = parser.parse_args()
    if args.output.exists():
        parser.error("output already exists")
    parsed = urlsplit(args.url)
    if parsed.scheme not in ("http", "https") or parsed.path not in ("", "/") or parsed.query or parsed.username:
        parser.error("URL must be an HTTP(S) origin without credentials")
    consumers = json.loads(args.keys.read_text())["consumers"]
    key = next(c["keys"][0] for c in consumers if c["id"] == args.consumer)
    connection_type = http.client.HTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection

    def connection():
        return connection_type(parsed.hostname, parsed.port, timeout=120)

    def get(method, path):
        conn = connection()
        conn.request(method, path, headers={"Authorization": "Bearer " + key})
        response = conn.getresponse()
        return conn, response

    digest = hashlib.sha256()
    with args.file.open("rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            digest.update(chunk)
    request = {"operation": args.operation, "input": {"size_bytes": args.file.stat().st_size,
               "sha256": digest.hexdigest()}, "parameters": json.loads(args.parameters), "output_format": args.format}
    boundary = uuid.uuid4().hex
    prefix = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"request\"\r\n"
              "Content-Type: application/json\r\n\r\n" + json.dumps(request) +
              f"\r\n--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"input\"\r\n"
              "Content-Type: application/octet-stream\r\n\r\n").encode()
    suffix = f"\r\n--{boundary}--\r\n".encode()
    conn = connection()
    conn.putrequest("POST", "/v1/jobs")
    conn.putheader("Authorization", "Bearer " + key)
    conn.putheader("Idempotency-Key", args.idempotency_key or uuid.uuid4().hex)
    conn.putheader("Content-Type", "multipart/form-data; boundary=" + boundary)
    conn.putheader("Content-Length", str(len(prefix) + args.file.stat().st_size + len(suffix)))
    conn.endheaders()
    conn.send(prefix)
    with args.file.open("rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            conn.send(chunk)
    conn.send(suffix)
    response = conn.getresponse()
    body = response.read(65536)
    if response.status != 202:
        raise RuntimeError(f"Upload rejected: HTTP {response.status}: {body.decode()}")
    job = json.loads(body)
    conn.close()
    deadline = time.monotonic() + 600
    while job["status"] in ("queued", "processing", "cancelling"):
        if time.monotonic() >= deadline:
            raise TimeoutError("Polling deadline exceeded; job may still exist")
        time.sleep(2)
        conn, response = get("GET", job["status_url"])
        body = response.read(65536)
        if response.status != 200:
            raise RuntimeError(f"Status HTTP {response.status}")
        job = json.loads(body)
        conn.close()
    if job["status"] != "completed":
        raise RuntimeError(f"Processing failed: {job.get('error')}")
    conn, response = get("GET", job["download_url"])
    if response.status != 200:
        raise RuntimeError(f"Download HTTP {response.status}")
    temp = args.output.with_name(args.output.name + ".partial-" + uuid.uuid4().hex)
    digest = hashlib.sha256()
    size = 0
    try:
        with temp.open("xb") as f:
            while chunk := response.read(65536):
                size += len(chunk)
                if size > job["result"]["size_bytes"]:
                    raise ValueError("Download exceeds expected size")
                digest.update(chunk)
                f.write(chunk)
            f.flush()
            os.fsync(f.fileno())
        if size != job["result"]["size_bytes"] or digest.hexdigest() != job["result"]["sha256"]:
            raise ValueError("Download integrity check failed")
        # Atomic non-overwriting publication on the same filesystem.
        os.link(temp, args.output)
        parent_fd = os.open(args.output.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
    finally:
        temp.unlink(missing_ok=True)
        conn.close()
    conn, response = get("DELETE", job["status_url"])
    response.read(65536)
    if response.status not in (202, 204):
        print("Output saved; remote cleanup will occur at expiry.")
    conn.close()
    print(f"Saved verified result: {args.output}")


if __name__ == "__main__":
    main()
