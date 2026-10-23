# FFmpeg service

A standalone Alpine container with a Go HTTP API, FFmpeg/FFprobe, temporary RAM-backed storage, and a mandatory Linux sandbox. No PostgreSQL, Redis or durable media volume.

Version: **0.1.0 — initial implementation**. See [BUILD_STATUS.md](BUILD_STATUS.md) for completed checks and remaining deployment verification. This repository is ready to build; the included Docker image has not been executed in the authoring environment.

## Requirements

- Linux host with Landlock ABI 3 or newer enabled (normally Linux 6.2+), seccomp, and a Docker runtime that permits the Landlock syscalls.
- Docker Engine and Compose, approximately 2 GiB of available container memory and 2 CPU shares/cores of capacity.
- A private Docker network shared with the calling application. Existing PostgreSQL is unaffected.

Startup refuses to run if sandbox checks, encoder checks, the API egress guard, or tmpfs validation fail. There is **no configuration flag to disable the sandbox**. Do not change to privileged mode or disable Docker seccomp to make startup succeed; use a compatible, updated host/runtime.

## Start

```sh
cp .env.example .env
python3 scripts/init-keys.py
sudo chown 10001:10001 secrets/api_keys.json
sudo chmod 0400 secrets/api_keys.json
docker network create --internal media_processing
docker compose up -d --build
docker compose logs ffmpeg
```

Create the network only if it does not already exist; verify an existing network is internal. UID ownership above assumes ordinary Linux Docker without user-namespace remapping. For rootless/remapped Docker, map the secret owner to the container's UID 10001. Do not relax the secret to world-readable for production.

Attach the Rails worker or another authorized application to `media_processing`. It reaches `http://ffmpeg:8080`. The service publishes **no host ports**. The image's Docker healthcheck calls the private readiness endpoint.

API keys are read once at startup. The file format is:

```json
{
  "consumers": [
    {"id": "rails", "keys": ["REPLACE_WITH_A_RANDOM_256_BIT_BASE64URL_KEY"]}
  ]
}
```

The placeholder is not a valid production key. The initialization script generates an actual random key without printing it. Add another consumer with a separate key for another application. An identity can contain up to four keys for rotation overlap; restart to reload. Restart discards jobs, so drain jobs first and let clients recover from 404 responses.

## API specification (v1)

All /v1 endpoints require `Authorization: Bearer <key>`.

| Endpoint | Purpose |
|---|---|
| GET /health/live | Minimal process status |
| GET /health/ready | Ready after mandatory startup checks |
| GET /v1/capabilities | Operations, formats and configured limits |
| POST /v1/jobs | Stream an uploaded file and JSON operation |
| GET /v1/jobs/{id} | JSON status or HTML with Accept: text/html |
| GET /v1/jobs/{id}/download | Result download, including single byte ranges |
| DELETE /v1/jobs/{id} | Cancel or delete; repeat until 204 for completed cleanup |

Machine-readable contract: [docs/openapi.json](docs/openapi.json).

Upload multipart parts in this order: `request` (application/json), then `file`. Include `Idempotency-Key` and the exact input size/SHA-256 in the request. For example:

```json
{
  "operation": "transcode",
  "input": {"size_bytes": 12345, "sha256": "<64 hexadecimal checksum characters>"},
  "parameters": {"width": 1280, "height": 720, "fit": "contain", "quality": 23},
  "output_format": "mp4"
}
```

The service validates the full streamed upload before returning 202 and a status URL. It never interprets a shell command or accepts arbitrary FFmpeg flags. Unknown fields and duplicate JSON keys are rejected.

| Operation | Formats |
|---|---|
| transcode | mp4 (H.264/AAC), webm (VP9/Opus) |
| resize | jpg, png; still images only |
| thumbnail | jpg, png; one video frame |
| extract_audio | m4a, mp3, opus |
| trim | mp4, webm; re-encoded boundaries |
| remux | mp4, webm; compatible codecs only |
| metadata | json; sanitized format/duration/stream summary |

