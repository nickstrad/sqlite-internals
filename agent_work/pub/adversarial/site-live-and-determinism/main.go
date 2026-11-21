// Command site-live-and-determinism attacks the publishing chunk's end-to-end
// claims on the live repository and isolated scratch repositories: page and
// experiment reachability, offline self-containment, HTTP serving under
// multiple base paths, byte-for-byte determinism across runs/environments,
// dependency-free builds, -out directory safety guards, and workflow hygiene.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/internal/repocheck"
	"github.com/nickstrad/walfleet/internal/site"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ; ")
	return strings.TrimSpace(s)
}

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n",
		id, sanitize(tried), sanitize(expect), sanitize(got), verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "HARNESS ERROR: %s: %v\n", msg, err)
	os.Exit(2)
}

func runCmd(dir string, env []string, name string, args ...string) (int, string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), out.String(), nil
		}
		return -1, out.String(), err
	}
	return 0, out.String(), nil
}

func mustGit(dir string, args ...string) string {
	code, out, err := runCmd(dir, nil, "git", args...)
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("git %s (exit %d): %s", strings.Join(args, " "), code, strings.TrimSpace(out)), err)
	}
	return strings.TrimSpace(out)
}

func writeFile(p, content string) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fatal("mkdir for "+p, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fatal("write "+p, err)
	}
}

func initScratchRepo(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch repo", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch repo", err)
	}
	mustGit(dir, "init", "-q", "-b", "main")
	mustGit(dir, "config", "user.name", "Adv Test")
	mustGit(dir, "config", "user.email", "adv@example.invalid")
	writeFile(filepath.Join(dir, "go.mod"), "module "+sqlitex.ModulePath+"\n\ngo 1.26.0\n")
	writeFile(filepath.Join(dir, "README.md"), "# Scratch Book\n\nWelcome to [Chapter 1](ch01-demo/ch01-demo.md).\n")
	writeFile(filepath.Join(dir, "ch01-demo/ch01-demo.md"), "# Chapter 1 — Demo\n\n## 1. Model\n\n## 2. Lab\n\n### Experiment 1.1 — Demo (`1.1-demo`)\n\nBody.\n")
	writeFile(filepath.Join(dir, "ch01-demo/experiments/1.1-demo/main.go"), "package main\n")
	mustGit(dir, "add", "-A")
	mustGit(dir, "commit", "-q", "-m", "init")
}

// hashTree returns a deterministic sorted list of "relpath sha256" lines for
// every regular file under dir.
func hashTree(dir string) (string, int, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		entries = append(entries, fmt.Sprintf("%s %s", filepath.ToSlash(rel), hex.EncodeToString(sum[:])))
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n"), len(entries), nil
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-pub", "site-live-and-determinism")
	if err := os.RemoveAll(workDir); err != nil {
		fatal("clean workDir", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir workDir", err)
	}

	// Build the real cmd/course binary once so attacks test the exact CLI.
	courseBin := filepath.Join(workDir, "course")
	code, out, err := runCmd(repoRoot, nil, "go", "build", "-buildvcs=false", "-o", courseBin, "./cmd/course")
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("build cmd/course (exit %d): %s", code, out), err)
	}

	siteDir := filepath.Join(workDir, "live-site")
	code, out, err = runCmd(repoRoot, nil, courseBin, "site", "-out", siteDir, "-base", "/sqlite-internals/")
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("initial live build failed (exit %d): %s", code, out), err)
	}

	attackLiveReachability(repoRoot, siteDir)
	attackLiveOfflineAndChrome(siteDir)
	attackLiveHTTPBases(repoRoot, courseBin, workDir)
	attackLiveDeterminism(repoRoot, courseBin, workDir)
	attackDependencyFreeBuild(repoRoot, courseBin, workDir)
	attackOutDirSafety(courseBin, workDir)
	attackWorkflowSpec(repoRoot, courseBin)
}

