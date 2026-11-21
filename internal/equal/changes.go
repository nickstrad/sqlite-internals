package equal

import (
	"fmt"
	"sort"
	"strings"
)

// PageChange describes a single page whose bytes differed between two manifests
// or that existed in only one manifest, together with its census roles before
// and after the operation.
type PageChange struct {
	Page       int
	RoleBefore string
	RoleAfter  string
}

// Added reports whether the page did not exist in the before manifest.
func (c PageChange) Added() bool { return c.RoleBefore == "" }

// Removed reports whether the page no longer exists in the after manifest.
func (c PageChange) Removed() bool { return c.RoleAfter == "" }

// String formats c for experiment logs, showing role transitions as well as
// newly allocated and removed pages.
func (c PageChange) String() string {
	switch {
	case c.Added():
		return fmt.Sprintf("%d(new %s)", c.Page, c.RoleAfter)
	case c.Removed():
		return fmt.Sprintf("%d(gone, was %s)", c.Page, c.RoleBefore)
	case c.RoleBefore != c.RoleAfter:
		return fmt.Sprintf("%d(%s->%s)", c.Page, c.RoleBefore, c.RoleAfter)
	}
	return fmt.Sprintf("%d(%s)", c.Page, c.RoleAfter)
}

// ChangedPages compares two per-page hash manifests and returns every page
// whose SHA-256 changed or that was added or removed, sorted by page number.
func ChangedPages(before, after Manifest) []PageChange {
	byPage := func(m Manifest) map[int]ManifestEntry {
		out := make(map[int]ManifestEntry, len(m.Entries))
		for _, e := range m.Entries {
			out[e.Page] = e
		}
		return out
	}
	a, b := byPage(before), byPage(after)
	pages := map[int]bool{}
	for p := range a {
		pages[p] = true
	}
	for p := range b {
		pages[p] = true
	}
	var changes []PageChange
	for p := range pages {
		ea, inA := a[p]
		eb, inB := b[p]
		// Only report byte changes (or added/removed pages). When secure_delete
		// is OFF, freePage2 (btree.c:6934-6935) calls sqlite3PagerDontWrite
		// (pager.c:6329-6339), so a freed leaf changes census role from
		// table-leaf to freelist-leaf without being written to disk or the WAL.
		if inA && inB && ea.SHA256 == eb.SHA256 {
			continue
		}
		changes = append(changes, PageChange{Page: p, RoleBefore: ea.Role, RoleAfter: eb.Role})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Page < changes[j].Page })
	return changes
}

// FormatPageChanges formats a slice of PageChange entries inside square brackets.
func FormatPageChanges(changes []PageChange) string {
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = c.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// PageNumbers extracts the sorted 1-based page numbers from changes.
func PageNumbers(changes []PageChange) []int {
	out := make([]int, len(changes))
	for i, c := range changes {
		out[i] = c.Page
	}
	return out
}
