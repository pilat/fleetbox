# ADR: CLI Mode Uses a Re-Exec'd Runner Process per VM

**Date:** 2026-06-03
**Status:** Accepted (superseded on macOS by ADR-0017; still in force on Linux)

## Context

A VZ virtual machine is an in-process object: it lives exactly as long as the process
that created it. In library mode this is fine — the test process owns its VMs and they
should die with it. But `fleetbox up myvm` must return to the shell while the VM keeps
running. Someone has to hold the VM object.

Comparable tools solve this with a host-agent daemon that also does port forwarding,
guest-agent communication, and filesystem mounting — and that accumulation of jobs is
exactly what makes such daemons fragile.

## Decision

1. **`fleetbox up` re-execs itself** with a hidden `--fleetbox-runner <name>` flag. The
   re-exec'd process (the *runner*) boots the VM through the same public
   `fleetbox.Start()` API, writes a pidfile, and listens on a unix socket.
2. **The runner's entire job is**: hold the VM object, answer `status`, answer `stop`
   (graceful ACPI shutdown, 30s budget). That is the complete protocol. No forwarding,
   no guest communication, no tunnels — the runner must never accumulate the
   host-agent grab-bag of responsibilities.
3. **One runner per VM.** No shared daemon, no state beyond the VM it holds.
4. **In library mode no runner exists.** The test process holds the VMs directly.
5. **CLI options cross the re-exec boundary** as values: `Option` funcs are applied to
   an `Options` struct, serialized to JSON, and passed via the `FLEETBOX_OPTS`
   environment variable.

## Alternatives Considered

**A single shared daemon managing all VMs (the Docker-style model).** Rejected: a
daemon is a lifecycle problem (who starts it, who upgrades it, what happens when it
crashes with 5 VMs inside). One runner per VM means one VM lost per crash, and `ps`
shows exactly what's running.

**launchd services per VM.** Rejected: ties fleetbox to macOS service management,
complicates cleanup, and makes the "process owns VM" model implicit instead of
explicit.

**Keeping the CLI process in the foreground (no detach).** Rejected as the only mode:
fine for debugging, useless for `up && run-tests && down` workflows. (The runner's
output is still captured to `runner-<name>.log` for debugging.)

## Consequences

- `fleetbox up` returns once the VM has an IP and SSH is ready; the runner keeps
  running detached (setsid).
- Liveness is pidfile + signal-0 checks; status is a socket round-trip. Stale
  pidfiles/sockets after a crash are cleaned up by `rm`.
- The CLI binary and the runner are the same binary — there is no separate daemon
  artifact to build, sign, or version.

**Amended by ADR-0009.** "One runner per VM" became "one *holder* per `up` group" so a
CLI cluster's VMs can share one in-process vmnet network (the cluster feature, ADR-0008).
Everything else here stands — same re-exec'd binary, same minimal socket protocol (now
also `addmember`), no forwarding or guest protocol. The single tradeoff: a holder crash
now loses a whole cluster rather than one VM.