func attackLiveReachability(repoRoot, siteDir string) {
	homeBytes, err := os.ReadFile(filepath.Join(siteDir, "index.html"))
	if err != nil {
		fatal("read home index.html", err)
	}
	home := string(homeBytes)

	chapters, err := repocheck.Chapters(repoRoot, "")
	if err != nil {
		fatal("list chapters", err)
	}

	var issues []string
	// Verify required static artifacts exist.
	for _, req := range []string{".nojekyll", "style.css", "index.html", "outline/index.html"} {
		if _, err := os.Stat(filepath.Join(siteDir, filepath.FromSlash(req))); err != nil {
			issues = append(issues, "missing "+req)
		}
	}

	readingPath := ""
	if rpStart := strings.Index(home, "Reading path</h2>"); rpStart >= 0 {
		readingPath = home[rpStart:]
		if rpEnd := strings.Index(readingPath, "</section>"); rpEnd >= 0 {
			readingPath = readingPath[:rpEnd]
		}
	} else {
		issues = append(issues, "home missing Reading path section")
	}

	// Expected rungs for currently landed chapters from OUTLINE.md §3 & §5.
	wantRungs := map[string]string{
		"ch01-architecture-and-execution-path": "Rung R0 (Recipe)",
		"ch02-file-format-and-pages":           "Rung R1 (Pages)",
		"ch04-pager-and-page-cache":            "",
		"ch05-wal-transactions-checkpoints":    "Rung R3 (Redo log)",
		"ch07-vfs-and-filesystem-io":           "",
	}

	totalExps := 0
	for _, ch := range chapters {
		chPagePath := filepath.Join(siteDir, ch.Dir, "index.html")
		chBytes, err := os.ReadFile(chPagePath)
		if err != nil {
			issues = append(issues, "missing chapter page "+ch.Dir)
			continue
		}
		chHTML := string(chBytes)
		if _, err := os.Stat(filepath.Join(ch.Path, "answers.md")); err == nil {
			if _, err := os.Stat(filepath.Join(siteDir, ch.Dir, "answers", "index.html")); err != nil {
				issues = append(issues, "missing answers page for "+ch.Dir)
			}
			if !strings.Contains(chHTML, `/sqlite-internals/`+ch.Dir+`/answers/`) {
				issues = append(issues, ch.Dir+" missing link to answers page")
			}
		}

		// Verify chapter appears in home reading path.
		chHref := `href="/sqlite-internals/` + ch.Dir + `/"`
		if !strings.Contains(readingPath, chHref) {
			issues = append(issues, "home missing link to "+ch.Dir)
		}

		// Check rung attribution in home reading path slice for this chapter.
		if wantRung, known := wantRungs[ch.Dir]; known {
			idx := strings.Index(readingPath, chHref)
			if idx >= 0 {
				endIdx := strings.Index(readingPath[idx:], "</li>")
				// Slice up to the nested <ul> or </li> for this chapter's heading line.
				ulIdx := strings.Index(readingPath[idx:], "<ul>")
				if ulIdx >= 0 && (endIdx < 0 || ulIdx < endIdx) {
					endIdx = ulIdx
				}
				snippet := readingPath[idx:]
				if endIdx > 0 {
					snippet = readingPath[idx : idx+endIdx]
				}
				if wantRung == "" {
					if strings.Contains(snippet, `class="rung"`) {
						issues = append(issues, ch.Dir+" unexpectedly got rung span: "+snippet)
					}
				} else if !strings.Contains(snippet, wantRung) {
					issues = append(issues, fmt.Sprintf("%s missing %q in reading path", ch.Dir, wantRung))
				}
			}
		}

		// Verify every experiment directory has a reading-path anchor link AND Code/Evidence links on the chapter page.
		expEntries, err := os.ReadDir(filepath.Join(ch.Path, "experiments"))
		if err != nil {
			issues = append(issues, "cannot read experiments dir for "+ch.Dir)
			continue
		}
		for _, e := range expEntries {
			if !e.IsDir() {
				continue
			}
			totalExps++
			expName := e.Name()
			expID, _, _ := strings.Cut(expName, "-")
			// Reading path must link to an anchor (#...) on the chapter page, not bare chapter root,
			// with link text naming the experiment id ("Experiment N.M") and a matching id on the chapter page.
			anchorRe := regexp.MustCompile(`href="/sqlite-internals/` + regexp.QuoteMeta(ch.Dir) + `/#([^"]+)"[^>]*>[^<]*Experiment ` + regexp.QuoteMeta(expID) + `\b`)
			m := anchorRe.FindStringSubmatch(readingPath)
			if m == nil {
				issues = append(issues, "home reading path missing anchored link for "+expName)
			} else if !strings.Contains(chHTML, `id="`+m[1]+`"`) {
				issues = append(issues, ch.Dir+" missing anchor id "+m[1]+" for "+expName)
			}
			codeURL := "https://github.com/nickstrad/walfleet/blob/main/" + ch.Dir + "/experiments/" + expName + "/main.go"
			evURL := "https://github.com/nickstrad/walfleet/blob/main/" + ch.Dir + "/evidence/" + expName + ".txt"
			if !strings.Contains(chHTML, `class="experiment-links"><a href="`+codeURL+`">Code</a>`) {
				issues = append(issues, ch.Dir+" missing Code link for "+expName)
			}
			if _, err := os.Stat(filepath.Join(ch.Path, "evidence", expName+".txt")); err == nil {
				if !strings.Contains(chHTML, `<a href="`+evURL+`">Evidence</a>`) {
					issues = append(issues, ch.Dir+" missing Evidence link for "+expName)
				}
			}
		}
	}

	// Verify every docs/knowledge/*.md is rendered and linked from Reference.
	knowledgeFiles, _ := filepath.Glob(filepath.Join(repoRoot, "docs", "knowledge", "*.md"))
	for _, kf := range knowledgeFiles {
		stem := strings.TrimSuffix(filepath.Base(kf), ".md")
		if _, err := os.Stat(filepath.Join(siteDir, "knowledge", stem, "index.html")); err != nil {
			issues = append(issues, "missing knowledge page "+stem)
		}
		if !strings.Contains(home, `href="/sqlite-internals/knowledge/`+stem+`/"`) {
			issues = append(issues, "home Reference missing "+stem)
		}
	}

	held := len(issues) == 0
	got := fmt.Sprintf("%d chapters, %d experiments, %d knowledge pages verified, 0 issues", len(chapters), totalExps, len(knowledgeFiles))
	if !held {
		got = fmt.Sprintf("%d issue(s): %s", len(issues), strings.Join(issues, "; "))
	}
	report("LIVE-01",
		"Build real repo under /sqlite-internals/ and verify every chapter, answers, outline, knowledge page, rung, and experiment anchor + Code/Evidence links",
		"all chapters, answers, knowledge pages, anchored experiments, Code/Evidence links, and rungs present",
		got, held)
}

