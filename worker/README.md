# Sandboxed worker

Build from the repository root:

```sh
make worker      # or: worker/build.sh [output-path]
make selftest
```

The build needs a C compiler, its platform SDK, and Python 3. No network is used
at build time. The default output is `bin/qjsbox-worker`. Acorn and
`worker/bootstrap.js` are embedded into the executable; deployment does not need
their source files. The build verifies every vendored source against
`vendor/manifest.json`.

## Upstream provenance

- QuickJS **2025-09-13**, upstream archive revision 2:
  <https://bellard.org/quickjs/quickjs-2025-09-13-2.tar.xz>.
  Official project: <https://bellard.org/quickjs/>.
- Acorn **8.15.0**, the upstream npm release:
  <https://registry.npmjs.org/acorn/-/acorn-8.15.0.tgz>.
  Official project: <https://github.com/acornjs/acorn>.
- Archive SHA-256 values and individual source hashes are recorded in
  `vendor/manifest.json`. Both MIT licenses are retained beside their sources.
  Source files are unchanged. Only required QuickJS engine files are vendored;
  `quickjs-libc`, the command-line shell, module loaders, and OS bindings are
  neither included nor linked.

## Native boundary

The worker handles one request and exits. Standard output contains only newline
terminated JSON frames: factor reads, a final result, or a structured error.
Standard input accepts a request no larger than 2 MiB and factor-response frames
no larger than 1 MiB. Output frames are at most 64 KiB. The parent applies the
smaller public source/input/value limits before invoking this protocol.

A 900 ms monotonic deadline starts before sandbox initialization and covers
trusted parser/bootstrap compilation, request parsing, user code, host IPC,
encoding, and cleanup. QuickJS's interrupt hook cooperates with that deadline;
a periodic native alarm exits the process even when the interpreter is blocked.
The parent must still impose its own kill-and-wait watchdog. The QuickJS heap
has a hard 64 MiB allocator ceiling and its execution stack is limited to
512 KiB. OS limits disable core dumps and file growth, restrict descriptors to
16, and bound CPU time to one second and the native stack to 8 MiB. All inherited
descriptors above standard error are closed before reading untrusted data.

On Linux (x86-64 and AArch64), a hard 256 MiB address-space limit applies, followed
by `no_new_privs` and an architecture-checked seccomp allowlist. Only existing-FD
I/O, memory management, monotonic time, signal return, and process exit syscalls
are permitted. File opening, networking, process creation, and program execution
are denied. Unsupported platforms/architectures fail closed.

On macOS, Seatbelt installs a deny-by-default profile, allowing only read-only
sysctl and self process information. File reads, network connections, and
process creation remain denied. Sandbox initialization failure stops execution.
The built-in `--selftest-isolation` mode attempts native file access, a network
connection, and process creation after installing the same sandbox.

macOS rejects finite `RLIMIT_AS`, `RLIMIT_DATA`, and `RLIMIT_RSS` on the tested
host; the Mach physical-footprint limit also requires privileges unavailable to
this process. Consequently the hard allocator ceiling is supplemented by a
10 ms watchdog sampling process RSS and terminating above 128 MiB. **This sampled
RSS guard can overshoot and is not a hard OS memory ceiling.** Failure to read the
RSS also terminates execution. Production Linux uses the hard address-space
limit above. Platform guarantees must not be described as identical.
