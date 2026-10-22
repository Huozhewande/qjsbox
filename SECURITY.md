# Security policy

qjsbox exists to contain untrusted code, so sandbox weaknesses are treated as
security bugs. Please report them privately through GitHub's
"Report a vulnerability" button on this repository rather than in a public
issue.

In scope:

- Escaping the worker sandbox: reading files, opening sockets, creating
  processes, or executing programs from script code.
- Bypassing a documented limit (time, memory, input/output size, read budget,
  concurrency) or making the Go supervisor hang.
- Reaching removed capabilities (`Function`, `eval`, `Date`, `Math.random`),
  mutating input, or reading undeclared factors.
- Making a missing or mistyped required value produce a successful result.

Out of scope:

- Behaviour of your own `ResolveFunc` or `OnReuse` callbacks, which are trusted
  host code.
- The macOS RSS sampler overshooting; it is documented as a soft guard.
- Platforms other than Linux x86-64/AArch64 and macOS, where the worker refuses
  to run.

Please include the platform, the output of `make selftest`, and a minimal
script that reproduces the issue.
