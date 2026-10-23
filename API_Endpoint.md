# API endpoints

Reference for every endpoint of the FFmpeg service (API version 1), with how to call each one. The contract is defined by SignalCraft design sheet 03; [`docs/openapi.json`](docs/openapi.json) is the machine-readable OpenAPI 3.1 version.

## Contents

- [Connecting](#connecting)
- [Authentication](#authentication)
- [Common response headers](#common-response-headers)
- [Endpoint summary](#endpoint-summary)
- [GET /health/live](#get-healthlive)
- [GET /health/ready](#get-healthready)
- [GET /v1/capabilities](#get-v1capabilities)
- [POST /v1/jobs](#post-v1jobs)
- [GET /v1/jobs/{id}](#get-v1jobsid)
- [GET /v1/jobs/{id}/download](#get-v1jobsiddownload)
- [DELETE /v1/jobs/{id}](#delete-v1jobsid)
- [Operations and parameters](#operations-and-parameters)
- [Job object and lifecycle](#job-object-and-lifecycle)
- [Errors](#errors)
- [Typical client flow](#typical-client-flow)

## Connecting

| Transport | Address | Who may connect |
|---|---|---|
| TCP (default) | `http://ffmpeg:8080` on the private Docker network | Loopback, peers on the container's attached Docker networks, and `WHITELIST_IPS` |
| Host machine | `http://127.0.0.1:<port>` when published as `127.0.0.1:<port>:8080` | The host's localhost |
| Unix socket | `UNIX_SOCKET` path (for example `/run/ffmpeg-api/api.sock`) on a shared volume | Processes with access to the socket (mode 0660) |

Connections from any other address are closed before HTTP is read. All paths below are relative to the base URL; there is no TLS on the private network.

```sh
# TCP, from a container on the shared Docker network
curl -H "Authorization: Bearer $KEY" http://ffmpeg:8080/v1/capabilities

# Unix socket
curl --unix-socket /run/ffmpeg-api/api.sock -H "Authorization: Bearer $KEY" http://localhost/v1/capabilities
```

Query strings are not accepted on any path (`400 malformed_request`).

## Authentication

Every `/v1/...` request needs `Authorization: Bearer <key>`. Keys are configured per consumer in the keys file (`API_KEYS_FILE`); a consumer may have up to four keys at once so keys can be rotated. Jobs belong to the consumer, not the key: any of a consumer's keys can reach its jobs, and another consumer gets `404` for them.

```json
{ "consumers": [ { "id": "signalcraft", "keys": ["<43–256 base64url or hex characters>"] } ] }
```

Missing or unknown keys get `401 invalid_authentication` with `WWW-Authenticate: Bearer`. Never put a key in a URL. The health endpoints need no key.

## Common response headers

| Header | When |
|---|---|
| `X-Request-ID` | Every response; quote it when reporting a problem |
| `Cache-Control: no-store` | Every response |
| `X-Content-Type-Options: nosniff`, `Content-Security-Policy`, `Referrer-Policy: no-referrer` | Every response |
| `Retry-After: 2` | Pending jobs, `429`, `503` and `409 idempotency_in_progress` |

## Endpoint summary

| Method and path | Auth | Success | Purpose |
|---|---|---|---|
| `GET /health/live` | No | 200 | Process is running |
| `GET /health/ready` | No | 200 / 503 | Ready to accept work |
| `GET /v1/capabilities` | Yes | 200 | Operations, formats and limits of this instance |
| `POST /v1/jobs` | Yes | 202 | Upload one file and start one job |
| `GET /v1/jobs/{id}` | Yes | 200 / 304 | Job status (JSON or HTML) |
| `GET /v1/jobs/{id}/download` | Yes | 200 / 206 | Download the result |
| `DELETE /v1/jobs/{id}` | Yes | 202 / 204 | Cancel and remove a job |

---

## GET /health/live

Liveness of the process. No authentication.

```sh
curl http://ffmpeg:8080/health/live
```

`200` → `{"ok": true}`

## GET /health/ready

Readiness. The server only listens after its sandbox, encoder and tmpfs checks pass; this returns `503 processor_unavailable` while shutting down. The image's Docker healthcheck (`ffmpeg-api healthcheck`) calls it.

```sh
curl http://ffmpeg:8080/health/ready
```

`200` → `{"ok": true}`

## GET /v1/capabilities

What this instance supports. Re-read it whenever `instance_id` changes (a restart), because job records do not survive restarts.

```sh
curl -H "Authorization: Bearer $KEY" http://ffmpeg:8080/v1/capabilities
```

```json
{
  "api_version": "1",
  "instance_id": "9f2c…",
  "operations": {
    "transcode": ["mp4", "webm"], "resize": ["jpg", "png"], "thumbnail": ["jpg", "png"],
    "extract_audio": ["m4a", "mp3", "opus"], "trim": ["mp4", "webm"], "remux": ["mp4", "webm"],
    "metadata": ["json"]
  },
  "parameters": {
    "transcode": ["width", "height", "fit", "quality", "audio_bitrate_kbps", "audio_mode", "max_dimension", "max_fps", "max_bitrate_kbps"],
    "trim": ["start_seconds", "duration_seconds", "quality", "width", "height", "fit", "audio_bitrate_kbps", "audio_mode", "max_dimension", "max_fps", "max_bitrate_kbps"],
    "remux": ["audio_mode"], "metadata": [], "…": []
  },
  "codecs": { "mp4": "h264/aac", "webm": "vp9/opus", "jpg": "mjpeg", "png": "png", "m4a": "aac", "mp3": "mp3", "opus": "opus" },
  "limits": {
    "max_upload_bytes": 268435456, "max_output_bytes": 268435456, "max_duration_seconds": 600,
    "max_pixels": 16777216, "max_concurrent_jobs": 1, "max_retained_jobs": 2,
    "max_jobs_per_consumer": 2, "job_ttl_seconds": 900,
    "process_timeout_seconds": 120, "upload_timeout_seconds": 120
  }
}
```

`parameters` lists the parameter names each operation accepts; use it to detect optional features rather than assuming a service version.

## POST /v1/jobs

Uploads one input file and starts one job. The body is `multipart/form-data` with **exactly two parts, in this order**:

1. `request` — `Content-Type: application/json`, at most 16 KiB.
2. `file` — the input bytes, exactly `input.size_bytes` long. The filename is display metadata only (at most 255 characters); neither it nor the part's `Content-Type` decides the format. A declared type that contradicts the detected format fails the job with `unsupported_media`.

Required header: `Idempotency-Key`, 16–128 printable ASCII characters.

Request JSON:

```json
{
  "operation": "thumbnail",
  "input": { "size_bytes": 10485760, "sha256": "<64 lowercase hex characters>" },
  "parameters": { "timestamp_seconds": 2.5, "width": 640, "height": 360, "fit": "cover", "jpeg_quality": 80 },
  "output_format": "jpg"
}
```

Unknown fields, duplicate keys, `null` parameters and wrong types are rejected. The request part is validated before any file bytes are read, so schema errors and oversize declarations cost no upload. The file is then streamed and hashed; size and SHA-256 must match the declaration.

```sh
SHA=$(sha256sum clip.mp4 | cut -d' ' -f1); SIZE=$(stat -c%s clip.mp4)
curl -H "Authorization: Bearer $KEY" -H "Idempotency-Key: thumb-$(uuidgen)" \
  -F "request={\"operation\":\"thumbnail\",\"input\":{\"size_bytes\":$SIZE,\"sha256\":\"$SHA\"},\"parameters\":{\"timestamp_seconds\":1,\"width\":320,\"height\":180},\"output_format\":\"png\"};type=application/json" \
  -F "file=@clip.mp4" \
  http://ffmpeg:8080/v1/jobs
```

**202 Accepted** returns the [job object](#job-object-and-lifecycle) with `status: "queued"`, plus `Location: /v1/jobs/{id}` and `Retry-After: 2`.

### Idempotency

The key is scoped to the consumer. A fingerprint of the operation, format, declared input and parameters (after defaults) is stored with it until the job's tombstone expires or the service restarts.

| Situation | Response |
|---|---|
| Same key, same fingerprint, job accepted | The file part is hashed without being stored; if it matches, `202` with the original job (no second execution). If not, `400 checksum_mismatch` and the original job is unaffected |
| Same key, different fingerprint | `409 idempotency_conflict`, before the file is read |
| Same key while the first upload is still in progress | `409 idempotency_in_progress` with `Retry-After` |
| Same key after the job was deleted or expired | `410 job_deleted` / `410 job_expired` |

Clients that already have a job ID should poll it instead of replaying the upload. A broken connection during upload creates no job; replay with the same key.

### Errors from this endpoint

| HTTP | Code | Cause |
|---|---|---|
| 400 | `malformed_request` | Bad `Idempotency-Key`, query string, malformed JSON, extra parts, or upload shorter than declared |
| 400 | `missing_part` | First part is not `request` or second is not `file` (`field` says which) |
| 400 | `checksum_mismatch` | Received bytes do not match `input.sha256` |
| 413 | `upload_too_large` | Declared or received size above `max_upload_bytes` |
| 415 | `unsupported_content_type` | Not multipart, or the request part is not `application/json` |
| 422 | `unsupported_operation` | Unknown `operation` (`field: operation`) |
| 422 | `incompatible_output` | `output_format` not allowed for the operation (`field: output_format`) |
| 422 | `invalid_parameter` | Any other schema or range error; `field` names it, e.g. `parameters.width` |
| 429 | `consumer_limit` | The consumer already holds `max_jobs_per_consumer` jobs; delete finished ones |
| 503 | `capacity_exhausted`, `storage_unavailable`, `processor_unavailable` | Server full or shutting down; retry after `Retry-After` |

## GET /v1/jobs/{id}

Current state of a job. Returns `200` even when processing failed: the failure is in the body's `error`.

```sh
curl -H "Authorization: Bearer $KEY" http://ffmpeg:8080/v1/jobs/$ID
```

- **Representation:** JSON by default. `Accept: text/html` returns a small escaped status page (still bearer-authenticated; intended for a backend to relay). Other `Accept` values get `406 unsupported_representation`. Responses carry `Vary: Accept`.
- **Polling:** while `queued`, `processing` or `cancelling`, responses include `Retry-After: 2`. Poll about every 2 seconds with jitter.
- **Conditional requests:** each response has an `ETag` (different per representation). Send it back as `If-None-Match` to get `304 Not Modified` while nothing has changed.

| HTTP | Meaning |
|---|---|
| 200 | Job object |
| 304 | Unchanged since the given ETag |
| 404 `job_not_found` | Unknown ID, another consumer's job, or lost after a restart |
| 410 `job_deleted` / `job_expired` | Tombstone; the job's files are gone |

## GET /v1/jobs/{id}/download

The result bytes of a `completed` job. Downloading never deletes the result.

```sh
curl -H "Authorization: Bearer $KEY" -o result.png http://ffmpeg:8080/v1/jobs/$ID/download
# Resume from byte 1048576
curl -H "Authorization: Bearer $KEY" -H "Range: bytes=1048576-" -o rest.part http://ffmpeg:8080/v1/jobs/$ID/download
```

- `Content-Type` is the service's own type for the output format; `Content-Disposition: attachment; filename=result.<format>`; `ETag` is the quoted SHA-256; `Accept-Ranges: bytes`.
- One range per request: `bytes=start-end`, `bytes=start-` or `bytes=-suffix` → `206` with `Content-Range`. Multiple or unsatisfiable ranges → `416 invalid_range` with `Content-Range: bytes */<size>`.
- Each transfer has a 120-second deadline. Every request, including resumed ranges, needs the bearer key.
- Verify the complete file's size and SHA-256 against `result` before using it.

| HTTP | Meaning |
|---|---|
| 200 / 206 | Bytes |
| 409 `result_not_ready` | Still queued or processing |
| 409 `result_unavailable` | Failed, cancelled or otherwise has no result |
| 404 / 410 | As for status |

## DELETE /v1/jobs/{id}

Cancels a running job and removes its files. Call it once the result is safely stored; jobs count against `max_jobs_per_consumer` until deleted or expired.

```sh
curl -X DELETE -H "Authorization: Bearer $KEY" http://ffmpeg:8080/v1/jobs/$ID
```

| Current state | Response | Then |
|---|---|---|
| `queued` or `processing` | 202 with the job (`cancelling`) | Process group killed, files removed, job becomes the `deleted` tombstone |
| `cancelling` | 202 | Same |
| `completed`, `failed` or `cancelled` | 204 | `deleted` tombstone |
| Same, while a download is in progress | 202 | Becomes `deleted` at once; files are removed when the cancelled download ends |
| `deleted` or `expired` tombstone | 204 | Nothing to do (idempotent) |
| Unknown or another consumer's | 404 | — |

A client DELETE always ends in `deleted`; `cancelled` only appears when the server itself stops a job.

---

## Operations and parameters

Each operation accepts only its own parameters. Dimensions are integers from 16 to 4096, their product at most `max_pixels`, and must be given together. Times are finite non-negative seconds within `max_duration_seconds`. `fit` is `contain` (keep aspect ratio, pad) or `cover` (scale and centre-crop). Upscaling is never done.

| Operation | Output formats | Parameters (defaults) |
|---|---|---|
| `transcode` | `mp4` (H.264 High/AAC), `webm` (VP9/Opus) | `width`, `height` (optional pair, **even**), `fit` (`contain`), `quality` 18–35 (23), `audio_bitrate_kbps` 64–320 (128), `audio_mode` (`preserve`), `max_dimension`, `max_fps`, `max_bitrate_kbps` (mp4 only) |
| `resize` | `jpg`, `png`; still images only | `width`, `height` (required), `fit` (`contain`), `jpeg_quality` 50–95 (85, jpg only) |
| `thumbnail` | `jpg`, `png`; one video frame | `timestamp_seconds` (required), `width`, `height` (required), `fit` (`contain`), `jpeg_quality` 50–95 (85, jpg only) |
| `extract_audio` | `m4a` (AAC), `mp3`, `opus` (Ogg) | `audio_bitrate_kbps` 64–320 (128), `channels` 1 or 2 (2) |
| `trim` | `mp4`, `webm` | `start_seconds` (required), `duration_seconds` > 0 (required), and the same optional parameters as `transcode` |
| `remux` | `mp4`, `webm` | `audio_mode` (`preserve`); stream copy only (H.264/AAC into MP4; VP8/VP9/AV1 with Opus/Vorbis into WebM; with `audio_mode: "remove"` only the video codec must fit) |
| `metadata` | `json` | none; the result file holds `format`, `duration_seconds`, `width`, `height`, `still_image`, and optional `video_codec`, `audio_codec`, `channels`, `frame_rate` |

Video parameters added in 0.2.0, all optional:

| Parameter | Range | Effect |
|---|---|---|
| `audio_mode` | `preserve` or `remove` | `remove` writes no audio stream; the output is probed and the job fails with `processing_failed` if any audio remains |
| `max_dimension` | even integer 16–4096 | Longest edge of the output after auto-rotation. Keeps the aspect ratio, never pads, never upscales; odd source sizes are rounded down to even. Cannot be combined with `width`/`height` |
| `max_fps` | integer 1–60 | Frame-rate ceiling, applied only when the source is faster; a slower source keeps its rate |
| `max_bitrate_kbps` | integer 100–20000, `mp4` only | Capped CRF (`-maxrate`, `-bufsize` twice the cap) with a slower encoder preset for smaller output |

Video operations always use the primary video stream and ignore attached cover art. MP4 video is H.264 High profile, `yuv420p`. Audio from sources with more than two channels is downmixed to stereo. `trim` seeks before decoding and re-encodes, so cuts are frame-accurate.

Inputs accepted: MOV/MP4, Matroska/WebM, MP3, WAV, FLAC, Ogg, AAC, PNG and JPEG. Playlists (HLS, concat), URLs, AVI, SVG/HTML, archives, animated images and files referencing external resources are rejected. Location and other source metadata are stripped from outputs; MP4 and M4A are written with faststart.

Example parameter sets:

```json
{"operation":"transcode","parameters":{"width":1280,"height":720,"fit":"contain","quality":23},"output_format":"mp4"}
{"operation":"resize","parameters":{"width":1080,"height":1080,"fit":"cover","jpeg_quality":82},"output_format":"jpg"}
{"operation":"extract_audio","parameters":{"audio_bitrate_kbps":96,"channels":1},"output_format":"opus"}
{"operation":"trim","parameters":{"start_seconds":5,"duration_seconds":15},"output_format":"webm"}
{"operation":"transcode","parameters":{"audio_mode":"remove","max_dimension":640,"max_fps":15,"quality":28,"max_bitrate_kbps":800},"output_format":"mp4"}
{"operation":"remux","parameters":{"audio_mode":"remove"},"output_format":"mp4"}
{"operation":"metadata","parameters":{},"output_format":"json"}
```

## Job object and lifecycle

```json
{
  "api_version": "1",
  "instance_id": "9f2c…",
  "job_id": "032f50ba9fda42b0bf5cb26d798d1ae2",
  "status": "completed",
  "created_at": "2026-10-22T14:00:00Z",
  "completed_at": "2026-10-22T14:00:09Z",
  "expires_at": "2026-10-22T14:15:09Z",
  "status_url": "/v1/jobs/032f50ba9fda42b0bf5cb26d798d1ae2",
  "download_url": "/v1/jobs/032f50ba9fda42b0bf5cb26d798d1ae2/download",
  "result": { "content_type": "image/png", "size_bytes": 4567, "sha256": "<64 hex>" },
  "error": null
}
```

`completed_at`, `expires_at`, `download_url`, `result` and `error` are `null` until they apply. Clients must ignore fields they do not know.

| From | Event | To |
|---|---|---|
| `queued` | Processing starts | `processing` |
| `processing` | Success / failure | `completed` / `failed` |
| `queued` or `processing` | Client DELETE | `cancelling` → `deleted` |
| `queued` or `processing` | Server shutdown | `cancelled` |
| `completed`, `failed` or `cancelled` | 900 s retention elapses | `expired` |
| `completed`, `failed` or `cancelled` | Client DELETE | `deleted` |

Failed and cancelled jobs release their files immediately; completed results are kept for `job_ttl_seconds` (900). `deleted` and `expired` are tombstones that answer `410` for another 900 seconds, then `404`.

## Errors

Synchronous errors (non-2xx responses) use one envelope:

```json
{
  "error": {
    "code": "invalid_parameter",
    "message": "Dimensions must be integers between 16 and 4096, even for video.",
    "field": "parameters.width",
    "retryable": false,
    "request_id": "33333333333333333333333333333333"
  }
}
```

Branch on `code`, not on `message`. `retryable` is true for `429`, `503` and `idempotency_in_progress`. Other HTTP codes not covered above: `405 method_not_allowed`, `500 internal_error`.

Asynchronous failures appear in the job's `error` (with `status: "failed"`) on a `200` status response:

| Code | Meaning | `field` | Retryable |
|---|---|---|---|
| `invalid_media` | Malformed or truncated media | — | no |
| `unsupported_media` | Unsupported format, animated image, external references, or declared type contradicts the content | — | no |
| `no_audio_track` | Audio required but absent | — | no |
| `incompatible_output` | Streams cannot be remuxed into the requested container | `output_format` | no |
| `upscale_not_allowed` | Requested size exceeds the source | `parameters.width` | no |
| `duration_exceeded` | Probed duration above `max_duration_seconds` | — | no |
| `dimensions_exceeded` | Source pixels above `max_pixels` | — | no |
| `range_out_of_bounds` | Trim range or thumbnail time past the end | `parameters.start_seconds` / `duration_seconds` / `timestamp_seconds` | no |
| `processing_timeout` | Computed processing time limit reached | — | yes |
| `output_too_large` | Result above `max_output_bytes` | — | no |
| `resource_limit` | Memory or file-size limit reached (reserved by the contract; not produced by this version) | — | yes |
| `processing_failed` | Other sanitized failure | — | yes |

Treat unknown codes as non-retryable. Retryable means a resubmission may work; use bounded backoff and a new idempotency key for a deliberate new attempt. Errors never include FFmpeg output, commands, paths or source metadata.

## Typical client flow

1. `GET /v1/capabilities`; remember `instance_id` and the limits.
2. `POST /v1/jobs` with a stable `Idempotency-Key` derived from your own processing record.
3. Poll `GET /v1/jobs/{id}` every ~2 s with `If-None-Match`, until `completed` or `failed`, within a deadline based on the processing timeout.
4. `GET /v1/jobs/{id}/download` (resume with `Range` if interrupted), then check size and SHA-256 against `result`.
5. Store the result durably, then `DELETE /v1/jobs/{id}`.
6. On `404` after a restart (`instance_id` changed), resubmit from the original stored input; on start-up, delete any remote jobs you no longer need.
