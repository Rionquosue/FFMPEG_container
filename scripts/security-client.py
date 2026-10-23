#!/usr/bin/env python3
"""Security regression checks against a running service, from a client
container on the private network. Covers the attack classes seen in real
deployments of FFmpeg-based services and HTTP upload APIs; fixtures are
minimal, inert files generated here."""
import hashlib
import http.client
import json
from pathlib import Path
import socket
import struct
import threading
import time
import uuid
import zlib

keys = json.loads(Path("/fixtures/keys.json").read_text())["consumers"]
KEY, OTHER_KEY = keys[0]["keys"][0], keys[1]["keys"][0]
VIDEO = Path("/fixtures/input.mp4").read_bytes()
failures = []


def check(name, condition, detail=""):
    print(("PASS  " if condition else "FAIL  ") + name + ("" if condition else f"  ({detail})"))
    if not condition:
        failures.append(name)


def request(method, path, body=None, extra=None, key=KEY, timeout=15):
    connection = http.client.HTTPConnection("ffmpeg", 8080, timeout=timeout)
    headers = {"Authorization": "Bearer " + key} if key else {}
    headers.update(extra or {})
    connection.request(method, path, body, headers)
    response = connection.getresponse()
    result = response.status, {k.lower(): v for k, v in response.getheaders()}, response.read()
    connection.close()
    return result


def multipart(spec, data, filename="input", content_type=None):
    boundary = uuid.uuid4().hex
    file_headers = f"Content-Disposition: form-data; name=\"file\"; filename=\"{filename}\"\r\n"
    if content_type:
        file_headers += f"Content-Type: {content_type}\r\n"
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"request\"\r\nContent-Type: application/json\r\n\r\n"
            + json.dumps(spec) + f"\r\n--{boundary}\r\n{file_headers}\r\n").encode() + data + f"\r\n--{boundary}--\r\n".encode()
    return body, "multipart/form-data; boundary=" + boundary


def spec_for(data, operation="metadata", output="json", parameters=None, size=None, sha=None):
    return {"operation": operation, "output_format": output, "parameters": parameters or {},
            "input": {"size_bytes": len(data) if size is None else size, "sha256": sha or hashlib.sha256(data).hexdigest()}}


def submit(data, key=KEY, **options):
    filename = options.pop("filename", "input")
    content_type = options.pop("content_type", None)
    body, ctype = multipart(spec_for(data, **options), data, filename, content_type)
    return request("POST", "/v1/jobs", body, {"Content-Type": ctype, "Idempotency-Key": uuid.uuid4().hex}, key=key)


def finish(job):
    for _ in range(240):
        code, _, raw = request("GET", job["status_url"])
        job = json.loads(raw) if code == 200 else {"status": f"http {code}"}
        if job["status"] not in ("queued", "processing"):
            return job
        time.sleep(0.25)
    return job


def cleanup(job):
    request("DELETE", job["status_url"])


def media_rejected(name, data, allowed=("invalid_media", "unsupported_media"), **options):
    """The upload may be accepted, but processing must fail closed with a contract code."""
    code, _, raw = submit(data, **options)
    if code != 202:
        check(name, 400 <= code < 500, f"HTTP {code}")
        return
    job = finish(json.loads(raw))
    error = (job.get("error") or {}).get("code")
    check(name, job["status"] == "failed" and error in allowed, f"{job['status']} {error}")
    cleanup(job)


def healthy():
    return request("GET", "/health/ready", key=None)[0] == 200


# --- Malicious media: playlist and reference tricks -------------------------
# HLS/concat local-file and SSRF reads (the 2016/2017 FFmpeg HLS and
# concat disclosures, still used against upload thumbnailers).
hls_local = b"#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\nfile:///run/secrets/api_keys\n#EXT-X-ENDLIST\n"
hls_ssrf = b"#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\nhttp://169.254.169.254/latest/meta-data/\n#EXT-X-ENDLIST\n"
concat = b"ffconcat version 1.0\nfile '/run/secrets/api_keys'\nfile '/etc/passwd'\n"
media_rejected("HLS playlist reading a local secret is rejected", hls_local)
media_rejected("HLS playlist pointing at cloud metadata (SSRF) is rejected", hls_ssrf, operation="transcode", output="mp4")
media_rejected("ffconcat list of local files is rejected", concat, operation="transcode", output="mp4")
# The same playlist disguised by filename and declared type (extension spoofing).
media_rejected("HLS playlist disguised as .mp4 is rejected", hls_local, filename="video.mp4", content_type="video/mp4",
               operation="thumbnail", output="png", parameters={"width": 32, "height": 32, "timestamp_seconds": 0})
# AVI with a GAB2 subtitle track carrying a playlist (the 2017 AVI/HLS chain):
# the AVI demuxer is not allowed at all.
avi = b"RIFF" + struct.pack("<I", 200) + b"AVI LIST" + struct.pack("<I", 4) + b"hdrl" + b"\x00" * 180
media_rejected("AVI container (GAB2 subtitle playlist chain) is rejected", avi)
# SVG / HTML with external references must never be parsed.
svg = b'<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><image xlink:href="file:///etc/passwd"/></svg>'
media_rejected("SVG with an external file reference is rejected", svg, operation="resize", output="png",
               parameters={"width": 32, "height": 32}, content_type="image/svg+xml")