func attackLiveOfflineAndChrome(siteDir string) {
	var issues []string
	pages := 0

	extAssetRe := regexp.MustCompile(`(?i)<(?:script\b|img\b[^>]*\bsrc=["']https?://|link\b[^>]*\bhref=["']https?://|iframe\b)`)
	cssExtRe := regexp.MustCompile(`(?i)(?:@import|url\(\s*["']?https?://)`)

	cssBytes, err := os.ReadFile(filepath.Join(siteDir, "style.css"))
	if err != nil {
		issues = append(issues, "missing style.css")
	} else {
		if cssExtRe.Match(cssBytes) {
			issues = append(issues, "style.css references external URL or @import")
		}
		if !bytes.Contains(cssBytes, []byte("prefers-color-scheme: dark")) {
			issues = append(issues, "style.css missing prefers-color-scheme: dark")
		}
	}

	err = filepath.WalkDir(siteDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		pages++
		rel, _ := filepath.Rel(siteDir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(data)
		if m := extAssetRe.FindString(s); m != "" {
			issues = append(issues, rel+" contains external asset/script: "+m)
		}
		if strings.Contains(s, "raw HTML omitted") {
			issues = append(issues, rel+" contains 'raw HTML omitted'")
		}
		if strings.Contains(s, "<!-- include:") {
			issues = append(issues, rel+" retains raw include marker")
		}
		if strings.Contains(s, "://fonts.googleapis.com") || strings.Contains(s, "cdnjs.cloudflare.com") {
			issues = append(issues, rel+" references CDN")
		}
		return nil
	})
	if err != nil {
		fatal("walk live site", err)
	}

	held := len(issues) == 0
	got := fmt.Sprintf("scanned %d HTML pages + style.css: 0 external assets, 0 scripts, 0 omitted HTML, 0 raw include markers, dark mode present", pages)
	if !held {
		got = fmt.Sprintf("%d issue(s): %s", len(issues), strings.Join(issues, "; "))
	}
	report("LIVE-02",
		"Scan all generated HTML and CSS of the real repo for external fonts/CDNs, scripts, omitted raw HTML, and unconverted include markers",
		"site is 100% offline self-contained with zero external assets, scripts, raw HTML omissions, or unconverted include markers",
		got, held)
}

