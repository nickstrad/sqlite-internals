VERDICT: NITS

## Arbiter checklist (`agent_work/pub/ARBITRATION.md`)

| Checklist item | Met? | How confirmed |
| --- | --- | --- |
| 1. Extract `id`, `href`, and `src` only from real opening tags (ignore text, `<pre><code>`, inline `<code>`, `<script>`/`<style>` bodies, escaped `&lt;…&gt;`, HTML comments) | Yes | Read `git diff 6003091..HEAD` (`internal/site/check.go:31-129`, `openingTagAttributes`); `TestCheckSiteIgnoresAttributeTextInCode`, `TestCheckSiteIgnoresAttributeTextInProseAndInlineCode`, `TestCheckSiteBrokenAnchorBesideSQLText`, and `TestCheckSiteHTMLEdges` PASS under `-race`; CLI probes on `<script>`, `<textarea>`, and `<!-- > … -->` matched expectations; adversarial `LINK-05`, `LINK-06`, `LINK-11a`, `LINK-11b` HELD. |
| 2. Parse tags accurately (`>` inside quoted value does not end tag, whitespace around `=`, double/single/unquoted values, case-insensitive tag/attr names, no unapproved dep bloat) | Yes | Read `internal/site/check.go:61-118` (quote-aware scanner, `isHTMLSpace` around `=`, unquoted value loop, `strings.ToLower` on tag/attr names, `html.UnescapeString`); `go.mod` unchanged; `TestCheckSiteTagScanner` PASS; adversarial `LINK-04` HELD; CLI probe caught unquoted `href`, boolean `href` (`empty link`), and `<script src=…>`. |
| 3. Keep duplicate-id detection and anchor resolution over real tag ids only (`LINK-10b` and `LINK-11b` fail) | Yes | Read `internal/site/check.go:155-167,180-188`; `TestCheckSiteTagScanner` (`id="real"` + `ID="real"`) and `TestCheckSiteBrokenAnchorBesideSQLText` PASS; adversarial `LINK-10b` HELD (`duplicate id "1-overview"`) and `LINK-11b` HELD (`missing anchor #alpha in #alpha`). |
| 4. Nothing else observable changes (`CheckSite` signature, messages, link resolution, real book `23 page(s), 1045 link(s) resolve` on both bases, rendered site unchanged) | Yes | `git diff --name-only 6003091..HEAD` touches only `internal/site/check.go` and `internal/site/site_test.go` in code (`CheckSite` signature, `checkLink`, and all rendering files untouched); `course site -check -base /sqlite-internals/` and `-base /` both report `PASS site -check: 23 page(s), 1045 link(s) resolve`. |
| 5. Regression tests in `internal/site/site_test.go` at `CheckSite` level covering items 5a–5f | Yes | Inspected `internal/site/site_test.go:456-576`: `TestCheckSiteIgnoresAttributeTextInCode` (5a, 5b), `TestCheckSiteBrokenAnchorBesideSQLText` (5c), `TestCheckSiteIgnoresAttributeTextInProseAndInlineCode` (5d + `<script>`/`<style>`), unchanged `TestCheckSiteHTMLEdges` (5e), and `TestCheckSiteTagScanner` (5e, 5f); all 24 `internal/site` tests PASS with `-race -count=1`. |
| 6. Re-run/record gates (`go test ./...`, both `site -check` bases, `check paths`, adversarial suite with `LINK-11a`/`11b` HELD and 0 regressions) and update `build-notes.md` | Yes | Verified `agent_work/pub/REPAIR-REPORT.md` (Round 2), `agent_work/pub/adversarial/results-run4.txt` (48 HELD, 0 BROKEN), fresh re-run of `run-all.sh` (48 HELD, 0 BROKEN), and `agent_work/pub/build-notes.md:13,334-338` updated to describe `openingTagAttributes` (document-wide regex scanning wording removed). |

## New findings

