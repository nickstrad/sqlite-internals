ARBITRATION: FAIL STANDS

Arbiter: Muse Code (`muse-wonder-full-1.0`, third family, top seat). Branch `chunk/pub`,
HEAD `f673362` ("pub: record adversarial run 3"). Question: does Review 2's single new
blocker — the repair's document-wide attribute scan in `internal/site/check.go`
(`CheckSite`) false-matching single-quoted text — stand? Yes. It was reproduced
independently three ways below. No tracked code was edited for this arbitration
(a temporary unit probe was created, run, and deleted; `git status --short` is empty).

## What the repair changed (read from the diff)

`git show 73a8a3d -- internal/site/check.go` (pre-repair patterns confirmed at
`git show 6716cfe:internal/site/check.go`):

```text
-idAttrPattern   = regexp.MustCompile(`\sid="([^"]*)"`)
-linkAttrPattern = regexp.MustCompile(`\s(?:href|src)="([^"]*)"`)
+idAttrPattern      = regexp.MustCompile(`\sid\s*=\s*(?:"([^"]*)"|'([^']*)')`)
+linkAttrPattern    = regexp.MustCompile(`\s(?:href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
+htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)
```

Current `CheckSite` (`internal/site/check.go:17-65`) strips HTML comments, then runs
`idAttrPattern.FindAllSubmatch` / `linkAttrPattern.FindAllSubmatch` over the entire HTML
file bytes, records duplicate ids, and builds the anchor set from every match. Nothing
restricts matches to HTML opening tags. Review 1 finding 4 asked for single-quoted
attribute matching; the repair delivered it document-wide instead of tag-scoped, which is
the defect. The deploy gate is real: `.github/workflows/pages.yml` runs
`go run ./cmd/course site -check -base "..."` before building/deploying, and
`cmd/course/site.go:67` calls `site.CheckSite`.

## Evidence (arbiter's own runs, `go1.26.8 linux/amd64`)

Built from HEAD: `go build -o /tmp/arb-course ./cmd/course`. Probe books under
`/tmp/arb-repos/` mirror the adversarial harness's scratch-repo fixtures; each was checked
with `course site -check -base /book/`.

1. LINK-11a reproduced exactly (valid page must pass, fails instead). Chapter markdown
   with a fenced SQL block repeating `WHERE id = 'alpha'` plus a fenced HTML block
   showing `<a href='/book/single-in-code/'>`:

   ```text
   === A LINK-11a (valid page, EXPECT exit 0) (exit=1) ===
   FAIL site -check: 2 problem(s)
     ch02-beta/index.html: duplicate id "alpha"
     ch02-beta/index.html: broken link /book/single-in-code/
   ```

2. LINK-11b reproduced exactly (broken anchor must fail, passes instead). Page with
   `[broken](#alpha)` whose only `id = 'alpha'` is SQL inside a fenced block:

   ```text
   === B LINK-11b (broken #alpha, EXPECT exit 1 missing anchor) (exit=0) ===
   PASS site -check: 6 page(s), 36 link(s) resolve
   ```

3. Controls showing scope and mechanism (same probe run):

   ```text
   === C1 real single-quoted broken links (EXPECT exit 1) (exit=1) ===
   FAIL site -check: 2 problem(s)   [ /book/does-not-exist/, /book/missing.png ]
   === C2 prose id='alpha' x2, valid page (EXPECT exit 0) (exit=1) ===
   FAIL site -check: 1 problem(s)   [ duplicate id "alpha" ]
   === C3 inline-code id='alpha' x2, valid page (EXPECT exit 0) (exit=1) ===
   FAIL site -check: 1 problem(s)   [ duplicate id "alpha" ]
   === C4 double-quoted id="alpha" x2 in code (exit=0) ===
   PASS site -check: 6 page(s), 35 link(s) resolve
   === C5 single id='alpha' in code, valid page (exit=0) ===
   PASS site -check: 6 page(s), 35 link(s) resolve
   ```

   C1 proves single-quote support cannot simply be reverted: real single-quoted raw-HTML
   links must be caught (Review 1 #4 / LINK-04). C2/C3 prove the defect is broader than
   `<pre><code>`: prose and inline-code text also false-match, so stripping only `<pre>`
   blocks cannot fix it. C4/C5 show the asymmetry: one occurrence alone passes only
   because no duplicate/broken target is triggered, while it still silently pollutes the
   anchor set (the LINK-11b failure mode).

4. Escaping mechanism verified in the rendered HTML of cases A and C4
   (`course site -out ...`, then `grep` on `ch02-beta/index.html`):

   ```text
   WHERE id = 'alpha';                                  (single quote left raw)
   &lt;a href='/book/single-in-code/'&gt;example&lt;/a&gt;   (< escaped, ' raw)
   WHERE id = &quot;alpha&quot;;                        (double quote escaped)
   ```

   This is why the pre-repair double-quote-only patterns never matched code text, and why
   adding the single-quote alternative document-wide created the bug. It matches
   REVIEW-2.md's explanation word for word.

5. `CheckSite` isolated from goldmark (temporary `go test` probe on hand-written HTML,
   removed afterwards). A page containing only text nodes —
   `<p>prose id = 'alpha' and id = 'alpha'</p>` plus escaped code text plus
   `<a href="#alpha">` — produced:

   ```text
   text-node problems: [index.html: duplicate id "alpha" index.html: broken link /b/in-code/]
   ```

   Note `#alpha` produced no missing-anchor problem in the same page: the false positive
   and the false negative coexist. A second probe established baselines the fix must
   preserve or close:

   ```text
   quoted-gt/case/unquoted problems: [index.html: broken link /b/missing-after-gt/]
   ```

   i.e. today `<a title="a>b" href='/b/missing-after-gt/'>` is detected (a naive
   `<[^>]*>` tag regex would regress this), while uppercase `<A HREF='...'>` and unquoted
   `<a href=...>` are missed (pre-existing gaps in the same extractor).

6. Adversarial suite re-run fresh on HEAD
   (`go run ./agent_work/pub/adversarial/site-link-and-check`): `LINK-11a ... BROKEN` and
   `LINK-11b ... BROKEN` with the same GOT strings as `results-run3.txt`, while
   `LINK-04`, `LINK-05`, `LINK-06`, `LINK-10b` remain `HELD`. The committed run-3 record
   is accurate, not stale.

7. Current book is green today, which does not overturn the FAIL:
   `go test ./internal/site/...` PASS; `site -check` under both `/sqlite-internals/` and
   `/` PASS (`23 page(s), 1045 link(s) resolve`); `check paths` PASS. A search found no
   `id = '` in current chapter markdown, so the defect is latent on today's corpus. It is
   still blocking: the chunk's contract is that `-check` fails on broken links/anchors
   and does not fail on valid pages, and repeated single-quoted SQL such as
   `WHERE id = 'x'` is ordinary content for a SQLite book. The first future page with it
   breaks the Pages workflow, and broken anchors are already silently passable today
   wherever matching SQL text coexists. A deploy gate unsound in both directions cannot
   land on corpus luck.

## Required actions (builder checklist)

Fix `CheckSite`'s attribute extraction in `internal/site/check.go`. The repair is accepted
only when all of the following hold:

1. Extract `id`, `href`, and `src` only from actual HTML opening tags. Text nodes —
   prose, `<pre><code>` and inline `<code>` contents, `<script>`/`<style>` bodies, and
   escaped `&lt;...&gt;` examples — must never contribute an id, a duplicate, a link, or
   an anchor. HTML comments must continue to contribute nothing (LINK-05/LINK-06 stay
   HELD). Stripping only `<pre>` blocks is not sufficient (prose case C2 above).
2. Parse tags correctly: a `>` inside a quoted attribute value (e.g. `title="a>b"`) must
   not end the tag; allow whitespace around `=`; accept double-quoted and single-quoted
   values (LINK-04 behavior preserved). Tag/attribute names must be matched
   case-insensitively and unquoted values (`href=/b/x/`) must be read — both are valid
   HTML5 reachable through raw-HTML passthrough and are missed today; any real tag
   tokenizer provides them. (Implementation choice is the builder's: `go.mod` currently
   has goldmark only, so either justify adding an HTML tokenizer dependency or write a
   small quote-aware tag scanner. The guarantees above, not the library, are required.)
3. Keep duplicate-id detection and anchor resolution, but over real tag ids only:
   LINK-10b (real duplicate `id="1-overview"`) must still fail, and LINK-11b's `#alpha`
   must now fail with a missing-anchor problem.
4. Change nothing else observable: `CheckSite` signature, problem message strings, and
   link-resolution rules (site base, repeated-`/` handling, `?query` stripping, `.html`
   anchor fallback) stay as repaired. The real book must still report
   `23 page(s), 1045 link(s) resolve` under `-base /sqlite-internals/` and `-base /`,
   and rendered site output must be unchanged (checker-only change).
5. Add regression tests in `internal/site/site_test.go` at `CheckSite` level (hand-written
   HTML, in the style of `TestCheckSiteHTMLEdges`), covering at minimum:
   a. repeated `WHERE id = 'alpha'` inside `<pre><code>` → no duplicate-id problem;
   b. single-quoted `href='...'`/`src='...'` as escaped text inside `<pre><code>` → no
      broken-link problem;
   c. broken `#alpha` link with SQL `id = 'alpha'` text present → missing-anchor problem;
   d. the same single-quoted `id`/`href` text in a `<p>` prose node and in inline
      `<code>` → ignored (proves tag-scoping rather than `<pre>`-stripping);
   e. real single-quoted tag attributes still caught, with all existing
      `TestCheckSiteHTMLEdges` expectations unchanged (missing single-quoted link/image,
      comment ghost anchor, `/b//a/#nope`, `?q=1` anchor, real duplicate `dup`);
   f. `>` inside a quoted value before `href` still caught; uppercase `HREF`/`ID` and an
      unquoted `href` value caught (per action 2).
6. Re-run and record: `go test ./...`, `course site -check` (both bases),
   `course check paths`, and `bash agent_work/pub/adversarial/run-all.sh` with
   LINK-11a/LINK-11b flipped to HELD and no other attack regressing. Update any
   `build-notes.md` wording that describes document-wide regex scanning, if present.

## Anything else in REVIEW-2.md

No. Every other row is marked fixed with evidence, PATH-03 was an attack-assertion bug
(corrected and HELD in run 3), and the listed command gates pass; the arbiter re-ran the
site tests, both `site -check` bases, and `check paths` green. New Finding 1 is the only
open item, and it alone keeps this chunk from landing. No commit or push was made.

ARBITRATION-DONE
