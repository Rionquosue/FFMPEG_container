# Security boundaries

This is an initial implementation, not an independently audited isolation product.

## Required host controls

Use a patched Linux kernel with Landlock ABI >=3, Docker's standard seccomp policy with Landlock syscalls permitted, read-only runtime root, no added capabilities, no-new-privileges and dedicated tmpfs. If startup fails, update or configure the runtime appropriately; never silently drop the sandbox.

The C helper is not setuid and needs no elevated capabilities. Landlock restricts each processing child to specific input/output inodes, required runtime libraries and executable files. The output inode is created by the API before execution. An exploited decoder cannot open another job's files or the API secret through ordinary filesystem access.

The child's seccomp filter denies socket creation, ptrace, process_vm operations, cross-process signals, pidfd access, namespace changes, mount operations, permission changes, io_uring and process creation except thread clones. clone3 deliberately returns ENOSYS to permit compatible libc thread fallback.

The API entrypoint permits only Internet-family stream sockets, rejects connect and datagram/sendmsg paths, and denies io_uring. The API can accept incoming HTTP and write responses, but cannot initiate outgoing connections even to a peer or Docker host. Startup checks that the connect denial is active. Child filters are additional restrictions inherited from the entrypoint.

A dedicated internal bridge is still required. An internal bridge alone does not isolate its peers or the host. Avoid unrelated peers, and keep the worker from exposing a proxy or forwarding packets. Optional host firewalls provide an additional boundary; the container does not manage them or require NET_ADMIN.

## Limits

- The kernel, runtime, helper and Go API remain part of the trusted computing base.
- Filesystem metadata visibility is not the same as file-read access. The service promises access restrictions, not a hidden virtual filesystem.
- Dynamic runtime library directories are readable. Never mount secrets or user files under /lib or /usr/lib.
- No host mount, socket, device, /proc allow rule or parent directory of /work is granted to FFmpeg.
- The same UID is used inside the container. Protection between jobs depends on Landlock and seccomp, not directory permissions alone.
- Hard CPU/address-space/file limits may reject legitimate large or complex media. They are deliberate.
- OOM termination loses all jobs; the caller owns retries and source retention.
- API errors never include FFmpeg stderr. Diagnose using reproducible safe fixtures, not by exposing internal commands or credentials.

## Release checks

The container smoke test invokes startup checks that attempt to open the real API secret and a sibling sentinel from the processing sandbox. They must fail with EACCES. Socket creation and ptrace must fail with EPERM. Actual sandboxed FFmpeg encoder discovery and FFprobe startup must succeed.

The host egress filter has a separate executable self-test:

```sh
media-sandbox --check-egress
```

Run the native container suite on amd64 and arm64, including normal media processing, and review the resulting vulnerability scan/SBOM. Verify no published ports and no writable filesystem outside tmpfs. Before accepting untrusted public uploads, add independent security review, load testing and an adversarial decoder corpus appropriate to your deployment.

Report suspected vulnerabilities privately to the repository owner. Do not include secrets or private media in public issues.
