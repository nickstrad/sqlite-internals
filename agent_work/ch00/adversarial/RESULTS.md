# Chunk 0 adversarial results (orchestrator's record)

Run 1 — 2026-11-20, on the build as first written (before repair), `run-all.sh`, 7 s.
Raw output: `results-run1.txt`. 30 attacks HELD, 8 BROKEN:

| Attack | Finding | Disposition |
| --- | --- | --- |
| PIN-02 | A module pinned to driver v1.58.0 (same SQLite 3.53.4) passes the pin check: the check compares the source id only, not the driver module version | to repair |
| CHK-07 | `check prose` treats any `//word:` comment as a directive, so `//note:…` prose by a non-Neon author passes | to repair |
| TRC-02 | strace `<unfinished …>` split after a comma shifts arguments (same defect as review finding 4) | to repair |
| TRC-03 | `? ERESTARTSYS` fsync counted as a succeeded fsync | to repair |
| TRC-06 | fd→path map is per process but keyed without regard to close/reopen from another thread: stale path attribution | to repair |
| TRC-08 | paths annotated `(deleted)` by strace are dropped by `UnderDir`, hiding writes/syncs to unlinked-while-open files | to repair |
| TRC-09 | merged stream loses a VFS call whose stdout line is split across two `write(1)` calls | to repair |
| SET-04 | sub-millisecond `BusyTimeout` passes `Validate()` then fails the post-apply comparison | to repair |