Input demuxers are explicitly limited to MOV/MP4, Matroska/WebM, MP3, WAV, FLAC, Ogg, AAC, PNG and JPEG. Playlists, arbitrary URLs, animated-image processing, SVG, archives and user filter expressions are unsupported. Non-image inputs must have a known bounded duration. Remux v1 supports H.264/AAC into MP4 and VP8/VP9/AV1 plus Opus/Vorbis into WebM.

No upscaling. Geometry applies after orientation; contain pads and cover center-crops. Text metadata is removed. A declared upload MIME type, if supplied and not application/octet-stream, must match the detected container family.

An HTML result page still requires bearer authentication. A normal browser link does not magically carry that header. A consuming application should relay the page/result through its own authenticated session. Never put API keys in a URL.

### Transport and authentication

The default base URL is `http://ffmpeg:8080` on the private container network. Use HTTPS at an authenticated gateway if access must cross a trusted network boundary. All `/v1` routes use the consumer's bearer key; health routes are unauthenticated. Jobs belong to the consumer identity, so overlapping rotation keys can access the same jobs. Another consumer receives 404 for those jobs.

Query parameters are rejected. Responses include `X-Request-ID` and `Cache-Control: no-store`. JSON is the default representation. API version `1` is a string; URLs in responses are relative to the base URL. Timestamps use RFC 3339 UTC. Job and instance IDs are opaque 32-character hexadecimal strings. There is no list endpoint, webhook, URL ingestion, or scheduled publishing endpoint in v1. The calling Rails application owns durable scheduling and service-specific publication times.

### Create a job

`POST /v1/jobs` requires `Content-Type: multipart/form-data; boundary=...`, bearer authentication, and `Idempotency-Key`. The key must contain 16–128 printable ASCII characters without spaces. Send exactly two multipart parts:

1. `request`: `Content-Type: application/json`, no filename, at most 16 KiB.
2. `file`: the raw media bytes. Its filename is optional, limited to 255 characters, and never used as a filesystem path. Use `application/octet-stream` when the MIME type is unknown.

| JSON field | Type | Requirement |
|---|---|---|
| `operation` | string | Required; one of the seven operations above |
| `input.size_bytes` | integer | Required; 1 through `max_upload_bytes`, equal to uploaded bytes |
| `input.sha256` | string | Required; 64 hexadecimal characters; case-insensitive on input |
| `parameters` | object | Required, including `{}` for operations without parameters |
| `output_format` | string | Required; one of the selected operation's formats |

Unknown object fields, duplicate JSON keys, null parameters and parameters belonging to another operation are rejected. The request contains no input URL or output path. A newly accepted upload is size/checksum-verified before the service returns **202 Accepted**, `Location: /v1/jobs/{id}`, `Retry-After: 2`, and the job representation below. Media decoding and processing occur asynchronously; 202 does not mean the media is valid or conversion succeeded.

| Operation | Required parameters | Optional parameters and defaults |
|---|---|---|
| `transcode` | None | `width` and `height` together; `fit="contain"`; `quality=23`; `audio_bitrate_kbps=128` |
| `resize` | `width`, `height` | `fit="contain"` |
| `thumbnail` | `timestamp_seconds`, `width`, `height` | `fit="contain"` |
| `extract_audio` | None | `audio_bitrate_kbps=128`, `channels=2` |
| `trim` | `start_seconds`, `duration_seconds` | `quality=23` |
| `remux` | None | None; send `{}` |
| `metadata` | None | None; send `{}` |

Parameter constraints:

- `width` and `height`: integers 16–4096, with their product at most `max_pixels`. Both must be supplied together. Transcode dimensions must be even; omitting both preserves source geometry subject to encoder requirements.
- `fit`: `contain` (preserve aspect ratio and pad) or `cover` (preserve aspect ratio and center-crop). A request requiring upscaling fails during processing.
- `quality`: integer 18–35; lower values request higher visual quality.
- `audio_bitrate_kbps`: integer 64–320. `channels`: integer 1 or 2.
- Time values: finite JSON numbers in seconds, from zero through `max_duration_seconds`. Thumbnail timestamp must be before the input's end. Trim duration must be greater than zero and `start_seconds + duration_seconds` must not exceed the input duration.

