package equal

import (
	"fmt"
	"sort"
	"strings"
)

type PageChange struct {
	Page       int
	RoleBefore string
	RoleAfter  string
}

func (c PageChange) Added() bool   { return c.RoleBefore == "" }
func (c PageChange) Removed() bool { return c.RoleAfter == "" }

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
		if inA && inB && ea.SHA256 == eb.SHA256 {
			continue
		}
		changes = append(changes, PageChange{Page: p, RoleBefore: ea.Role, RoleAfter: eb.Role})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Page < changes[j].Page })
	return changes
}

func FormatPageChanges(changes []PageChange) string {
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = c.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func PageNumbers(changes []PageChange) []int {
	out := make([]int, len(changes))
	for i, c := range changes {
		out[i] = c.Page
	}
	return out
}
