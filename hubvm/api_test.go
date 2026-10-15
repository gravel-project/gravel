package hubvm

import (
	"bytes"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublicAPI pins the package's exported declarations, without their comments or bodies, to
// testdata/api.txt. The package is a public surface a control plane imports (gravel#211), so a
// change here is a release decision (docs/releasing.md "Versions"): adding is a patch, removing,
// renaming or changing a signature is Breaking. Rerun with -update and say which in the CHANGELOG.
func TestPublicAPI(t *testing.T) {
	got := publicAPI(t)
	golden := filepath.Join("testdata", "api.txt")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./hubvm -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("hubvm's exported API changed: a public surface (docs/releasing.md); rerun with -update, review the diff and add a CHANGELOG line (Breaking: when anything was removed or changed)\n%s", firstDiff(string(want), got))
	}
}

func publicAPI(t *testing.T) string {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	p, err := doc.NewFromFiles(fset, files, "github.com/gravel-project/gravel/hubvm")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	decl := func(n ast.Node) {
		if err := (&printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}).Fprint(&b, fset, n); err != nil {
			t.Fatal(err)
		}
		b.WriteString("\n")
	}
	values := func(vs []*doc.Value) {
		for _, v := range vs {
			decl(v.Decl)
		}
	}
	funcs := func(fs []*doc.Func) {
		for _, f := range fs {
			decl(f.Decl)
		}
	}
	values(p.Consts)
	values(p.Vars)
	funcs(p.Funcs)
	for _, ty := range p.Types {
		decl(ty.Decl)
		values(ty.Consts)
		values(ty.Vars)
		funcs(ty.Funcs)
		funcs(ty.Methods)
	}
	// The printer leaves a blank line where a field's comment was.
	var out []string
	for line := range strings.Lines(b.String()) {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "")
}
