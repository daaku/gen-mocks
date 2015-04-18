package main // import "sourcegraph.com/sourcegraph/gen-mocks"

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/imports"
)

var (
	ifacePkgDir = flag.String("p", ".", "directory of package containing interface types")
	ifacePat    = flag.String("i", ".+Service", "regexp pattern for selecting interface types by name")
	writeFiles  = flag.Bool("w", false, "write over existing files in output directory (default: writes to stdout)")
	outDir      = flag.String("o", ".", "output directory")
	outPkg      = flag.String("outpkg", "", "output pkg name (default: same as input pkg)")
	namePrefix  = flag.String("name_prefix", "Mock", "output: name prefix of mock impl types (e.g., T -> MockT)")

	fset = token.NewFileSet()
)

func main() {
	flag.Parse()
	log.SetFlags(0)

	bpkg, err := build.Import(*ifacePkgDir, ".", build.FindOnly)
	if err != nil {
		log.Fatal(err)
	}

	pat, err := regexp.Compile(*ifacePat)
	if err != nil {
		log.Fatal(err)
	}

	pkgs, err := parser.ParseDir(fset, *ifacePkgDir, nil, parser.AllErrors)
	if err != nil {
		log.Fatal(err)
	}

	for _, pkg := range pkgs {
		ifaces, err := readIfaces(pkg, pat)
		if err != nil {
			log.Fatal(err)
		}
		if len(ifaces) == 0 {
			log.Printf("warning: package has no interface types matching %q", *ifacePat)
			continue
		}

		var pkgName string
		if *outPkg == "" {
			pkgName = pkg.Name
		} else {
			pkgName = *outPkg
		}

		if err := writeMockImplFiles(*outDir, pkgName, pkg.Name, bpkg.ImportPath, ifaces); err != nil {
			log.Fatal(err)
		}
	}
}

// readIfaces returns a list of interface types in pkg that should be
// mocked.
func readIfaces(pkg *ast.Package, pat *regexp.Regexp) ([]*ast.TypeSpec, error) {
	var ifaces []*ast.TypeSpec
	ast.Walk(visitFn(func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.GenDecl:
			if node.Tok == token.TYPE {
				for _, spec := range node.Specs {
					tspec := spec.(*ast.TypeSpec)
					if _, ok := tspec.Type.(*ast.InterfaceType); !ok {
						continue
					}
					if name := tspec.Name.Name; pat.MatchString(name) {
						ifaces = append(ifaces, tspec)
					}
				}
			}
			return false
		default:
			return true
		}
	}), pkg)
	return ifaces, nil
}

type visitFn func(node ast.Node) (descend bool)

func (v visitFn) Visit(node ast.Node) ast.Visitor {
	descend := v(node)
	if descend {
		return v
	} else {
		return nil
	}
}

