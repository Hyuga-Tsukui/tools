// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.20

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"html/template"
	"io"
	"log"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

//go:embed doc.go
var doc string

// flags
var (
	testFlag = flag.Bool("test", false, "include implicit test packages and executables")
	tagsFlag = flag.String("tags", "", "comma-separated list of extra build tags (see: go help buildconstraint)")

	filterFlag    = flag.String("filter", "<module>", "report only packages matching this regular expression (default: module of first package)")
	generatedFlag = flag.Bool("generated", false, "include dead functions in generated Go files")
	formatFlag    = flag.String("format", "", "format output records using template")
	jsonFlag      = flag.Bool("json", false, "output JSON records")
	cpuProfile    = flag.String("cpuprofile", "", "write CPU profile to this file")
	memProfile    = flag.String("memprofile", "", "write memory profile to this file")
)

func usage() {
	// Extract the content of the /* ... */ comment in doc.go.
	_, after, _ := strings.Cut(doc, "/*\n")
	doc, _, _ := strings.Cut(after, "*/")
	io.WriteString(flag.CommandLine.Output(), doc+`
Flags:

`)
	flag.PrintDefaults()
}

func main() {
	log.SetPrefix("deadcode: ")
	log.SetFlags(0) // no time prefix

	flag.Usage = usage
	flag.Parse()
	if len(flag.Args()) == 0 {
		usage()
		os.Exit(2)
	}

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		// NB: profile won't be written in case of error.
		defer pprof.StopCPUProfile()
	}

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			log.Fatal(err)
		}
		// NB: profile won't be written in case of error.
		defer func() {
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatalf("Writing memory profile: %v", err)
			}
			f.Close()
		}()
	}

	var tmpl *template.Template
	if *formatFlag != "" {
		if *jsonFlag {
			log.Fatalf("you cannot specify both -format=template and -json")
		}
		var err error
		tmpl, err = template.New("deadcode").Parse(*formatFlag)
		if err != nil {
			log.Fatalf("invalid -format: %v", err)
		}
	}

	// Load, parse, and type-check the complete program(s).
	cfg := &packages.Config{
		BuildFlags: []string{"-tags=" + *tagsFlag},
		Mode:       packages.LoadAllSyntax | packages.NeedModule,
		Tests:      *testFlag,
	}
	initial, err := packages.Load(cfg, flag.Args()...)
	if err != nil {
		log.Fatalf("Load: %v", err)
	}
	if len(initial) == 0 {
		log.Fatalf("no packages")
	}
	if packages.PrintErrors(initial) > 0 {
		log.Fatalf("packages contain errors")
	}

	// Gather names of generated files.
	generated := make(map[string]bool)
	packages.Visit(initial, nil, func(p *packages.Package) {
		for _, file := range p.Syntax {
			if isGenerated(file) {
				generated[p.Fset.File(file.Pos()).Name()] = true
			}
		}
	})

	// If -filter is unset, use first module (if available).
	if *filterFlag == "<module>" {
		if mod := initial[0].Module; mod != nil && mod.Path != "" {
			*filterFlag = "^" + regexp.QuoteMeta(mod.Path) + "\\b"
		} else {
			*filterFlag = "" // match any
		}
	}
	filter, err := regexp.Compile(*filterFlag)
	if err != nil {
		log.Fatalf("-filter: %v", err)
	}

	// Create SSA-form program representation
	// and find main packages.
	prog, pkgs := ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	mains := ssautil.MainPackages(pkgs)
	if len(mains) == 0 {
		log.Fatalf("no main packages")
	}
	var roots []*ssa.Function
	for _, main := range mains {
		roots = append(roots, main.Func("init"), main.Func("main"))
	}

	// Compute the reachabilty from main.
	// (We don't actually build a call graph.)
	res := rta.Analyze(roots, false)

	// Subtle: the -test flag causes us to analyze test variants
	// such as "package p as compiled for p.test" or even "for q.test".
	// This leads to multiple distinct ssa.Function instances that
	// represent the same source declaration, and it is essentially
	// impossible to discover this from the SSA representation
	// (since it has lost the connection to go/packages.Package.ID).
	//
	// So, we de-duplicate such variants by position:
	// if any one of them is live, we consider all of them live.
	// (We use Position not Pos to avoid assuming that files common
	// to packages "p" and "p [p.test]" were parsed only once.)
	reachablePosn := make(map[token.Position]bool)
	for fn := range res.Reachable {
		if fn.Pos().IsValid() {
			reachablePosn[prog.Fset.Position(fn.Pos())] = true
		}
	}

	// Group unreachable functions by package path.
	byPkgPath := make(map[string]map[*ssa.Function]bool)
	for fn := range ssautil.AllFunctions(prog) {
		if fn.Synthetic != "" {
			continue // ignore synthetic wrappers etc
		}

		// Use generic, as instantiations may not have a Pkg.
		if orig := fn.Origin(); orig != nil {
			fn = orig
		}

		// Ignore unreachable nested functions.
		// Literal functions passed as arguments to other
		// functions are of course address-taken and there
		// exists a dynamic call of that signature, so when
		// they are unreachable, it is invariably because the
		// parent is unreachable.
		if fn.Parent() != nil {
			continue
		}

		posn := prog.Fset.Position(fn.Pos())

		if !reachablePosn[posn] {
			reachablePosn[posn] = true // suppress dups with same pos

			pkgpath := fn.Pkg.Pkg.Path()
			m, ok := byPkgPath[pkgpath]
			if !ok {
				m = make(map[*ssa.Function]bool)
				byPkgPath[pkgpath] = m
			}
			m[fn] = true
		}
	}

	var packages []jsonPackage

	// Report dead functions grouped by packages.
	// TODO(adonovan): use maps.Keys, twice.
	pkgpaths := make([]string, 0, len(byPkgPath))
	for pkgpath := range byPkgPath {
		pkgpaths = append(pkgpaths, pkgpath)
	}
	sort.Strings(pkgpaths)
	for _, pkgpath := range pkgpaths {
		if !filter.MatchString(pkgpath) {
			continue
		}

		m := byPkgPath[pkgpath]

		// Print functions that appear within the same file in
		// declaration order. This tends to keep related
		// methods such as (T).Marshal and (*T).Unmarshal
		// together better than sorting.
		fns := make([]*ssa.Function, 0, len(m))
		for fn := range m {
			fns = append(fns, fn)
		}
		sort.Slice(fns, func(i, j int) bool {
			xposn := prog.Fset.Position(fns[i].Pos())
			yposn := prog.Fset.Position(fns[j].Pos())
			if xposn.Filename != yposn.Filename {
				return xposn.Filename < yposn.Filename
			}
			return xposn.Line < yposn.Line
		})

		var functions []jsonFunction
		for _, fn := range fns {
			posn := prog.Fset.Position(fn.Pos())

			// Without -generated, skip functions declared in
			// generated Go files.
			// (Functions called by them may still be reported.)
			gen := generated[posn.Filename]
			if gen && !*generatedFlag {
				continue
			}

			functions = append(functions, jsonFunction{
				Name:      fn.String(),
				RelName:   fn.RelString(fn.Pkg.Pkg),
				Posn:      posn.String(),
				Generated: gen,
			})
		}
		packages = append(packages, jsonPackage{
			Path:  pkgpath,
			Funcs: functions,
		})
	}

	// Format the output, in the manner of 'go list (-json|-f=template)'.
	switch {
	case *jsonFlag:
		// -json
		out, err := json.MarshalIndent(packages, "", "\t")
		if err != nil {
			log.Fatalf("internal error: %v", err)
		}
		os.Stdout.Write(out)

	case tmpl != nil:
		// -format=template
		for _, p := range packages {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, p); err != nil {
				log.Fatal(err)
			}
			if n := buf.Len(); n == 0 || buf.Bytes()[n-1] != '\n' {
				buf.WriteByte('\n')
			}
			os.Stdout.Write(buf.Bytes())
		}

	default:
		// functions grouped by package
		for _, pkg := range packages {
			seen := false
			for _, fn := range pkg.Funcs {
				if !seen {
					seen = true
					fmt.Println(pkg.Path)
				}
				fmt.Printf("\t%s\n", fn.RelName)
			}
			if seen {
				fmt.Println()
			}
		}
	}
}

