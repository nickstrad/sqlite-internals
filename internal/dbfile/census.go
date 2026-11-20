package dbfile

import (
	"fmt"
	"sort"
)

type Role int

const (
	RoleUnaccounted Role = iota
	RoleTableInterior
	RoleTableLeaf
	RoleIndexInterior
	RoleIndexLeaf
	RoleOverflow
	RoleFreelistTrunk
	RoleFreelistLeaf
	RolePointerMap
	RoleLockByte
)

var AllRoles = []Role{RoleTableInterior, RoleTableLeaf, RoleIndexInterior, RoleIndexLeaf, RoleOverflow, RoleFreelistTrunk, RoleFreelistLeaf, RolePointerMap, RoleLockByte, RoleUnaccounted}

func (r Role) String() string {
	switch r {
	case RoleUnaccounted:
		return "unaccounted"
	case RoleTableInterior:
		return "table-interior"
	case RoleTableLeaf:
		return "table-leaf"
	case RoleIndexInterior:
		return "index-interior"
	case RoleIndexLeaf:
		return "index-leaf"
	case RoleOverflow:
		return "overflow"
	case RoleFreelistTrunk:
		return "freelist-trunk"
	case RoleFreelistLeaf:
		return "freelist-leaf"
	case RolePointerMap:
		return "pointer-map"
	case RoleLockByte:
		return "lock-byte"
	}
	return fmt.Sprintf("role(%d)", int(r))
}

func ParseRole(name string) (Role, bool) {
	for _, r := range AllRoles {
		if r.String() == name {
			return r, true
		}
	}
	return 0, false
}

func roleOf(t PageType) Role {
	switch t {
	case TableInterior:
		return RoleTableInterior
	case TableLeaf:
		return RoleTableLeaf
	case IndexInterior:
		return RoleIndexInterior
	}
	return RoleIndexLeaf
}

type PageInfo struct {
	Number     int
	Role       Role
	Tree       string
	Parent     int
	CellCount  int
	Payload    int
	Unused     int
	MaxPayload uint64
}

type Tree struct {
	Name     string
	Type     string
	RootPage int
	Pages    int
	Overflow int
	Cells    int
}

type Census struct {
	PageSize       int
	UsableSize     int
	PageCount      int
	Pages          []PageInfo
	Trees          []Tree
	Schema         []SchemaEntry
	FreelistTrunks []int
	FreelistLeaves []int
	Unaccounted    []int
}

func (c *Census) Page(n int) PageInfo { return c.Pages[n-1] }

func (c *Census) RoleCounts() map[Role]int {
	counts := map[Role]int{}
	for _, p := range c.Pages {
		counts[p.Role]++
	}
	return counts
}

func (c *Census) FreelistLength() int { return len(c.FreelistTrunks) + len(c.FreelistLeaves) }

func (c *Census) Complete() error {
	if len(c.Unaccounted) == 0 {
		return nil
	}
	shown := c.Unaccounted
	if len(shown) > 10 {
		shown = shown[:10]
	}
	return corruptPage(c.Unaccounted[0], "%d page(s) have no role: %v", len(c.Unaccounted), shown)
}

type censusBuilder struct {
	file   *File
	census *Census
}

func (info PageInfo) describe() string {
	if info.Tree == "" {
		return info.Role.String()
	}
	return fmt.Sprintf("%s of %q", info.Role, info.Tree)
}

func (b *censusBuilder) claim(n int, info PageInfo) error {
	if n < 1 || n > b.census.PageCount {
		return corruptPage(max(n, 0), "page %d (%s) is outside 1..%d", n, info.describe(), b.census.PageCount)
	}
	existing := b.census.Pages[n-1]
	if existing.Role != RoleUnaccounted {
		return corruptPage(n, "page claimed twice: as %s and as %s", existing.describe(), info.describe())
	}
	info.Number = n
	b.census.Pages[n-1] = info
	return nil
}

