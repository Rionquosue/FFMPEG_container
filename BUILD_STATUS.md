# Build status — 22 October 2026

Initial implementation: 0.1.0. Repository: [Rionquosue/FFMPEG_container](https://github.com/Rionquosue/FFMPEG_container). No image publication, release publication or server deployment has been performed.

## Completed locally

| Check | Result |
|---|---|
| Go tests with race detector | 43 tests/subtests passed; no failures or race reports |
| Real media fixtures | Metadata, MP4/WebM transcode, PNG/JPEG resize, thumbnail, M4A/MP3/Opus audio extraction, trim and remux passed |
| Lifecycle/API | Authentication, ownership, checksum failure, malformed multipart/JSON, idempotency/races, cancellation, limits, range responses, HTML, ETags, expiry and cleanup covered |
| Static analysis | go vet passed |
| Go binary build | Linux amd64 and cross-compiled arm64 passed |
| Sandbox helper build | Native amd64 compilation with -Wall -Wextra -Werror passed |
| API seccomp egress self-test | TCP connect, datagram sockets and Unix sockets denied as expected |
| OpenAPI | OpenAPI 3.1 validated; positive/negative request examples validated |
| Supporting files | Python compilation, shell syntax, Compose/workflow YAML parsing passed |

Toolchain: verified official Go 1.27.3 archive. Media tests used the workspace's Ubuntu FFmpeg 6.1.1. They use an executor that exists only in test code; production has no unrestricted executor switch.

## Not verified in this environment

- Docker image build and startup: no Docker/Podman executable or daemon is available here.
- Per-job Landlock confinement: the workspace reports ENOSYS for the Landlock ABI query. Production must refuse startup on such a host.
- Alpine FFmpeg package behavior, native arm64 execution, container resource exhaustion, vulnerability scan and SBOM output.
- GitHub Actions execution: see the repository's [Actions runs](https://github.com/Rionquosue/FFMPEG_container/actions) for the result associated with each commit. The local checks above do not establish a successful container build or confinement run.

The included native amd64/arm64 CI workflow builds the actual image, requires sandbox startup tests, runs real API/media operations in the private Docker network, scans the image and creates an SBOM. Those gates must pass before treating this as deployable on your host.

## Implementation choices

- Uses seekable tmpfs input/output files for all operations. Transfers to/from callers remain streamed.
- Requires Landlock ABI >=3 plus seccomp instead of an optional sandbox.
- Adds a whole-API syscall egress guard, so the API cannot initiate network connections; it can answer incoming HTTP.
- Keys reload on restart; overlapping keys for one consumer are supported, but live reload is not implemented.
- SIGTERM stops accepting requests, allows up to 10 seconds for active HTTP requests, then cancels processing and cleans scratch.
- Metadata is deliberately limited to normalized format, duration, dimensions and codec/channel information.
- Registry, idempotency records, results and keys in memory are lost on restart.

See README.md for setup, SECURITY.md for boundaries, and docs/openapi.json for the concrete API.