1. **nit** — `internal/site/check.go:120-126` (`openingTagAttributes` raw-text end-tag lookup)
   - **What is wrong:** After opening a raw-text element (`script`, `style`, `textarea`, `title`), line 121 searches for the closing tag using `closing := bytes.Index(bytes.ToLower(data[i:]), []byte("</"+tagName))` and advances `i += closing` into the original `data` slice. This has three minor edge cases:
     1. `bytes.ToLower` performs full Unicode per-rune lowercasing, which can change UTF-8 byte length (e.g., `Ⱥ` U+023A 2 bytes → `ⱥ` 3 bytes; `İ` U+0130 2 bytes → `i` 1 byte), shifting `closing` off the real byte offset in `data[i:]`.
     2. The match does not verify that `</` + `tagName` is followed by ASCII whitespace, `/`, or `>`, so a substring like `</scripture>` inside a `<script>` body terminates raw-text skipping prematurely.
     3. Calling `bytes.ToLower(data[i:])` allocates and lowercases the entire remainder of the HTML file on every page's `<title>` tag near the top of `<head>`.
   - **Proof (CLI probes with built `course` binary on scratch repos using `site -check -base /b/`):**
     - `<script>var s = "ȺȺȺȺȺȺȺȺȺȺ";</script><a href="/b/broken-after-script/">x</a>` → `PASS site -check: 1 page(s), 2 link(s) resolve` (exit 0; byte-length expansion skips past `<a href="/b/broken-after-script/">`).
     - `<script>var s = "İİ…(x30)…İ"; var b = "<a href='/b/in-script/'>";</script>` → `FAIL site -check: 1 problem(s)` / `index.html: broken link /b/in-script/` (exit 1; byte-length shrinkage lands back inside `<script>`).
     - `<script>var s = "</scripture><a href='/b/in-script/'>";</script>` → `FAIL site -check: 1 problem(s)` / `index.html: broken link /b/in-script/` (exit 1).
   - **Why nit, not blocker:** Generated pages in this book emit zero `<script>`, `<style>`, or `<textarea>` tags, and normal English/ASCII/UTF-8 `<title>` values do not contain runs of length-changing Unicode codepoints (a single shift inside `<title>` still lands before `<link rel="stylesheet">`). Worth tightening later with an ASCII-only case-insensitive scan checking the closing-tag delimiter (`isHTMLSpace`/`/`/`>`).

## Commands run

- `export PATH=/usr/local/go/bin:$PATH; go build ./... && go vet ./... && go test ./...` — PASS (all packages ok).
- `export PATH=/usr/local/go/bin:$PATH; bash agent_work/verify/crossbuild.sh` — PASS (`CROSSBUILD OK`: `linux/amd64`, `darwin/arm64`, `darwin/amd64` build/vet/test-compile, `check gofmt` 154 Go files).
- `export PATH=/usr/local/go/bin:$PATH; go test -race -count=1 -v ./internal/site/...` — PASS (24/24 tests, 0 races).
- `export PATH=/usr/local/go/bin:$PATH; go run ./cmd/course site -check -base /sqlite-internals/` — PASS (`23 page(s), 1045 link(s) resolve`).
- `export PATH=/usr/local/go/bin:$PATH; go run ./cmd/course site -check -base /` — PASS (`23 page(s), 1045 link(s) resolve`).
- `export PATH=/usr/local/go/bin:$PATH; go run ./cmd/course check paths` — PASS (`184 tracked text file(s)... 0 binary file(s) and 17 upstream SQLite copies skipped, 7 external link(s) reported`).
- `export PATH=/usr/local/go/bin:$PATH; bash agent_work/pub/adversarial/run-all.sh` — PASS (`48 HELD, 0 BROKEN`, 15 s; `agent_work/pub/adversarial/results.txt` updated; no BROKEN attacks to align).
- Comment-policy check (`git diff 6003091..HEAD -- internal/ cmd/`): PASS (0 comments added to `.go` files).