# --- Resource bombs ------------------------------------------------------------
def png(width, height):
    def chunk(kind, payload):
        return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(b"\x00" * 64)) + chunk(b"IEND", b""))


media_rejected("PNG declaring 60000x60000 pixels (pixel bomb) is refused before decoding", png(60000, 60000),
               allowed=("dimensions_exceeded", "invalid_media"), operation="resize", output="png",
               parameters={"width": 32, "height": 32})
# A genuinely two-hour video that compresses to a few kilobytes (duration bomb).
long_video = Path("/fixtures/long.mp4").read_bytes()
media_rejected(f"Two-hour video in {len(long_video) // 1024} KiB (duration bomb) is refused", long_video,
               allowed=("duration_exceeded",), operation="transcode", output="mp4")
media_rejected("Truncated MP4 fails closed", VIDEO[: len(VIDEO) // 3], operation="transcode", output="mp4")
check("Service is still ready after malicious media", healthy())

# --- API: authentication, ownership, traversal ---------------------------------
check("No bearer key is rejected", request("GET", "/v1/capabilities", key=None)[0] == 401)
check("Wrong bearer key is rejected", request("GET", "/v1/capabilities", key="x" * 43)[0] == 401)
check("Key in the query string is not accepted", request("GET", "/v1/capabilities?key=" + KEY, key=None)[0] in (400, 401))
code, _, raw = submit(VIDEO)
job = finish(json.loads(raw)) if code == 202 else {}
check("Reference job completes", job.get("status") == "completed", job)
if job.get("status") == "completed":
    for method, path in [("GET", job["status_url"]), ("GET", job["download_url"]), ("DELETE", job["status_url"])]:
        check(f"Other consumer gets 404 for {method} {path.split('/v1')[1][:20]}…",
              request(method, path, key=OTHER_KEY)[0] == 404)
    code, headers, _ = request("GET", job["download_url"])
    check("Download is attachment with a server-chosen name and nosniff",
          code == 200 and headers.get("content-disposition", "").startswith("attachment")
          and "result." in headers.get("content-disposition", "") and headers.get("x-content-type-options") == "nosniff")
    cleanup(job)
for path in ["/v1/jobs/../../../etc/passwd", "/v1/jobs/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
             "/v1/jobs/" + "0" * 32 + "/../../run/secrets/api_keys", "/v1/jobs/" + "0" * 32 + "/download/../../x"]:
    status = request("GET", path)[0]
    check(f"Traversal path is not served: {path[:40]}", status in (400, 404), status)
code, _, raw = submit(VIDEO, filename="../../../../run/secrets/api_keys")
check("Path-traversal filename is display metadata only", code == 202, code)
if code == 202:
    cleanup(finish(json.loads(raw)))

# --- API: input validation and injection --------------------------------------
check("Declared size over the limit is refused (413)", submit(VIDEO, size=10**12)[0] == 413)
check("Body larger than declared is refused", submit(VIDEO, size=len(VIDEO) - 100)[0] in (400, 413))
check("Checksum that does not match is refused", submit(VIDEO, sha="0" * 64)[0] == 400)
check("FFmpeg filter injection through parameters is refused",
      submit(VIDEO, parameters={"filter": "movie=/run/secrets/api_keys"})[0] == 422)
check("Argument injection through output_format is refused", submit(VIDEO, output="json -i /etc/passwd")[0] == 422)
boundary = uuid.uuid4().hex
big = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"request\"\r\nContent-Type: application/json\r\n\r\n"
       + json.dumps({"pad": "x" * 20000}) + f"\r\n--{boundary}--\r\n").encode()
check("Oversized request JSON is refused",
      request("POST", "/v1/jobs", big, {"Content-Type": "multipart/form-data; boundary=" + boundary,
                                        "Idempotency-Key": uuid.uuid4().hex})[0] == 400)

# --- Slow and flooding clients -------------------------------------------------
sock = socket.create_connection(("ffmpeg", 8080), timeout=15)
sock.sendall(b"GET /health/live HTTP/1.1\r\nHost: ffmpeg\r\n")
started = time.time()
try:
    closed = sock.recv(1) == b""
except (ConnectionResetError, socket.timeout):
    closed = True
sock.close()
check("Slowloris (incomplete headers) is cut off within 10 s", closed and time.time() - started < 10,
      f"{time.time() - started:.1f}s")
codes = []
lock = threading.Lock()


def flood():
    try:
        status = submit(VIDEO)[0]
    except OSError:
        status = 0
    with lock:
        codes.append(status)


threads = [threading.Thread(target=flood) for _ in range(40)]
for thread in threads:
    thread.start()
for thread in threads:
    thread.join()
check("Upload flood gets only 202/429/503", set(codes) <= {202, 429, 503}, sorted(set(codes)))
check("Service is still ready after the flood", healthy())

print(f"{len(failures)} failed" if failures else "ALL SECURITY CHECKS PASSED")
raise SystemExit(1 if failures else 0)