Idempotency is scoped to the consumer. The fingerprint includes operation, output format, input size/hash and normalized parameter defaults. Repeating the same key and fingerprint returns **202** with the existing job and `Location`, even if that job has already completed or failed; the repeated file body is not processed again. A different fingerprint returns **409 `idempotency_conflict`**. A simultaneous request while the first upload is still being received returns **409 `idempotency_in_progress`**, with `Retry-After: 2`. A retained deleted/expired record returns **410**. Aborted uploads release their key. Records disappear after their retention period or a restart; callers must retain their own source and processing state.

### Job representation and polling

`GET /v1/jobs/{id}` returns **200** and a job object. This illustrative response shows a newly accepted job:

```json
{
  "api_version": "1",
  "instance_id": "11111111111111111111111111111111",
  "job_id": "22222222222222222222222222222222",
  "status": "queued",
  "created_at": "2026-10-22T12:00:00Z",
  "completed_at": null,
  "expires_at": null,
  "status_url": "/v1/jobs/22222222222222222222222222222222",
  "download_url": null,
  "result": null,
  "error": null
}
```

| Field/state | Meaning |
|---|---|
| `queued`, `processing` | Accepted work waiting for or using a processing slot; polling responses include `Retry-After: 2` |
| `completed` | Result is available; `download_url` and `result` are populated |
| `failed` | Processing failed; `error` is populated and media is removed |
| `cancelling`, `cancelled` | Cancellation requested / processing stopped |
| `deleting` | Active downloads are being stopped before cleanup |
| `completed_at`, `expires_at` | Populated at completion, failure or cancellation; otherwise null |
| `result` | Object containing `content_type`, integer `size_bytes`, and lowercase hexadecimal `sha256`; otherwise null |
| `error` | Error object for failed processing; otherwise null |

Deleted or expired tombstones return an error with **410**, rather than a 200 job object. Once a tombstone is removed, the response is **404**. The internal upload reservation is not externally readable as a job.

Poll using `Accept: application/json`, observing `Retry-After` where present. Status supports `ETag` and an exact matching `If-None-Match` value, which returns **304** without a body. `Accept: text/html` selects an escaped HTML status page; `Vary: Accept` distinguishes representations. Unsupported representations return **406**. Cancellation is asynchronous: repeat DELETE until 204 to acknowledge complete removal.

### Download and delete

`GET /v1/jobs/{id}/download` returns **200** with the result bytes, media `Content-Type`, `Content-Disposition: attachment; filename=result.<format>`, and a quoted SHA-256 `ETag`. It supports one `Range: bytes=start-end`, `bytes=start-`, or `bytes=-length` range, returning **206** and `Content-Range`. Multiple or unsatisfiable ranges return **416** with `Content-Range: bytes */<size>`. Conditional download requests use standard HTTP conditional response behavior; a matching `If-None-Match` can return 304.

The result is available only in `completed` state. Queued/processing jobs return **409 `result_not_ready`**; other noncompleted states return **409 `result_unavailable`**. Downloads do not consume or delete results. Verify the full file's size and SHA-256 against `result`, commit it to durable storage, then delete the job. Partial downloads still need full-file checksum verification after assembly.

`DELETE /v1/jobs/{id}` has no request body. It returns **202** with a job representation when cancellation or transfer shutdown is pending, and **204** without a body when cleanup is complete. Repeat DELETE on a retained deleted tombstone also returns 204. Expired jobs return 410; unknown or inaccessible jobs return 404. Deletion cancels active downloads. There is no automatic retry of a failed processing job; use a new idempotency key for a deliberate new attempt.

The `metadata` operation also produces a downloadable file, with `application/json` content. It contains `format`, numeric `duration_seconds`, integer `width` and `height`, boolean `still_image`, and optional `video_codec`, `audio_codec`, and `channels`. It excludes arbitrary source tags. Dimensions are orientation-adjusted; audio-only inputs have zero dimensions.

### Errors