// TODO(adonovan): use go1.21's ast.IsGenerated.

// isGenerated reports whether the file was generated by a program,
// not handwritten, by detecting the special comment described
// at https://go.dev/s/generatedcode.
//
// The syntax tree must have been parsed with the ParseComments flag.
// Example:
//
//	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.PackageClauseOnly)
//	if err != nil { ... }
//	gen := ast.IsGenerated(f)
func isGenerated(file *ast.File) bool {
	_, ok := generator(file)
	return ok
}

func generator(file *ast.File) (string, bool) {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if comment.Pos() > file.Package {
				break // after package declaration
			}
			// opt: check Contains first to avoid unnecessary array allocation in Split.
			const prefix = "// Code generated "
			if strings.Contains(comment.Text, prefix) {
				for _, line := range strings.Split(comment.Text, "\n") {
					if rest, ok := strings.CutPrefix(line, prefix); ok {
						if gen, ok := strings.CutSuffix(rest, " DO NOT EDIT."); ok {
							return gen, true
						}
					}
				}
			}
		}
	}
	return "", false
}

// -- output protocol (for JSON or text/template) --

// Keep in sync with doc comment!

type jsonFunction struct {
	Name      string // name (with package qualifier)
	RelName   string // name (sans package qualifier)
	Posn      string // position in form "filename:line:col"
	Generated bool   // function is declared in a generated .go file
}

func (f jsonFunction) String() string { return f.Name }

type jsonPackage struct {
	Path  string
	Funcs []jsonFunction
}

func (p jsonPackage) String() string { return p.Path }
