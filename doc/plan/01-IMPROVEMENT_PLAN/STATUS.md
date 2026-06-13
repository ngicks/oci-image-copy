# oci-image-copy — Improvement Plan 01 — Status

| Entry | Title | Status | Commit |
|-------|-------|--------|--------|
| O1 | manifest digest verification (trust root) | done | 7b00620 |
| O2 | index bounds + ParseIndex choke point | done | 401b303 |
| O3 | ssh cancellation/BatchMode + Output dedup | done | 05b0934 |
| O4 | chunk-name algorithm + probe error taxonomy | done | 2466945 |
| O5 | imageref host/path/tag validation | done | 535c6ec |
| O6 | cli wrapper dedup + shellQuote tests + header redaction | done | 656fdd0 |

Baseline: all module tests green (2026-06-13).
Per-entry summaries: `SUMMARY-<entry>.md` in this directory.