func attackLiveHTTPBases(repoRoot, courseBin, workDir string) {
	bases := []struct {
		flagArg    string
		normalized string
	}{
		{"/sqlite-internals/", "/sqlite-internals/"},
		{"/", "/"},
		{"/org/nested/book/", "/org/nested/book/"},
		{"sqlite-internals", "/sqlite-internals/"},
	}

	hrefRe := regexp.MustCompile(`\s(?:href|src)="([^"]*)"`)
	idRe := regexp.MustCompile(`\sid="([^"]*)"`)
	schemeRe := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

	var issues []string
	totalFetched := 0
	totalAnchors := 0

	for i, tc := range bases {
		outDir := filepath.Join(workDir, fmt.Sprintf("base-%d", i))
		// 1. Run -check with this base.
		code, out, err := runCmd(repoRoot, nil, courseBin, "site", "-check", "-base", tc.flagArg)
		if err != nil || code != 0 {
			issues = append(issues, fmt.Sprintf("site -check -base %q failed (exit %d): %s", tc.flagArg, code, strings.TrimSpace(out)))
			continue
		}
		// 2. Build into outDir.
		code, out, err = runCmd(repoRoot, nil, courseBin, "site", "-out", outDir, "-base", tc.flagArg)
		if err != nil || code != 0 {
			issues = append(issues, fmt.Sprintf("site -out -base %q failed (exit %d): %s", tc.flagArg, code, strings.TrimSpace(out)))
			continue
		}
		// 3. Serve over HTTP with StripPrefix matching GitHub Pages semantics.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fatal("listen", err)
		}
		mux := http.NewServeMux()
		prefix := strings.TrimSuffix(tc.normalized, "/")
		if prefix == "" {
			mux.Handle("/", http.FileServer(http.Dir(outDir)))
		} else {
			mux.Handle(tc.normalized, http.StripPrefix(prefix, http.FileServer(http.Dir(outDir))))
		}
		srv := &http.Server{Handler: mux}
		go srv.Serve(ln)

		origin := "http://" + ln.Addr().String()
		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		}

		queue := []string{tc.normalized}
		seen := map[string]bool{}
		pageIDs := map[string]map[string]bool{}
		type anchorRef struct{ from, targetPath, frag string }
		var anchorRefs []anchorRef

		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			if seen[u] {
				continue
			}
			seen[u] = true
			resp, err := client.Get(origin + u)
			if err != nil {
				issues = append(issues, fmt.Sprintf("base %q GET %s error: %v", tc.flagArg, u, err))
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			totalFetched++
			if resp.StatusCode != http.StatusOK {
				issues = append(issues, fmt.Sprintf("base %q GET %s returned %d", tc.flagArg, u, resp.StatusCode))
				continue
			}
			if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				continue
			}
			ids := map[string]bool{}
			for _, m := range idRe.FindAllSubmatch(body, -1) {
				ids[html.UnescapeString(string(m[1]))] = true
			}
			pageIDs[u] = ids

			for _, m := range hrefRe.FindAllSubmatch(body, -1) {
				raw := html.UnescapeString(string(m[1]))
				if raw == "" {
					issues = append(issues, fmt.Sprintf("base %q %s has empty href/src", tc.flagArg, u))
					continue
				}
				if schemeRe.MatchString(raw) || strings.HasPrefix(raw, "//") {
					continue
				}
				pPart, frag, hasFrag := strings.Cut(raw, "#")
				target := u
				if pPart != "" {
					if strings.HasPrefix(pPart, "/") {
						if !strings.HasPrefix(pPart, tc.normalized) && pPart+"/" != tc.normalized {
							issues = append(issues, fmt.Sprintf("base %q %s link leaks outside base: %s", tc.flagArg, u, raw))
							continue
						}
						target = pPart
					} else {
						target = path.Join(path.Dir(u), pPart)
						if strings.HasSuffix(pPart, "/") {
							target += "/"
						}
					}
				}
				if !seen[target] {
					queue = append(queue, target)
				}
				if hasFrag && frag != "" {
					anchorRefs = append(anchorRefs, anchorRef{u, target, frag})
				}
			}
		}

		for _, ref := range anchorRefs {
			totalAnchors++
			ids, ok := pageIDs[ref.targetPath]
			if !ok {
				// Non-HTML asset or failed fetch (already reported if failed).
				continue
			}
			decodedFrag := ref.frag
			if uFrag, err := url.PathUnescape(ref.frag); err == nil {
				decodedFrag = uFrag
			}
			if !ids[ref.frag] && !ids[decodedFrag] {
				issues = append(issues, fmt.Sprintf("base %q %s -> %s#%s missing in fetched DOM", tc.flagArg, ref.from, ref.targetPath, ref.frag))
			}
		}

		srv.Close()
		ln.Close()
	}

	held := len(issues) == 0
	got := fmt.Sprintf("all %d base paths passed -check and HTTP crawl (%d URLs 200, %d internal anchors verified)", len(bases), totalFetched, totalAnchors)
	if !held {
		got = fmt.Sprintf("%d issue(s): %s", len(issues), strings.Join(issues, "; "))
	}
	report("LIVE-03",
		"Build, -check, and HTTP-crawl the real site under bases /sqlite-internals/, /, /org/nested/book/, and unnormalized sqlite-internals",
		"all bases pass -check, every internal URL returns HTTP 200 without leaking outside base, and every #anchor exists in fetched HTML",
		got, held)
}

