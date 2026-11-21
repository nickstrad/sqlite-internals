package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/nickstrad/sqlite-internals/internal/capture"
	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	recipeStep   = 12
	sweepSamples = 500
)

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

type probeVerdict struct {
	integrityCaught bool
	quickCaught     bool
	logicalCaught   bool
	manifestCaught  bool
	manifestPages   []int
	integrityMsg    string
	quickMsg        string
}

// runPragmaCheck opens path read-only/immutable via raw sql.Open (so even corrupt
// headers/structures can be tested without sqlitex's open-time PRAGMA validation
// masking what PRAGMA integrity_check/quick_check actually sees) and runs the pragma.
func runPragmaCheck(ctx context.Context, path, pragma string) (caught bool, msg string) {
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {"ro"}, "immutable": {"1"}})
	if err != nil {
		return true, err.Error()
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return true, err.Error()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.QueryContext(ctx, pragma)
	if err != nil {
		return true, err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return true, err.Error()
		}
		lines = append(lines, s)
	}
	if err := rows.Err(); err != nil {
		return true, err.Error()
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return false, "ok"
	}
	if len(lines) == 0 {
		return true, "empty"
	}
	return true, lines[0]
}

// evaluateCorrupt writes image to path, runs integrity_check, quick_check, E3 logical
// hash, and capture.VerifyImage against original manifest & logical baseline.
func evaluateCorrupt(ctx context.Context, path string, image []byte, origManifest equal.Manifest, origLogical equal.Logical) probeVerdict {
	if err := os.WriteFile(path, image, 0o644); err != nil {
		fatal("write corrupt copy", err)
	}
	defer os.Remove(path)

	var v probeVerdict
	v.integrityCaught, v.integrityMsg = runPragmaCheck(ctx, path, "PRAGMA integrity_check")
	v.quickCaught, v.quickMsg = runPragmaCheck(ctx, path, "PRAGMA quick_check")

	gotLogical, err := equal.LogicalHashFile(ctx, path)
	if err != nil {
		v.logicalCaught = true
	} else {
		diff := equal.CompareLogical(origLogical, gotLogical)
		v.logicalCaught = !diff.Equal
	}

	diffPages, err := capture.VerifyImage(origManifest, image)
	if err != nil {
		v.manifestCaught = true
	} else {
		v.manifestCaught = len(diffPages) > 0
		v.manifestPages = diffPages
	}
	return v
}