func (f *File) Census() (*Census, error) {
	c := &Census{PageSize: f.PageSize(), UsableSize: f.UsableSize(), PageCount: f.pageCount, Pages: make([]PageInfo, f.pageCount)}
	for i := range c.Pages {
		c.Pages[i].Number = i + 1
	}
	b := &censusBuilder{file: f, census: c}
	if lock := f.LockBytePage(); lock != 0 {
		if err := b.claim(lock, PageInfo{Role: RoleLockByte}); err != nil {
			return nil, err
		}
	}
	if f.Header.AutoVacuum() {
		for n := 2; n <= f.pageCount; n++ {
			if f.IsPointerMapPage(n) {
				if err := b.claim(n, PageInfo{Role: RolePointerMap}); err != nil {
					return nil, err
				}
			}
		}
	}
	schema, err := f.Schema()
	if err != nil {
		return nil, fmt.Errorf("schema table: %w", err)
	}
	c.Schema = schema
	trees := []Tree{{Name: "sqlite_schema", Type: "table", RootPage: 1}}
	for _, e := range schema {
		if e.RootPage > 0 {
			trees = append(trees, Tree{Name: e.Name, Type: e.Type, RootPage: e.RootPage})
		}
	}
	for i := range trees {
		if err := b.walkTree(&trees[i]); err != nil {
			return nil, fmt.Errorf("tree %q (root %d): %w", trees[i].Name, trees[i].RootPage, err)
		}
	}
	c.Trees = trees
	if err := b.walkFreelist(); err != nil {
		return nil, fmt.Errorf("freelist: %w", err)
	}
	for _, p := range c.Pages {
		if p.Role == RoleUnaccounted {
			c.Unaccounted = append(c.Unaccounted, p.Number)
		}
	}
	return c, nil
}

func (b *censusBuilder) walkTree(t *Tree) error {
	f := b.file
	walker := TreeWalker{File: f}
	walker.OnPage = func(p *BTreePage, parent, depth int) error {
		if depth == 0 && p.Type.IsTable() != (t.Type == "table") {
			return corruptPage(p.Number, "root of %s %q is a %s page", t.Type, t.Name, p.Type)
		}
		t.Pages++
		t.Cells += p.CellCount
		return b.claim(p.Number, PageInfo{Role: roleOf(p.Type), Tree: t.Name, Parent: parent, CellCount: p.CellCount, Unused: p.Unused()})
	}
	walker.OnCell = func(p *BTreePage, c Cell) error {
		info := &b.census.Pages[p.Number-1]
		if p.Type == TableInterior {
			return nil
		}
		info.Payload += c.LocalSize
		info.MaxPayload = max(info.MaxPayload, c.PayloadSize)
		chain, err := f.OverflowChain(p.Number, c)
		if err != nil {
			return err
		}
		perPage := f.UsableSize() - 4
		for _, o := range chain {
			t.Overflow++
			err := b.claim(o.Number, PageInfo{Role: RoleOverflow, Tree: t.Name, Parent: p.Number, Payload: o.Payload, Unused: perPage - o.Payload})
			if err != nil {
				return err
			}
		}
		return nil
	}
	return walker.Walk(t.RootPage)
}

func (b *censusBuilder) walkFreelist() error {
	f := b.file
	want := int(f.Header.FreelistCount)
	maxLeaves := f.UsableSize()/4 - 2
	counted := 0
	trunk := int(f.Header.FirstFreelistTrunk)
	parent := 0
	for trunk != 0 {
		if counted >= want {
			return corruptPage(trunk, "freelist continues past the %d pages the header counts", want)
		}
		if err := b.claim(trunk, PageInfo{Role: RoleFreelistTrunk, Parent: parent}); err != nil {
			return err
		}
		counted++
		b.census.FreelistTrunks = append(b.census.FreelistTrunks, trunk)
		data, err := f.Page(trunk)
		if err != nil {
			return err
		}
		leaves := be32int(data[4:8])
		if leaves > maxLeaves {
			return corruptAt(trunk, 4, "trunk lists %d leaves, at most %d fit", leaves, maxLeaves)
		}
		b.census.Pages[trunk-1].CellCount = leaves
		for i := range leaves {
			leaf := be32int(data[8+4*i:])
			if leaf == 1 || leaf == f.LockBytePage() {
				return corruptAt(trunk, 8+4*i, "freelist leaf %d can never be free", leaf)
			}
			if err := b.claim(leaf, PageInfo{Role: RoleFreelistLeaf, Parent: trunk}); err != nil {
				return err
			}
			b.census.FreelistLeaves = append(b.census.FreelistLeaves, leaf)
		}
		counted += leaves
		parent = trunk
		trunk = be32int(data[0:4])
	}
	if counted != want {
		return corruptAt(0, 36, "header counts %d freelist pages, the trunk chain holds %d", want, counted)
	}
	return nil
}

func (c *Census) SortedTrees() []Tree {
	out := append([]Tree(nil), c.Trees...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].RootPage < out[j].RootPage })
	return out
}