func attackLiveDeterminism(repoRoot, courseBin, workDir string) {
	type runSpec struct {
		label string
		cwd   string
		env   []string
	}
	baseEnv := os.Environ()
	specs := []runSpec{
		{"utc-root", repoRoot, append(append([]string{}, baseEnv...), "TZ=UTC", "LC_ALL=C")},
		{"ny-root", repoRoot, append(append([]string{}, baseEnv...), "TZ=America/New_York", "LC_ALL=C")},
		{"tokyo-subcwd", filepath.Join(repoRoot, "cmd", "course"), append(append([]string{}, baseEnv...), "TZ=Asia/Tokyo")},
		{"parallel-1", repoRoot, append(append([]string{}, baseEnv...), "TZ=UTC")},
		{"parallel-2", repoRoot, append(append([]string{}, baseEnv...), "TZ=Europe/London")},
		{"parallel-3", repoRoot, append(append([]string{}, baseEnv...), "TZ=Pacific/Auckland")},
	}

	manifests := make([]string, len(specs))
	counts := make([]int, len(specs))
	errs := make([]string, len(specs))

	var wg sync.WaitGroup
	for i, sp := range specs {
		i, sp := i, sp
		outDir := filepath.Join(workDir, "det-"+sp.label)
		run := func() {
			code, out, err := runCmd(sp.cwd, sp.env, courseBin, "site", "-out", outDir, "-base", "/sqlite-internals/")
			if err != nil || code != 0 {
				errs[i] = fmt.Sprintf("%s exit %d: %s", sp.label, code, strings.TrimSpace(out))
				return
			}
			m, n, err := hashTree(outDir)
			if err != nil {
				errs[i] = fmt.Sprintf("%s hash error: %v", sp.label, err)
				return
			}
			manifests[i] = m
			counts[i] = n
		}
		if i < 3 {
			run()
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run()
			}()
		}
	}
	wg.Wait()

	var issues []string
	for _, e := range errs {
		if e != "" {
			issues = append(issues, e)
		}
	}
	if len(issues) == 0 {
		ref := manifests[0]
		for i := 1; i < len(manifests); i++ {
			if manifests[i] != ref {
				issues = append(issues, fmt.Sprintf("run %s differed from %s", specs[i].label, specs[0].label))
			}
		}
	}

	held := len(issues) == 0
	got := fmt.Sprintf("%d builds (%d files each) produced byte-for-byte identical trees across TZs, CWDs, and concurrency", len(specs), counts[0])
	if !held {
		got = strings.Join(issues, "; ")
	}
	report("LIVE-04",
		"Build real site 6 times across different TZs (UTC/NY/Tokyo/London/Auckland), subdirectory CWD, and parallel runs",
		"all 6 output trees are byte-for-byte identical (deterministic, no timestamps or map-order drift)",
		got, held)
}