// attackINT01 runs a seeded 500-offset uniform random 1-bit flip sweep across the
// entire step-12 database and reports detection rates overall and per census role.
func attackINT01(ctx context.Context, original []byte, census *dbfile.Census, manifest equal.Manifest, origLogical equal.Logical, work string) {
	dir := filepath.Join(work, "int01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("int01 mkdir", err)
	}
	defer os.RemoveAll(dir)

	rng := rand.New(rand.NewPCG(90301, 20261121))
	seen := make(map[int]bool, sweepSamples)
	offsets := make([]int, 0, sweepSamples)
	for len(offsets) < sweepSamples {
		off := rng.IntN(len(original))
		if !seen[off] {
			seen[off] = true
			offsets = append(offsets, off)
		}
	}

	type roleStats struct {
		total, integ, quick, logical, manifest int
	}
	byRole := map[string]*roleStats{}
	var mu sync.Mutex
	var (
		totInteg, totQuick, totLogical, totManifest, exactPage int
		quickExceedsInteg                                      int
	)

	pageSize := census.PageSize
	ch := make(chan int, len(offsets))
	for i := range offsets {
		ch <- i
	}
	close(ch)

	workers := 4
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			buf := make([]byte, len(original))
			p := filepath.Join(dir, fmt.Sprintf("w%d.db", wid))
			for idx := range ch {
				off := offsets[idx]
				copy(buf, original)
				buf[off] ^= 0x01
				pgno := off/pageSize + 1
				role := census.Page(pgno).Role.String()
				if off < 100 {
					role = "db-header"
				}

				v := evaluateCorrupt(ctx, p, buf, manifest, origLogical)

				mu.Lock()
				st := byRole[role]
				if st == nil {
					st = &roleStats{}
					byRole[role] = st
				}
				st.total++
				if v.integrityCaught {
					totInteg++
					st.integ++
				}
				if v.quickCaught {
					totQuick++
					st.quick++
				}
				if v.quickCaught && !v.integrityCaught {
					quickExceedsInteg++
				}
				if v.logicalCaught {
					totLogical++
					st.logical++
				}
				if v.manifestCaught {
					totManifest++
					st.manifest++
				}
				if slices.Equal(v.manifestPages, []int{pgno}) {
					exactPage++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	// Print compact per-region breakdown as INFO lines for chapter prose and review.
	roles := make([]string, 0, len(byRole))
	for k := range byRole {
		roles = append(roles, k)
	}
	sort.Strings(roles)
	for _, r := range roles {
		st := byRole[r]
		fmt.Printf("INFO INT-01 role=%-15s n=%3d | integrity=%3d (%.1f%%) quick=%3d (%.1f%%) E3=%3d (%.1f%%) manifest=%3d (%.1f%%)\n",
			r, st.total,
			st.integ, 100*float64(st.integ)/float64(st.total),
			st.quick, 100*float64(st.quick)/float64(st.total),
			st.logical, 100*float64(st.logical)/float64(st.total),
			st.manifest, 100*float64(st.manifest)/float64(st.total))
	}

	held := totManifest == sweepSamples &&
		exactPage == sweepSamples &&
		totInteg < sweepSamples &&
		totQuick <= totInteg &&
		quickExceedsInteg == 0 &&
		totLogical > totInteg &&
		totLogical < sweepSamples

	got := fmt.Sprintf("integrity=%d/%d (%.1f%%) quick=%d/%d (%.1f%%) E3=%d/%d (%.1f%%) manifest=%d/%d (%.1f%%, exactPage=%d/%d)",
		totInteg, sweepSamples, 100*float64(totInteg)/float64(sweepSamples),
		totQuick, sweepSamples, 100*float64(totQuick)/float64(sweepSamples),
		totLogical, sweepSamples, 100*float64(totLogical)/float64(sweepSamples),
		totManifest, sweepSamples, 100*float64(totManifest)/float64(sweepSamples),
		exactPage, sweepSamples)
	report("INT-01", fmt.Sprintf("seeded sweep of %d uniform random 1-bit flips across step-12 DB (%d bytes)", sweepSamples, len(original)),
		"manifest catches 100% (exact page); integrity_check & quick_check miss content/unused bytes; quick<=integrity<E3<manifest", got, held)
}

// attackINT02 probes four specific blind-spot regions beyond 9.3's 4 sites:
// overflow payload, unallocated B-tree page free space, freelist trunk unused tail,
// and database header reserved bytes.
func attackINT02(ctx context.Context, f *dbfile.File, original []byte, census *dbfile.Census, manifest equal.Manifest, origLogical equal.Logical, work string) {
	dir := filepath.Join(work, "int02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("int02 mkdir", err)
	}
	defer os.RemoveAll(dir)

	pageSize := census.PageSize

	// 2a: Overflow page payload (byte 100 inside first RoleOverflow page).
	ovPage := -1
	for _, info := range census.Pages {
		if info.Role == dbfile.RoleOverflow {
			ovPage = info.Number
			break
		}
	}
	if ovPage < 0 {
		fatal("int02a", fmt.Errorf("no overflow page found in step-%d DB", recipeStep))
	}
	imgA := append([]byte(nil), original...)
	imgA[(ovPage-1)*pageSize+100] ^= 0x01
	vA := evaluateCorrupt(ctx, filepath.Join(dir, "ov.db"), imgA, manifest, origLogical)
	heldA := !vA.integrityCaught && !vA.quickCaught && vA.logicalCaught && slices.Equal(vA.manifestPages, []int{ovPage})
	report("INT-02a", fmt.Sprintf("1-bit flip in overflow page payload (page %d, byte 100)", ovPage),
		"integrity_check=ok, quick_check=ok, E3 catches, manifest names exact overflow page",
		fmt.Sprintf("integrity=%q quick=%q E3_caught=%t manifest=%v", vA.integrityMsg, vA.quickMsg, vA.logicalCaught, vA.manifestPages), heldA)

	// 2b: Unallocated free space inside a live B-tree table leaf page (between end of cell pointer array and CellContentStart).
	freePage, freeOff := -1, -1
	for _, info := range census.Pages {
		if info.Role == dbfile.RoleTableLeaf && info.Number > 1 {
			p, err := f.BTreePage(info.Number)
			if err != nil {
				continue
			}
			ptrEnd := p.HeaderOffset + 8 + 2*p.CellCount
			if p.CellContentStart-ptrEnd >= 16 {
				freePage = info.Number
				freeOff = ptrEnd + (p.CellContentStart-ptrEnd)/2
				break
			}
		}
	}
	if freePage < 0 {
		fatal("int02b", fmt.Errorf("no table leaf with unallocated gap found"))
	}
	imgB := append([]byte(nil), original...)
	imgB[(freePage-1)*pageSize+freeOff] ^= 0xff
	vB := evaluateCorrupt(ctx, filepath.Join(dir, "free.db"), imgB, manifest, origLogical)
	heldB := !vB.integrityCaught && !vB.quickCaught && !vB.logicalCaught && slices.Equal(vB.manifestPages, []int{freePage})
	report("INT-02b", fmt.Sprintf("1-byte flip in unallocated free space of live table leaf (page %d, byte %d)", freePage, freeOff),
		"integrity_check=ok, quick_check=ok, E3=equal, ONLY page manifest catches it",
		fmt.Sprintf("integrity=%q quick=%q E3_caught=%t manifest=%v", vB.integrityMsg, vB.quickMsg, vB.logicalCaught, vB.manifestPages), heldB)

	// 2c: Freelist trunk unused tail (bytes past 8 + 4*leafCount on first freelist trunk).
	if len(census.FreelistTrunks) == 0 {
		fatal("int02c", fmt.Errorf("no freelist trunk found"))
	}
	trunkPage := census.FreelistTrunks[0]
	trunkOff := (trunkPage - 1) * pageSize
	leafCount := int(binary.BigEndian.Uint32(original[trunkOff+4 : trunkOff+8]))
	tailPos := 8 + 4*leafCount + 64
	if tailPos >= pageSize {
		tailPos = pageSize - 1
	}
	imgC := append([]byte(nil), original...)
	imgC[trunkOff+tailPos] ^= 0x55
	vC := evaluateCorrupt(ctx, filepath.Join(dir, "trunk.db"), imgC, manifest, origLogical)
	heldC := !vC.integrityCaught && !vC.quickCaught && !vC.logicalCaught && slices.Equal(vC.manifestPages, []int{trunkPage})
	report("INT-02c", fmt.Sprintf("1-byte flip in freelist trunk unused tail (page %d, leafCount=%d, byte %d)", trunkPage, leafCount, tailPos),
		"integrity_check=ok, quick_check=ok, E3=equal, ONLY page manifest catches it",
		fmt.Sprintf("integrity=%q quick=%q E3_caught=%t manifest=%v", vC.integrityMsg, vC.quickMsg, vC.logicalCaught, vC.manifestPages), heldC)

	// 2d: Database header reserved bytes (page 1, byte 80 inside reserved zero span 72..91).
	imgD := append([]byte(nil), original...)
	imgD[80] ^= 0x01
	vD := evaluateCorrupt(ctx, filepath.Join(dir, "hdr80.db"), imgD, manifest, origLogical)
	heldD := !vD.integrityCaught && !vD.quickCaught && !vD.logicalCaught && slices.Equal(vD.manifestPages, []int{1})
	report("INT-02d", "1-bit flip in database header reserved zero region (page 1, byte 80)",
		"integrity_check=ok, quick_check=ok, E3=equal, ONLY page manifest catches it",
		fmt.Sprintf("integrity=%q quick=%q E3_caught=%t manifest=%v", vD.integrityMsg, vD.quickMsg, vD.logicalCaught, vD.manifestPages), heldD)
}

// attackINT03 flips an indexed column (events.account_id) in BOTH the table leaf
// and its matching index leaf entry (choosing a boundary entry so sort order is
// preserved), proving integrity_check passes even on a corrupted indexed column
// when table and index agree.
func attackINT03(ctx context.Context, f *dbfile.File, original []byte, census *dbfile.Census, manifest equal.Manifest, origLogical equal.Logical, work string) {
	dir := filepath.Join(work, "int03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("int03 mkdir", err)
	}
	defer os.RemoveAll(dir)

	pageSize := census.PageSize

	// Find a leaf of index events_by_account where two adjacent cells have
	// account_id A and A+1 (both 1-byte signed integers), so bumping the first
	// entry's account_id from A to A+1 preserves index sort order!
	type idxTarget struct {
		idxPage   int
		idxByte   int
		rowid     int64
		oldAcct   byte
		newAcct   byte
		tablePage int
		tableByte int
	}
	var found *idxTarget

	for _, info := range census.Pages {
		if info.Tree != "events_by_account" || info.Role != dbfile.RoleIndexLeaf {
			continue
		}
		p, err := f.BTreePage(info.Number)
		if err != nil {
			continue
		}
		for c := 0; c < p.CellCount-1; c++ {
			cell0, err0 := p.Cell(c)
			cell1, err1 := p.Cell(c + 1)
			if err0 != nil || err1 != nil || cell0.Overflows() || cell1.Overflows() {
				continue
			}
			v0, err0 := dbfile.DecodeRecord(cell0.Local)
			v1, err1 := dbfile.DecodeRecord(cell1.Local)
			if err0 != nil || err1 != nil || len(v0) < 2 || len(v1) < 2 {
				continue
			}
			// Index entry is (account_id, rowid); ensure A >= 2 and next is A+1 so
			// bumping A -> A+1 stays <= next (and if rowid0 < rowid1, strictly ordered!).
			if v0[0].Class == dbfile.Integer && v1[0].Class == dbfile.Integer &&
				v0[1].Class == dbfile.Integer && v1[1].Class == dbfile.Integer &&
				v0[0].Int >= 2 && v0[0].Int < 120 &&
				v1[0].Int == v0[0].Int+1 && v0[1].Int < v1[1].Int {
				colStart, colLen, _, err := dbfile.RecordColumn(cell0.Local, 0)
				if err != nil || colLen != 1 {
					continue
				}
				idxByte := (p.Number-1)*pageSize + cell0.Offset + dbfile.VarintLength(cell0.PayloadSize) + colStart
				found = &idxTarget{
					idxPage: p.Number,
					idxByte: idxByte,
					rowid:   v0[1].Int,
					oldAcct: byte(v0[0].Int),
					newAcct: byte(v0[0].Int + 1),
				}
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		fatal("int03", fmt.Errorf("could not locate an order-preserving index boundary entry"))
	}

	// Now locate the matching row in the events table leaves.
	for _, info := range census.Pages {
		if info.Tree != "events" || info.Role != dbfile.RoleTableLeaf {
			continue
		}
		p, err := f.BTreePage(info.Number)
		if err != nil {
			continue
		}
		for c := 0; c < p.CellCount; c++ {
			cell, err := p.Cell(c)
			if err != nil || cell.Overflows() || cell.Rowid != found.rowid {
				continue
			}
			colStart, colLen, _, err := dbfile.RecordColumn(cell.Local, 1)
			if err != nil || colLen != 1 {
				continue
			}
			found.tablePage = p.Number
			found.tableByte = (p.Number-1)*pageSize + cell.Offset + dbfile.VarintLength(cell.PayloadSize) + dbfile.VarintLength(uint64(cell.Rowid)) + colStart
			break
		}
		if found.tablePage > 0 {
			break
		}
	}
	if found.tablePage == 0 {
		fatal("int03", fmt.Errorf("could not locate table row %d", found.rowid))
	}

	img := append([]byte(nil), original...)
	img[found.idxByte] = found.newAcct
	img[found.tableByte] = found.newAcct

	v := evaluateCorrupt(ctx, filepath.Join(dir, "coord.db"), img, manifest, origLogical)
	wantPages := []int{found.idxPage, found.tablePage}
	sort.Ints(wantPages)
	if wantPages[0] == wantPages[1] {
		wantPages = wantPages[:1]
	}

	held := !v.integrityCaught && !v.quickCaught && v.logicalCaught && slices.Equal(v.manifestPages, wantPages)
	report("INT-03", fmt.Sprintf("coordinated account_id %d->%d flip on row %d in BOTH table (p.%d) and index (p.%d)", found.oldAcct, found.newAcct, found.rowid, found.tablePage, found.idxPage),
		"integrity_check=ok and quick_check=ok (structure & index consistent); E3 & manifest catch both pages",
		fmt.Sprintf("integrity=%q quick=%q E3_caught=%t manifest=%v", v.integrityMsg, v.quickMsg, v.logicalCaught, v.manifestPages), held)
}

// attackINT04 runs must-be-able-to-fail boundary tests on dbfile.RecordColumn and
// capture.VerifyImage.
func attackINT04(manifest equal.Manifest, original []byte) {
	// 1. dbfile.RecordColumn invalid inputs must all return non-nil errors without panicking.
	badRecords := []struct {
		name    string
		payload []byte
		col     int
	}{
		{"empty", nil, 0},
		{"truncated-varint", []byte{0x80}, 0},
		{"hdr-too-small", []byte{0x00, 0x01}, 0},
		{"hdr-past-end", []byte{0x05, 0x01, 0x01}, 0},
		{"reserved-st-10", []byte{0x02, 10}, 0},
		{"reserved-st-11", []byte{0x02, 11}, 0},
		{"negative-col", []byte{0x02, 0x01, 0x42}, -1},
		{"col-past-end", []byte{0x02, 0x01, 0x42}, 1},
		{"body-truncated", []byte{0x02, 0x04, 0x01, 0x02}, 0}, // serial type 4 needs 4 bytes, only 2 present
	}
	recOK := 0
	for _, tc := range badRecords {
		if _, _, _, err := dbfile.RecordColumn(tc.payload, tc.col); err != nil {
			recOK++
		}
	}
	// Valid zero-length serial types (0=NULL, 8=int 0, 9=int 1) followed by 1-byte int.
	validPayload := []byte{0x05, 0, 8, 9, 1, 0x7f}
	s0, l0, t0, e0 := dbfile.RecordColumn(validPayload, 0)
	s3, l3, t3, e3 := dbfile.RecordColumn(validPayload, 3)
	validOK := e0 == nil && s0 == 5 && l0 == 0 && t0 == 0 && e3 == nil && s3 == 5 && l3 == 1 && t3 == 1

	// 2. capture.VerifyImage boundary & multi-page detection checks.
	badPS := manifest
	badPS.PageSize = 0
	_, errBadPS := capture.VerifyImage(badPS, original)

	badSeq := manifest
	badSeq.Entries = append([]equal.ManifestEntry(nil), manifest.Entries...)
	badSeq.Entries[1].Page = 999
	_, errBadSeq := capture.VerifyImage(badSeq, original)

	_, errShort := capture.VerifyImage(manifest, original[:len(original)-1])
	_, errLong := capture.VerifyImage(manifest, append(append([]byte(nil), original...), 0))

	multi := append([]byte(nil), original...)
	multi[0] ^= 0x01
	multi[5*manifest.PageSize+17] ^= 0x80
	multi[117*manifest.PageSize+4095] ^= 0x04
	multiDiff, errMulti := capture.VerifyImage(manifest, multi)
	multiOK := errMulti == nil && slices.Equal(multiDiff, []int{1, 6, 118})

	held := recOK == len(badRecords) && validOK && errBadPS != nil && errBadSeq != nil && errShort != nil && errLong != nil && multiOK
	got := fmt.Sprintf("RecordColumn bad=%d/%d validZeroLen=%t; VerifyImage badPS=%t badSeq=%t short=%t long=%t multiDiff=%v",
		recOK, len(badRecords), validOK, errBadPS != nil, errBadSeq != nil, errShort != nil, errLong != nil, multiDiff)
	report("INT-04", "must-fail boundary suite on dbfile.RecordColumn and capture.VerifyImage",
		"every invalid record/manifest/image is rejected without panic and multi-page corruption lists all differing pages", got, held)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	work := filepath.Join(root, "_work", "adv-ch09", "integrity-sweep")
	if err := os.RemoveAll(work); err != nil {
		fatal("clean work", err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		fatal("mkdir work", err)
	}

	r := recipe.Default().WithSteps(recipeStep)
	origPath := filepath.Join(work, "original.db")
	if err := r.Build(ctx, origPath, recipeStep); err != nil {
		fatal("build original", err)
	}
	original, err := os.ReadFile(origPath)
	if err != nil {
		fatal("read original", err)
	}
	f, err := dbfile.Open(origPath)
	if err != nil {
		fatal("dbfile open", err)
	}
	defer f.Close()
	census, err := f.Census()
	if err != nil {
		fatal("census", err)
	}
	manifest, err := capture.BuildManifest(f)
	if err != nil {
		fatal("manifest", err)
	}
	origLogical, err := equal.LogicalHashFile(ctx, origPath)
	if err != nil {
		fatal("logical hash", err)
	}

	attackINT01(ctx, original, census, manifest, origLogical, work)
	attackINT02(ctx, f, original, census, manifest, origLogical, work)
	attackINT03(ctx, f, original, census, manifest, origLogical, work)
	attackINT04(manifest, original)
}
