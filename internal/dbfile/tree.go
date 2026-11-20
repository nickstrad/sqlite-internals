package dbfile

import "fmt"

const MaxTreeDepth = 20

type TreeWalker struct {
	File   *File
	OnPage func(p *BTreePage, parent int, depth int) error
	OnCell func(p *BTreePage, c Cell) error
}

func (w TreeWalker) Walk(root int) error {
	rootPage, err := w.File.BTreePage(root)
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	return w.walk(rootPage, rootPage.Type.IsTable(), 0, 0, seen)
}

func (w TreeWalker) walk(p *BTreePage, table bool, parent, depth int, seen map[int]bool) error {
	if depth >= MaxTreeDepth {
		return corruptPage(p.Number, "b-tree is deeper than %d levels", MaxTreeDepth)
	}
	if seen[p.Number] {
		return corruptPage(parent, "b-tree points to page %d a second time", p.Number)
	}
	seen[p.Number] = true
	if p.Type.IsTable() != table {
		return corruptPage(p.Number, "%s page inside a %s b-tree", p.Type, treeKind(table))
	}
	if w.OnPage != nil {
		if err := w.OnPage(p, parent, depth); err != nil {
			return err
		}
	}
	for i := range p.CellCount {
		c, err := p.Cell(i)
		if err != nil {
			return err
		}
		if !p.Type.IsLeaf() {
			if err := w.descend(p, int(c.LeftChild), table, depth, seen); err != nil {
				return err
			}
		}
		if w.OnCell != nil {
			if err := w.OnCell(p, c); err != nil {
				return err
			}
		}
	}
	if !p.Type.IsLeaf() {
		return w.descend(p, int(p.RightChild), table, depth, seen)
	}
	return nil
}

func (w TreeWalker) descend(p *BTreePage, child int, table bool, depth int, seen map[int]bool) error {
	if child < 2 || child > w.File.PageCount() || child == w.File.LockBytePage() || w.File.IsPointerMapPage(child) {
		return corruptPage(p.Number, "child pointer to page %d, which cannot be a b-tree page", child)
	}
	childPage, err := w.File.BTreePage(child)
	if err != nil {
		return err
	}
	return w.walk(childPage, table, p.Number, depth+1, seen)
}

func treeKind(table bool) string {
	if table {
		return "table"
	}
	return "index"
}

type Row struct {
	Rowid         int64
	Values        []Value
	Page          int
	OverflowPages []int
}

func (f *File) WalkTable(root int, visit func(Row) error) error {
	walker := TreeWalker{File: f}
	walker.OnPage = func(p *BTreePage, parent, depth int) error {
		if depth == 0 && !p.Type.IsTable() {
			return corruptPage(p.Number, "page %d is the root of an index, not a table", p.Number)
		}
		return nil
	}
	walker.OnCell = func(p *BTreePage, c Cell) error {
		if p.Type != TableLeaf {
			return nil
		}
		payload, chain, err := f.Payload(p.Number, c)
		if err != nil {
			return err
		}
		values, err := DecodeRecord(payload)
		if err != nil {
			return corruptAt(p.Number, c.Offset, "row %d: %v", c.Rowid, err)
		}
		row := Row{Rowid: c.Rowid, Values: values, Page: p.Number}
		for _, o := range chain {
			row.OverflowPages = append(row.OverflowPages, o.Number)
		}
		return visit(row)
	}
	return walker.Walk(root)
}

type SchemaEntry struct {
	Type      string
	Name      string
	TableName string
	RootPage  int
	SQL       string
	Rowid     int64
}

func (f *File) textValue(v Value, what string) (string, error) {
	switch v.Class {
	case Null:
		return "", nil
	case Text:
		return DecodeText(v.Bytes, f.Header.TextEncoding)
	}
	return "", fmt.Errorf("schema column %s holds %s, want text", what, v.Class)
}

func (f *File) Schema() ([]SchemaEntry, error) {
	var entries []SchemaEntry
	err := f.WalkTable(1, func(row Row) error {
		if len(row.Values) < 5 {
			return corruptPage(row.Page, "schema row %d has %d columns, want 5", row.Rowid, len(row.Values))
		}
		e := SchemaEntry{Rowid: row.Rowid}
		var err error
		names := []string{"type", "name", "tbl_name"}
		targets := []*string{&e.Type, &e.Name, &e.TableName}
		for i, target := range targets {
			if *target, err = f.textValue(row.Values[i], names[i]); err != nil {
				return corruptPage(row.Page, "schema row %d: %v", row.Rowid, err)
			}
		}
		if e.SQL, err = f.textValue(row.Values[4], "sql"); err != nil {
			return corruptPage(row.Page, "schema row %d: %v", row.Rowid, err)
		}
		switch root := row.Values[3]; root.Class {
		case Null:
		case Integer:
			if root.Int < 0 || root.Int > int64(f.pageCount) {
				return corruptPage(row.Page, "schema row %d (%s): root page %d is outside 0..%d", row.Rowid, e.Name, root.Int, f.pageCount)
			}
			e.RootPage = int(root.Int)
		default:
			return corruptPage(row.Page, "schema row %d (%s): root page holds %s", row.Rowid, e.Name, root.Class)
		}
		entries = append(entries, e)
		return nil
	})
	return entries, err
}