func attackDependencyFreeBuild(repoRoot, courseBin, workDir string) {
	// Construct a restricted PATH containing symlinks ONLY to git and go (hiding sqlite3, strace, docker, gcc).
	gitPath, err := exec.LookPath("git")
	if err != nil {
		fatal("locate git", err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		fatal("locate go", err)
	}
	tinyBin := filepath.Join(workDir, "tiny-bin")
	if err := os.MkdirAll(tinyBin, 0o755); err != nil {
		fatal("mkdir tinyBin", err)
	}
	if err := os.Symlink(gitPath, filepath.Join(tinyBin, "git")); err != nil {
		fatal("symlink git", err)
	}
	if err := os.Symlink(goPath, filepath.Join(tinyBin, "go")); err != nil {
		fatal("symlink go", err)
	}

	env := []string{
		"PATH=" + tinyBin,
		"HOME=" + os.Getenv("HOME"),
		"GOPATH=" + os.Getenv("GOPATH"),
		"GOMODCACHE=" + os.Getenv("GOMODCACHE"),
		"GOCACHE=" + os.Getenv("GOCACHE"),
		"SQLITE_SRC=/nonexistent-sqlite-tree",
		"SQLITE3_BIN=/nonexistent-sqlite3",
	}

	outDir := filepath.Join(workDir, "nodep-site")
	code1, out1, err1 := runCmd(repoRoot, env, courseBin, "site", "-check", "-base", "/sqlite-internals/")
	code2, out2, err2 := runCmd(repoRoot, env, courseBin, "site", "-out", outDir, "-base", "/sqlite-internals/")

	held := err1 == nil && code1 == 0 && err2 == nil && code2 == 0
	got := fmt.Sprintf("-check exit=%d (%s); -out exit=%d (%s)",
		code1, strings.TrimSpace(out1), code2, strings.TrimSpace(out2))
	if err1 != nil {
		got += " err1=" + err1.Error()
	}
	if err2 != nil {
		got += " err2=" + err2.Error()
	}
	report("LIVE-05",
		"Run course site -check and -out with SQLITE_SRC=/nonexistent and PATH containing only git+go (no sqlite3/strace/docker/gcc)",
		"both commands succeed with exit 0",
		got, held)
}

func attackOutDirSafety(courseBin, workDir string) {
	scratch := filepath.Join(workDir, "out-safety-repo")
	initScratchRepo(scratch)

	// (a) -out . must be refused.
	cDot, outDot, _ := runCmd(scratch, nil, courseBin, "site", "-out", ".")
	// (b) -out .. (parent dir containing repo) must be refused.
	cParent, outParent, _ := runCmd(scratch, nil, courseBin, "site", "-out", "..")
	// (c) -out non-empty dir without .nojekyll must be refused and preserve sentinel.
	nonEmpty := filepath.Join(workDir, "non-empty-guard")
	sentinel := filepath.Join(nonEmpty, "keep.txt")
	writeFile(sentinel, "precious\n")
	cNonEmpty, outNonEmpty, _ := runCmd(scratch, nil, courseBin, "site", "-out", nonEmpty)
	_, sentinelErr := os.Stat(sentinel)
	// (d) -out pointing to a regular file must be refused and preserve file.
	regFile := filepath.Join(workDir, "regular-file.txt")
	writeFile(regFile, "keep-file\n")
	cReg, _, _ := runCmd(scratch, nil, courseBin, "site", "-out", regFile)
	regData, _ := os.ReadFile(regFile)
	// (e) -out pointing to previous build (.nojekyll present) must wipe stale files.
	prevSite := filepath.Join(workDir, "prev-site")
	writeFile(filepath.Join(prevSite, ".nojekyll"), "")
	writeFile(filepath.Join(prevSite, "stale.html"), "old")
	cPrev, _, _ := runCmd(scratch, nil, courseBin, "site", "-out", prevSite)
	_, staleErr := os.Stat(filepath.Join(prevSite, "stale.html"))
	_, freshErr := os.Stat(filepath.Join(prevSite, "index.html"))

	var coreIssues []string
	if cDot == 0 || !strings.Contains(outDot, "contains the repository") {
		coreIssues = append(coreIssues, fmt.Sprintf("-out . exit=%d out=%q", cDot, strings.TrimSpace(outDot)))
	}
	if cParent == 0 || !strings.Contains(outParent, "contains the repository") {
		coreIssues = append(coreIssues, fmt.Sprintf("-out .. exit=%d out=%q", cParent, strings.TrimSpace(outParent)))
	}
	if cNonEmpty == 0 || sentinelErr != nil || !strings.Contains(outNonEmpty, ".nojekyll") {
		coreIssues = append(coreIssues, fmt.Sprintf("-out nonEmpty exit=%d sentinelErr=%v", cNonEmpty, sentinelErr))
	}
	if cReg == 0 || string(regData) != "keep-file\n" {
		coreIssues = append(coreIssues, fmt.Sprintf("-out regFile exit=%d preserved=%v", cReg, string(regData) == "keep-file\n"))
	}
	if cPrev != 0 || !os.IsNotExist(staleErr) || freshErr != nil {
		coreIssues = append(coreIssues, fmt.Sprintf("-out prevSite exit=%d staleErr=%v freshErr=%v", cPrev, staleErr, freshErr))
	}
	report("LIVE-06a",
		"Test -out safety guards: '.', '..', non-empty dir without .nojekyll, regular file, and overwrite of previous .nojekyll build",
		"'.', '..', non-empty dir, and regular file refused without data loss; previous .nojekyll dir cleanly replaced with stale.html removed",
		func() string {
			if len(coreIssues) == 0 {
				return "all 5 core -out guard cases behaved safely"
			}
			return strings.Join(coreIssues, "; ")
		}(),
		len(coreIssues) == 0)

	// (f) Symlink containment bypass attack inside a disposable scratch repo:
	// create symlink -> parent of disposable repo, and put a .nojekyll inside
	// a nested scratch repo so we test whether prepareSiteDir resolves symlinks
	// before checking "contains the repository".
	symBase := filepath.Join(workDir, "sym-attack")
	symRepo := filepath.Join(symBase, "real-parent", "repo")
	initScratchRepo(symRepo)
	writeFile(filepath.Join(symRepo, ".nojekyll"), "")
	mustGit(symRepo, "add", ".nojekyll")
	mustGit(symRepo, "commit", "-q", "-m", "add nojekyll")

	symParent := filepath.Join(symBase, "link-to-parent")
	if err := os.Symlink(filepath.Join(symBase, "real-parent"), symParent); err != nil {
		fatal("symlink parent", err)
	}
	// Passing <link-to-parent>/repo points to the EXACT repo root via a symlinked parent!
	symRepoOut := filepath.Join(symParent, "repo")
	cSym, outSym, _ := runCmd(symRepo, nil, courseBin, "site", "-out", symRepoOut)
	_, gitDirErr := os.Stat(filepath.Join(symRepo, ".git"))

	heldSym := cSym != 0 && gitDirErr == nil && strings.Contains(outSym, "contains the repository")
	gotSym := fmt.Sprintf("exit=%d, repo .git intact=%v, output=%q", cSym, gitDirErr == nil, strings.TrimSpace(outSym))
	report("LIVE-06b",
		"Pass -out <symlink-to-parent>/repo pointing to a disposable scratch repo that contains .nojekyll",
		"prepareSiteDir resolves symlinks and refuses because -out is the repository itself",
		gotSym, heldSym)
}

func attackWorkflowSpec(repoRoot, courseBin string) {
	wfPath := filepath.Join(repoRoot, ".github", "workflows", "pages.yml")
	raw, err := os.ReadFile(wfPath)
	if err != nil {
		fatal("read pages.yml", err)
	}
	wf := string(raw)

	required := []string{
		"branches: [main]",
		"workflow_dispatch:",
		"contents: read",
		"pages: write",
		"id-token: write",
		"group: pages",
		"cancel-in-progress: false",
		"actions/checkout@",
		"actions/setup-go@",
		"go-version-file: go.mod",
		"actions/configure-pages@",
		"actions/upload-pages-artifact@",
		"include-hidden-files: true",
		"actions/deploy-pages@",
		"name: github-pages",
		"go run ./cmd/course site -check -base",
		"go run ./cmd/course site -out _site -base",
	}
	var issues []string
	for _, needle := range required {
		if !strings.Contains(wf, needle) {
			issues = append(issues, "missing "+needle)
		}
	}
	// Verify configure-pages appears BEFORE site -check and site -out.
	idxCfg := strings.Index(wf, "actions/configure-pages@")
	idxChk := strings.Index(wf, "site -check")
	idxBld := strings.Index(wf, "site -out _site")
	if !(idxCfg >= 0 && idxChk > idxCfg && idxBld > idxChk) {
		issues = append(issues, "step ordering wrong (configure-pages -> site -check -> site -out)")
	}
	// Verify what workflow's "${{ steps.pages.outputs.base_path }}/" produces when base_path is "" (custom domain),
	// "/sqlite-internals" (project pages), or "/sqlite-internals/" (already trailing slash).
	for _, basePathOut := range []string{"", "/sqlite-internals", "/sqlite-internals/"} {
		arg := basePathOut + "/"
		gotNorm := site.NormalizeBase(arg)
		wantNorm := "/"
		if basePathOut != "" {
			wantNorm = "/sqlite-internals/"
		}
		if gotNorm != wantNorm {
			issues = append(issues, fmt.Sprintf("NormalizeBase(%q)=%q want %q", arg, gotNorm, wantNorm))
		}
	}
	// Actually execute course site -check with empty base_path -> "/" and double slash "//sqlite-internals//".
	for _, arg := range []string{"/", "/sqlite-internals//"} {
		c, out, err := runCmd(repoRoot, nil, courseBin, "site", "-check", "-base", arg)
		if err != nil || c != 0 {
			issues = append(issues, fmt.Sprintf("site -check -base %q failed: %s", arg, strings.TrimSpace(out)))
		}
	}

	held := len(issues) == 0
	got := "pages.yml contains all required triggers, permissions, concurrency, actions, step order, and base_path normalization holds for empty/single/double slashes"
	if !held {
		got = strings.Join(issues, "; ")
	}
	report("LIVE-07",
		"Inspect .github/workflows/pages.yml structure and simulate configure-pages base_path outputs ('', '/sqlite-internals', '/sqlite-internals/')",
		"workflow meets all publishing spec requirements and handles custom-domain and project-pages base_path outputs cleanly",
		got, held)
}
