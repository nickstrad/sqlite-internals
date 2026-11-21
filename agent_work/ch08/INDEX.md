# agent_work/ch08 — working files of chunk 8

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch08/build-notes.md` | plain-data notes for the prose pass | — |
| `agent_work/ch08/BUILD-REPORT.md` | build report | — |

No working scripts were kept. One scratch probe (deleted before commit) showed, before the
experiments were written: the modernc defaults (synchronous 2, secure_delete 0, cache_size −2000,
foreign_keys 0, busy_timeout 0, wal_autocheckpoint 1000, journal_size_limit −1); that
`default_cache_size` persists and seeds `cache_size`; that `VACUUM INTO` output has header bytes
18–19 = 1; that `mode=rw` on a missing file fails at open (`unable to open database file (14)`)
while a non-database file opens fine and fails at the first statement (`file is not a database
(26)`) — the last two are Neon's adversarial list for 8.1 and are not in the experiment.