HTTP errors use this JSON envelope (messages are sanitized, and callers should branch on `code`):

```json
{
  "error": {
    "code": "capacity_exhausted",
    "message": "capacity exhausted.",
    "retryable": true,
    "request_id": "33333333333333333333333333333333"
  }
}
```

| HTTP status | Error codes / cause |
|---|---|
| 400 | `query_parameters_not_supported`, `invalid_idempotency_key`, `missing_request_part`, `malformed_request`, `missing_file_part`, `incomplete_upload`, `checksum_mismatch`, `unexpected_part` |
| 401 | `invalid_authentication`; includes `WWW-Authenticate: Bearer` |
| 404 | `job_not_found`; also hides ownership differences |
| 405 | `method_not_allowed` |
| 406 | `unsupported_representation` |
| 409 | `idempotency_conflict`, `idempotency_in_progress`, `result_not_ready`, `result_unavailable` |
| 410 | `job_deleted`, `job_expired` |
| 413 | `upload_too_large` (configured or declared size exceeded) |
| 415 | `unsupported_content_type` |
| 416 | `invalid_range` |
| 422 | `invalid_parameter` |
| 429 | `consumer_limit` |
| 500 | `internal_error` |
| 503 | `capacity_exhausted`, `storage_unavailable`, `processor_unavailable` |

429, 503 and `idempotency_in_progress` include `Retry-After: 2` and `retryable: true`. Other HTTP errors have `retryable: false`; `result_not_ready` still calls for polling the status endpoint. Retry transient failures with bounded backoff and jitter. A broken upload connection may yield no HTTP response; replay using the same idempotency key and request fingerprint.

Asynchronous failures appear in `job.error` on a **200 status response**, with one of: `invalid_media`, `media_type_mismatch`, `upscale_not_allowed`, `unsupported_media`, `invalid_parameter`, `no_audio_track`, `incompatible_output`, `processing_timeout`, `output_too_large`, `processing_failed`, or `invalid_output`. These errors have `retryable: false` and omit `request_id`. The error schema reserves optional `field`; current handlers do not populate it. Raw FFmpeg diagnostics are never returned.

### Health and capabilities

Both health endpoints return **200** with `{"ok":true}` while healthy. Readiness returns 503 during shutdown. The server only starts listening after mandatory sandbox, encoder and tmpfs checks succeed.

`GET /v1/capabilities` returns **200** with `api_version`, `instance_id`, an `operations` map (operation → supported output formats), a `codecs` map (format → codec description), and `limits`. The exposed limits are `max_upload_bytes`, `max_output_bytes`, `max_duration_seconds`, `max_pixels`, `max_concurrent_jobs`, `max_retained_jobs`, `max_jobs_per_consumer`, and `job_ttl_seconds`. Read these values rather than assuming the defaults below. A changed instance ID indicates that previous in-memory jobs may have been lost. The processing queue is bounded and does not promise FIFO ordering.

## Streaming client

`scripts/client.py` uses only Python's standard library. It hashes and uploads in bounded chunks, polls, verifies the result, atomically saves it without overwriting an existing file, then acknowledges deletion.

Run it **inside a caller container on the media network**, with a consumer-specific key file and mounted input/output directory:

```sh
python3 client.py /files/input.mp4 /files/output.mp4 \
  --keys /run/secrets/client_keys.json --consumer rails \
  --operation transcode --format mp4 \
  --parameters '{"width":1280,"height":720,"fit":"contain"}'
```

The client key file uses the same JSON structure, containing only that application's consumer. For Rails, use the OpenAPI contract with bounded streaming from PostgreSQL; do not buffer a whole file in Ruby memory.

## Job lifecycle and recovery

Queued → processing → completed/failed. DELETE on queued or processing work returns 202 while cancellation runs; status then becomes cancelled. A subsequent DELETE returns 204. GET never removes the result.

Completed output remains for 900 seconds. After verifying and committing it to durable storage, the caller deletes the remote job. Failed/cancelled media is removed immediately. Metadata is retained for 900 seconds, then expired/deleted tombstones remain another 900 seconds.

