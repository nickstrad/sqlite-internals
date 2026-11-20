package govfs

import (
	"context"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func translatedLibDir(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "modernc.org/sqlite").Output()
	if err != nil {
		t.Skipf("go list -m modernc.org/sqlite: %v", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "lib")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("translated library source not available: %v", err)
	}
	return dir
}

type translatedSource struct {
	funcs   map[string]*ast.FuncDecl
	aliases map[string]ast.Expr
}

func parseTranslated(t *testing.T, dir string, wanted map[string]bool) translatedSource {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := translatedSource{funcs: map[string]*ast.FuncDecl{}, aliases: map[string]ast.Expr{}}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		match, err := build.Default.MatchFile(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if !match {
			continue
		}
		text, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		hasWanted := false
		for fn := range wanted {
			if strings.Contains(string(text), "func "+fn+"(") {
				hasWanted = true
				break
			}
		}
		if !hasWanted && !strings.Contains(string(text), "type Tsqlite3_int64 =") && !strings.Contains(string(text), "type Ti64 =") {
			continue
		}
		file, err := parser.ParseFile(fset, name, text, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && wanted[d.Name.Name] {
					src.funcs[d.Name.Name] = d
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Assign.IsValid() {
						src.aliases[ts.Name.Name] = ts.Type
					}
				}
			}
		}
	}
	return src
}

func (s translatedSource) typeString(t *testing.T, e ast.Expr) string {
	t.Helper()
	switch x := e.(type) {
	case *ast.Ident:
		if target, ok := s.aliases[x.Name]; ok {
			return s.typeString(t, target)
		}
		return x.Name
	case *ast.StarExpr:
		return "*" + s.typeString(t, x.X)
	case *ast.SelectorExpr:
		return s.typeString(t, x.X) + "." + x.Sel.Name
	}
	t.Fatalf("unexpected type expression %T", e)
	return ""
}

func (s translatedSource) fieldTypes(t *testing.T, fields *ast.FieldList) []string {
	if fields == nil {
		return nil
	}
	var types []string
	for _, f := range fields.List {
		n := max(len(f.Names), 1)
		for range n {
			types = append(types, s.typeString(t, f.Type))
		}
	}
	return types
}

func reflectTypes(fn reflect.Type) (in, out []string) {
	for i := range fn.NumIn() {
		in = append(in, fn.In(i).String())
	}
	for i := range fn.NumOut() {
		out = append(out, fn.Out(i).String())
	}
	return in, out
}

func TestSignaturesMatchTranslatedSource(t *testing.T) {
	v := registerForTest(t, Options{})
	path := filepath.Join(t.TempDir(), "t.db")
	db := openWith(t, path, "wal", v.Name())
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	targets := ForwardTargets()
	seen := map[string]Target{}
	wanted := map[string]bool{}
	for _, target := range targets {
		short := strings.TrimPrefix(target.Function, translatedPackage)
		seen[target.Slot.Method] = target
		wanted[short] = true
	}
	for _, slot := range append(VFSSlots(), IOSlots()...) {
		if _, ok := seen[slot.Method]; !ok {
			t.Errorf("slot %s was not resolved to a translated function", slot.Method)
		}
	}
	src := parseTranslated(t, translatedLibDir(t), wanted)
	for method, target := range seen {
		short := strings.TrimPrefix(target.Function, translatedPackage)
		decl, ok := src.funcs[short]
		if !ok {
			t.Errorf("%s: translated function %s not found in source", method, short)
			continue
		}
		srcIn := src.fieldTypes(t, decl.Type.Params)
		srcOut := src.fieldTypes(t, decl.Type.Results)
		goIn, goOut := reflectTypes(target.Slot.Signature)
		if strings.Join(srcIn, ",") != strings.Join(goIn, ",") || strings.Join(srcOut, ",") != strings.Join(goOut, ",") {
			t.Errorf("%s: translated %s(%s) (%s), shim signature (%s) (%s)", method, short, strings.Join(srcIn, ", "), strings.Join(srcOut, ", "), strings.Join(goIn, ", "), strings.Join(goOut, ", "))
		}
	}
	if len(seen) != len(vfsSlots)+len(ioSlots) {
		t.Errorf("resolved %d slots, want %d", len(seen), len(vfsSlots)+len(ioSlots))
	}
}