func writeMockImplFiles(outDir, outPkg, ifacePkgName, ifacePkgPath string, svcIfaces []*ast.TypeSpec) error {
	if err := os.MkdirAll(outDir, 0700); err != nil {
		return err
	}
	for _, iface := range svcIfaces {
		var decls []ast.Decl

		// mock method fields on struct
		var methFields []*ast.Field
		for _, methField := range iface.Type.(*ast.InterfaceType).Methods.List {
			if meth, ok := methField.Type.(*ast.FuncType); ok {
				methFields = append(methFields, &ast.Field{
					Names: []*ast.Ident{ast.NewIdent(methField.Names[0].Name + "_")},
					Type:  meth,
				})
			}
		}

		// struct implementation type
		mockTypeName := *namePrefix + iface.Name.Name
		implType := &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{&ast.TypeSpec{
			Name: ast.NewIdent(mockTypeName),
			Type: &ast.StructType{Fields: &ast.FieldList{List: methFields}},
		}}}
		decls = append(decls, implType)

		// struct methods
		for _, methField := range iface.Type.(*ast.InterfaceType).Methods.List {
			if meth, ok := methField.Type.(*ast.FuncType); ok {
				synthesizeFieldNamesIfMissing(meth.Params)
				if ifacePkgName != outPkg {
					// TODO(sqs): check for import paths or dirs unequal, not pkg name
					qualifyPkgRefs(meth, ifacePkgName)
				}
				decls = append(decls, &ast.FuncDecl{
					Recv: &ast.FieldList{List: []*ast.Field{
						{
							Names: []*ast.Ident{ast.NewIdent("s")},
							Type:  &ast.StarExpr{X: ast.NewIdent(mockTypeName)},
						},
					}},
					Name: ast.NewIdent(methField.Names[0].Name),
					Type: meth,
					Body: &ast.BlockStmt{List: []ast.Stmt{
						&ast.ReturnStmt{Results: []ast.Expr{
							&ast.CallExpr{
								Fun: &ast.SelectorExpr{
									X:   ast.NewIdent("s"),
									Sel: ast.NewIdent(methField.Names[0].Name + "_"),
								},
								Args: fieldListToIdentList(meth.Params),
							},
						}},
					}},
				})
			}
		}

		// compile-time implements checks
		var ifaceType ast.Expr
		if ifacePkgName == outPkg {
			ifaceType = ast.NewIdent(iface.Name.Name)
		} else {
			ifaceType = &ast.SelectorExpr{X: ast.NewIdent(ifacePkgName), Sel: ast.NewIdent(iface.Name.Name)}
		}
		decls = append(decls, &ast.GenDecl{
			Tok: token.VAR,
			Specs: []ast.Spec{
				&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("_")},
					Type:  ifaceType,
					Values: []ast.Expr{
						&ast.CallExpr{
							Fun:  &ast.ParenExpr{X: &ast.StarExpr{X: ast.NewIdent(mockTypeName)}},
							Args: []ast.Expr{ast.NewIdent("nil")},
						},
					},
				},
			},
		})

		file := &ast.File{
			Name:  ast.NewIdent(outPkg),
			Decls: decls,
		}
		filename := fset.Position(iface.Pos()).Filename
		filename = filepath.Join(outDir, strings.TrimSuffix(filepath.Base(filename), ".go")+"_mock.go")
		log.Println("#", filename)
		var w io.Writer
		if *writeFiles {
			f, err := os.Create(filename)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		} else {
			w = os.Stdout
		}

		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, file); err != nil {
			return err
		}

		// Always put blank lines between funcs.
		src := bytes.Replace(buf.Bytes(), []byte("}\nfunc"), []byte("}\n\nfunc"), -1)

		var err error
		src, err = imports.Process(filename, src, nil)
		if err != nil {
			return err
		}

		fmt.Fprintln(w, "// generated by gen-mocks; DO NOT EDIT")
		fmt.Fprintln(w)
		w.Write(src)
	}
	return nil
}

// qualifyPkgRefs qualifies all refs to non-package-qualified non-builtin types in f so that they refer to definitions in pkg. E.g., 'func(x MyType) -> func (x pkg.MyType)'.
func qualifyPkgRefs(f *ast.FuncType, pkg string) {
	var qualify func(x ast.Expr) ast.Expr
	qualify = func(x ast.Expr) ast.Expr {
		switch y := x.(type) {
		case *ast.Ident:
			if ast.IsExported(y.Name) {
				return &ast.SelectorExpr{X: ast.NewIdent(pkg), Sel: y}
			}
		case *ast.StarExpr:
			y.X = qualify(y.X)
		case *ast.ArrayType:
			y.Elt = qualify(y.Elt)
		case *ast.MapType:
			y.Key = qualify(y.Key)
			y.Value = qualify(y.Value)
		}
		return x
	}
	for _, p := range f.Params.List {
		p.Type = qualify(p.Type)
	}
	for _, r := range f.Results.List {
		r.Type = qualify(r.Type)
	}
}

// synthesizeFieldNamesIfMissing adds synthesized variable names to fl
// if it contains fields with no name. E.g., the field list in
// `func(string, int)` would be converted to `func(v0 string, v1
// int)`.
func synthesizeFieldNamesIfMissing(fl *ast.FieldList) {
	for i, f := range fl.List {
		if len(f.Names) == 0 {
			f.Names = []*ast.Ident{ast.NewIdent(fmt.Sprintf("v%d", i))}
		}
	}
}

func fieldListToIdentList(fl *ast.FieldList) []ast.Expr {
	var fs []ast.Expr
	for _, f := range fl.List {
		for _, name := range f.Names {
			fs = append(fs, ast.NewIdent(name.Name))
		}
	}
	return fs
}