Downloads pin results until the response ends, with a 120-second deadline. Expiry blocks new downloads; deletion cancels active transfers. Resource accounting retains a pinned result until it can be removed.

The registry and idempotency keys are ephemeral. After restart, clients receive 404 for lost jobs and resubmit from their durable sources. A stable idempotency key prevents duplicate processing only while its record exists in the running instance; never treat it as durable exactly-once execution.

## Limits and configuration

| Variable | Default |
|---|---:|
| MAX_UPLOAD_BYTES | 268435456 |
| MAX_OUTPUT_BYTES | 268435456 |
| STORAGE_BUDGET_BYTES | 805306368 |
| MAX_CONCURRENT_JOBS | 1 |
| MAX_RETAINED_JOBS | 2 |
| MAX_JOBS_PER_CONSUMER | 2 |
| MAX_JOB_RECORDS | 1000 |
| MAX_PIXELS | 16777216 |
| MAX_DURATION_SECONDS | 600 |
| PROCESS_TIMEOUT_SECONDS | 120 |
| UPLOAD_TIMEOUT_SECONDS | 120 |
| JOB_TTL_SECONDS | 900 |
| TOMBSTONE_TTL_SECONDS | 900 |

Other variables: HTTP_ADDR (default :8080), WORK_DIR (/work), API_KEYS_FILE (/run/secrets/api_keys), SANDBOX_PATH (/usr/local/bin/media-sandbox). Compose and the healthcheck use port 8080; keep that port unless also updating the healthcheck.

Reserve declared input bytes plus the maximum possible output before accepting a job. Completed results count against capacity until deleted/expired. Global saturation returns 503; a per-consumer limit returns 429. Both include Retry-After.

The container has a 1 GiB tmpfs, 2 GiB memory ceiling and 128 PID limit. Each processing invocation has a 768 MiB virtual-memory ceiling, CPU/time limits and a kernel file-size limit. There are at most 32 active authenticated HTTP handlers. These conservative defaults prioritize predictable failure over throughput.

Increase limits together: tmpfs must exceed the admission budget and container RAM must cover tmpfs plus simultaneous decoding and the API. tmpfs may use host swap; the Compose swap limit requests no additional container swap, and the host must enforce the intended swap policy.

## Security model

See [SECURITY.md](SECURITY.md). The main controls are:

- Read-only root, UID 10001, dropped capabilities, no-new-privileges, bounded tmpfs and no host port.
- A seccomp entrypoint blocks new outgoing connections and datagrams for the API and all descendants, while allowing inbound HTTP replies.
- A separate helper applies per-job Landlock filesystem rules and a stricter seccomp filter before every FFmpeg/FFprobe execution.
- Only that job's input/output, required runtime libraries, and the FFmpeg executables are readable. Only its already-created output is writable. Secrets, other jobs and /proc process memory are inaccessible.
- Direct process execution, sanitized errors, bounded output/log buffers, strict schemas and explicit media/protocol allowlists.

No API request bodies, tokens, source names or FFmpeg stderr are logged. Lifecycle startup/shutdown logs go to stdout. Codec/package manifests are recorded under /usr/share/ffmpeg-service in the image.

## Development and CI

```sh
make test
make build
docker build -t ffmpeg-service:test .
sh scripts/container-smoke.sh ffmpeg-service:test
```

Go tests include an explicitly test-only direct executor for generated media fixtures. Production code always uses the sandbox. Tests do not imply sandbox containment was verified on a kernel that lacks Landlock.

GitHub Actions runs native amd64/arm64 tests, Docker build, container API/confinement smoke tests, a vulnerability scan and SBOM generation. No images or releases are published automatically. The workflow must pass on the target-compatible runners before deploying. The image build uses digest-pinned Alpine/Go bases, installs current repository FFmpeg packages, and records their exact versions. Pin the final image digest after a successful scan; rebuild regularly for security patches.

The fixed scanner policy fails on fixable high/critical findings; review unfixed findings too. Trivy is pinned to a verified release commit. The remaining CI action references are version tags and should be pinned by the repository's supply-chain policy before public release.
