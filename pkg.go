// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"go/build"
	"go/scanner"
	"go/token"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	gobin = os.Getenv("GOBIN")
)

type packageList []*Package

func (p packageList) Len() int {
	return len(p)
}

func (p packageList) Less(i, j int) bool {
	return p[i].ImportPath < p[j].ImportPath
}

func (p packageList) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func packageBaseImportPath(s string) string {
	i := strings.IndexByte(s, ':')
	if i == -1 {
		return s
	}
	return s[:i]
}

func packageOptions(s string) []string {
	i := strings.IndexByte(s, ':')
	if i == -1 {
		return nil
	}
	return strings.Split(s[i+1:], ",")
}

// A Package describes a single package found in a directory.
type Package struct {
	*build.Package
	buildContext   *build.Context
	baseImportPath string

	Target     string        // install path
	Standard   bool          // is this package part of the standard Go library?
	Stale      bool          // would 'go install' do anything for this package?
	Incomplete bool          // was there an error loading this package or dependencies?
	Error      *PackageError // error loading this package (not dependencies)

	imports     []*Package
	deps        []*Package
	local       bool // imported via local path (./ or ../)
	fingerprint *string
	race        bool
}

// A PackageError describes an error loading information about a package.
type PackageError struct {
	ImportStack   []string // shortest path from package named on command line to this one
	Pos           string   // position of error
	Err           string   // the error itself
	isImportCycle bool     // the error is an import cycle
	hard          bool     // whether the error is soft or hard; soft errors are ignored in some places
}

func (p *PackageError) Error() string {
	// Import cycles deserve special treatment.
	if p.isImportCycle {
		return fmt.Sprintf("%s\npackage %s\n", p.Err, strings.Join(p.ImportStack, "\n\timports "))
	}
	if p.Pos != "" {
		// Omit import stack.  The full path to the file where the error
		// is the most important thing.
		return p.Pos + ": " + p.Err
	}
	if len(p.ImportStack) == 0 {
		return p.Err
	}
	return "package " + strings.Join(p.ImportStack, "\n\timports ") + ": " + p.Err
}

// An importStack is a stack of import paths.
type importStack []string

func (is *importStack) push(p string) {
	*is = append(*is, p)
}

func (is *importStack) pop() {
	*is = (*is)[0 : len(*is)-1]
}

func (is *importStack) copy() []string {
	return append([]string{}, *is...)
}

// shorterThan returns true if sp is shorter than t.
// We use this to record the shortest import sequence
// that leads to a particular package.
func (is *importStack) shorterThan(t []string) bool {
	s := *is
	if len(s) != len(t) {
		return len(s) < len(t)
	}
	// If they are the same length, settle ties using string ordering.
	for i := range s {
		if s[i] != t[i] {
			return s[i] < t[i]
		}
	}
	return false // they are equal
}

// packageCache is a lookup cache for loadPackage,
// so that if we look up a package multiple times
// we return the same pointer each time.
var packageCache = map[string]*Package{}

// dirToImportPath returns the pseudo-import path we use for a package
// outside the Go path.  It begins with _/ and then contains the full path
// to the directory.  If the package lives in c:\home\gopher\my\pkg then
// the pseudo-import path is _/c_/home/gopher/my/pkg.
// Using a pseudo-import path like this makes the ./ imports no longer
// a special case, so that all the code to deal with ordinary imports works
// automatically.
func dirToImportPath(dir string) string {
	return path.Join("_", strings.Map(makeImportValid, filepath.ToSlash(dir)))
}

func makeImportValid(r rune) rune {
	// Should match Go spec, compilers, and ../../go/parser/parser.go:/isValidImport.
	const illegalChars = `!"#$%&'()*,:;<=>?[\]^{|}` + "`\uFFFD"
	if !unicode.IsGraphic(r) || unicode.IsSpace(r) || strings.ContainsRune(illegalChars, r) {
		return '_'
	}
	return r
}

// loadImport scans the directory named by path, which must be an import path,
// but possibly a local import path (an absolute file system path or one beginning
// with ./ or ../).  A local relative path is interpreted relative to srcDir.
// It returns a *Package describing the package found in that directory.
func loadImport(buildContext *build.Context, path string, srcDir string,
	stk *importStack, importPos []token.Position) *Package {
	stk.push(path)
	defer stk.pop()

	// Determine canonical identifier for this package.
	// For a local import the identifier is the pseudo-import path
	// we create from the full directory to the package.
	// Otherwise it is the usual import path.
	importPath := path
	isLocal := build.IsLocalImport(path)
	if isLocal {
		importPath = dirToImportPath(filepath.Join(srcDir, path))
	}
	fullImportPath := importPath
	if contains(buildContext.BuildTags, "race") {
		fullImportPath += ":race"
	}
	if p := packageCache[fullImportPath]; p != nil {
		return reusePackage(p, stk)
	}

	p := new(Package)
	p.local = isLocal
	packageCache[fullImportPath] = p

	// Load package.
	// Import always returns bp != nil, even if an error occurs,
	// in order to return partial information.
	//
	// TODO: After Go 1, decide when to pass build.AllowBinary here.
	// See issue 3268 for mistakes to avoid.
	bp, err := buildContext.Import(path, srcDir, build.ImportComment)
	bp.ImportPath = fullImportPath
	if gobin != "" {
		bp.BinDir = gobin
	}
	if err == nil && !isLocal && bp.ImportComment != "" && bp.ImportComment != path {
		err = fmt.Errorf("code in directory %s expects import %q", bp.Dir, bp.ImportComment)
	}
	p.baseImportPath = importPath
	p.load(buildContext, stk, bp, err)
	if p.Error != nil && len(importPos) > 0 {
		pos := importPos[0]
		pos.Filename = shortPath(pos.Filename)
		p.Error.Pos = pos.String()
	}

	return p
}

// reusePackage reuses package p to satisfy the import at the top
// of the import stack stk.  If this use causes an import loop,
// reusePackage updates p's error information to record the loop.
func reusePackage(p *Package, stk *importStack) *Package {
	// We use p.imports==nil to detect a package that
	// is in the midst of its own loadPackage call
	// (all the recursion below happens before p.imports gets set).
	if p.imports == nil {
		if p.Error == nil {
			p.Error = &PackageError{
				ImportStack:   stk.copy(),
				Err:           "import cycle not allowed",
				isImportCycle: true,
			}
		}
		p.Incomplete = true
	}
	// Don't rewrite the import stack in the error if we have an import cycle.
	// If we do, we'll lose the path that describes the cycle.
	if p.Error != nil && !p.Error.isImportCycle && stk.shorterThan(p.Error.ImportStack) {
		p.Error.ImportStack = stk.copy()
	}
	return p
}

// expandScanner expands a scanner.List error into all the errors in the list.
// The default Error method only shows the first error.
func expandScanner(err error) error {
	// Look for parser errors.
	if err, ok := err.(scanner.ErrorList); ok {
		// Prepare error with \n before each message.
		// When printed in something like context: %v
		// this will put the leading file positions each on
		// its own line.  It will also show all the errors
		// instead of just the first, as err.Error does.
		var buf bytes.Buffer
		for _, e := range err {
			e.Pos.Filename = shortPath(e.Pos.Filename)
			buf.WriteString("\n")
			buf.WriteString(e.Error())
		}
		return errors.New(buf.String())
	}
	return err
}

var raceExclude = map[string]bool{
	"runtime/race": true,
	"runtime/cgo":  true,
	"cmd/cgo":      true,
	"syscall":      true,
	"errors":       true,
}

var cgoExclude = map[string]bool{
	"runtime/cgo": true,
}

var cgoSyscallExclude = map[string]bool{
	"runtime/cgo":  true,
	"runtime/race": true,
}

// load populates p using information from bp, err, which should
// be the result of calling build.Context.Import.
func (p *Package) load(buildContext *build.Context, stk *importStack, bp *build.Package, err error) *Package {
	p.Package = bp
	p.buildContext = buildContext
	p.Standard = p.Goroot && p.ImportPath != "" && !strings.Contains(p.ImportPath, ".")
	p.race = contains(p.buildContext.BuildTags, "race")

	if err != nil {
		p.Incomplete = true
		err = expandScanner(err)
		p.Error = &PackageError{
			ImportStack: stk.copy(),
			Err:         err.Error(),
		}
		return p
	}

	if p.Name == "main" {
		_, elem := filepath.Split(p.Dir)
		full := buildContext.GOOS + "_" + buildContext.GOARCH + "/" + elem
		if buildContext.GOOS != runtime.GOOS || buildContext.GOARCH != runtime.GOARCH {
			// Install cross-compiled binaries to subdirectories of bin.
			elem = full
		}
		if p.BinDir != "" {
			// Install to GOBIN or bin of GOPATH entry.
			p.Target = filepath.Join(p.BinDir, elem)
		}
		if p.Target != "" && buildContext.GOOS == "windows" {
			p.Target += ".exe"
		}
	} else if p.local {
		// Local import turned into absolute path.
		// No permanent install target.
		p.Target = ""
	} else {
		p.Target = p.PkgObj
	}

	importPaths := p.Imports
	// Packages that use cgo import runtime/cgo implicitly.
	// Packages that use cgo also import syscall implicitly,
	// to wrap errno.
	// Exclude certain packages to avoid circular dependencies.
	if len(p.CgoFiles) > 0 && (!p.Standard || !cgoExclude[p.baseImportPath]) {
		importPaths = append(importPaths, "runtime/cgo")
	}
	if len(p.CgoFiles) > 0 && (!p.Standard || !cgoSyscallExclude[p.baseImportPath]) {
		importPaths = append(importPaths, "syscall")
	}
	// Everything depends on runtime, except runtime and unsafe.
	if !p.Standard || (p.baseImportPath != "runtime" && p.baseImportPath != "unsafe") {
		importPaths = append(importPaths, "runtime")
		// When race detection enabled everything depends on runtime/race.
		// Exclude certain packages to avoid circular dependencies.
		if p.race && (!p.Standard || !raceExclude[p.baseImportPath]) {
			importPaths = append(importPaths, "runtime/race")
		}
	}

	// Build list of imported packages and full dependency list.
	imports := make([]*Package, 0, len(p.Imports))
	deps := make(map[string]*Package)
	for i, path := range importPaths {
		if path == "C" {
			continue
		}
		p1 := loadImport(buildContext, path, p.Dir, stk, p.ImportPos[path])
		if p1.local {
			if !p.local && p.Error == nil {
				p.Error = &PackageError{
					ImportStack: stk.copy(),
					Err:         fmt.Sprintf("local import %q in non-local package", path),
				}
				pos := p.ImportPos[path]
				if len(pos) > 0 {
					p.Error.Pos = pos[0].String()
				}
			}
			path = p1.ImportPath
			importPaths[i] = path
		}
		deps[path] = p1
		imports = append(imports, p1)
		for _, dep := range p1.deps {
			deps[dep.ImportPath] = dep
		}
		if p1.Incomplete {
			p.Incomplete = true
		}
	}
	p.imports = imports

	p.deps = make([]*Package, 0, len(deps))
	for _, dep := range deps {
		p.deps = append(p.deps, dep)
	}
	sort.Sort(packageList(p.deps))

	// unsafe is a fake package.
	if p.Standard && (p.baseImportPath == "unsafe" || buildContext.Compiler == "gccgo") {
		p.Target = ""
	}

	// Check for C code compiled with Plan 9 C compiler.
	// No longer allowed except in runtime and runtime/cgo, for now.
	if len(p.CFiles) > 0 && !p.usesCgo() && (!p.Standard || p.baseImportPath != "runtime") {
		p.Error = &PackageError{
			ImportStack: stk.copy(),
			Err:         fmt.Sprintf("C source files not allowed when not using cgo: %s", strings.Join(p.CFiles, " ")),
		}
		return p
	}

	return p
}

// usesSwig reports whether the package needs to run SWIG.
func (p *Package) usesSwig() bool {
	return len(p.SwigFiles) > 0 || len(p.SwigCXXFiles) > 0
}

// usesCgo reports whether the package needs to run cgo
func (p *Package) usesCgo() bool {
	return len(p.CgoFiles) > 0
}

// Fingerprint the package returning a digest that changes if any of
// the sources of the packages or its dependencies change.
func (p *Package) Fingerprint() string {
	if p.fingerprint != nil {
		return *p.fingerprint
	}

	h := sha1.New()

	for _, dep := range p.deps {
		if !p.race && dep.Standard {
			continue
		}
		fp := dep.Fingerprint()
		if fp == "" {
			p.fingerprint = &fp
			return *p.fingerprint
		}
		_, err := h.Write([]byte(fp))
		if err != nil {
			log.Fatal(err)
		}
	}

	// TODO(pmattis): I need to add the output of "go version", not the
	// version/GOOS/GOARCH that build-cache was compiled with.
	flags := stringList(
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
		p.ImportPath,
		p.CgoCFLAGS,
		p.CgoCPPFLAGS,
		p.CgoCXXFLAGS,
		p.CgoLDFLAGS,
		p.CgoPkgConfig)
	for _, flag := range flags {
		_, err := h.Write([]byte(flag))
		if err != nil {
			log.Fatal(err)
		}
	}

	files := stringList(
		p.GoFiles,
		p.CgoFiles,
		p.CFiles,
		p.CXXFiles,
		p.MFiles,
		p.HFiles,
		p.SFiles,
		p.SwigFiles,
		p.SwigCXXFiles,
		p.SysoFiles)
	for _, file := range files {
		_, err := h.Write([]byte(file))
		if err != nil {
			log.Fatal(err)
		}
		f, err := os.Open(filepath.Join(p.Dir, file))
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.Copy(h, f); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}

	s := hex.EncodeToString(h.Sum(nil))
	p.fingerprint = &s
	return *p.fingerprint
}

// computeStale computes the Stale flag in the package dag that starts
// at the named pkgs (command-line arguments).
func computeStale(pkgs []*Package) {
	topRoot := map[string]bool{}
	for _, p := range pkgs {
		topRoot[p.Root] = true
	}

	// packageList returns the list of packages in the dag rooted at roots
	// as visited in a depth-first post-order traversal.
	packageList := func(roots []*Package) []*Package {
		seen := map[*Package]bool{}
		all := []*Package{}
		var walk func(*Package)
		walk = func(p *Package) {
			if seen[p] {
				return
			}
			seen[p] = true
			for _, p1 := range p.imports {
				walk(p1)
			}
			all = append(all, p)
		}
		for _, root := range roots {
			walk(root)
		}
		return all
	}

	for _, p := range packageList(pkgs) {
		p.Stale = isStale(p, topRoot)
	}
}

// The runtime version string takes one of two forms:
// "go1.X[.Y]" for Go releases, and "devel +hash" at tip.
// Determine whether we are in a released copy by
// inspecting the version.
var isGoRelease = strings.HasPrefix(runtime.Version(), "go1")

// isStale reports whether package p needs to be rebuilt.
func isStale(p *Package, topRoot map[string]bool) bool {
	if p.Standard && (p.baseImportPath == "unsafe" || p.buildContext.Compiler == "gccgo") {
		// fake, builtin package
		return false
	}
	if p.Error != nil {
		return true
	}

	// A package without Go sources means we only found
	// the installed .a file.  Since we don't know how to rebuild
	// it, it can't be stale, even if -a is set.  This enables binary-only
	// distributions of Go packages, although such binaries are
	// only useful with the specific version of the toolchain that
	// created them.
	if len(p.GoFiles) == 0 && len(p.CgoFiles) == 0 && len(p.TestGoFiles) == 0 &&
		len(p.XTestGoFiles) == 0 && !p.usesSwig() {
		return false
	}

	if p.Target == "" || p.Stale {
		log.Printf("isStale 1: %s", p.ImportPath)
		return true
	}

	// Package is stale if completely unbuilt.
	var built time.Time
	if fi, err := os.Stat(p.Target); err == nil {
		built = fi.ModTime()
	}
	if built.IsZero() {
		log.Printf("isStale 2: %s", p.ImportPath)
		return true
	}

	olderThan := func(file string) bool {
		fi, err := os.Stat(file)
		return err != nil || fi.ModTime().After(built)
	}

	// Package is stale if a dependency is, or if a dependency is newer.
	for _, p1 := range p.deps {
		if p1.Stale || p1.Target != "" && olderThan(p1.Target) {
			log.Printf("isStale 3: %s", p.ImportPath)
			return true
		}
	}

	// Have installed copy, probably built using current compilers,
	// and built after its imported packages.  The only reason now
	// that we'd have to rebuild it is if the sources were newer than
	// the package.   If a package p is not in the same tree as any
	// package named on the command-line, assume it is up-to-date
	// no matter what the modification times on the source files indicate.
	// This avoids rebuilding $GOROOT packages when people are
	// working outside the Go root, and it effectively makes each tree
	// listed in $GOPATH a separate compilation world.
	// See issue 3149.
	if p.Root != "" && !topRoot[p.Root] {
		return false
	}

	srcs := stringList(p.GoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles,
		p.SFiles, p.CgoFiles, p.SysoFiles, p.SwigFiles, p.SwigCXXFiles)
	for _, src := range srcs {
		if olderThan(filepath.Join(p.Dir, src)) {
			log.Printf("isStale 4: %s", p.ImportPath)
			return true
		}
	}

	return false
}

var cwd, _ = os.Getwd()

// loadPackage is like loadImport but is used for command-line arguments,
// not for paths found in import statements.  In addition to ordinary import paths,arg
// loadPackage accepts pseudo-paths beginning with cmd/ to denote commands
// in the Go command directory, as well as paths to those directories.
func loadPackage(arg string, stk *importStack) *Package {
	base := packageBaseImportPath(arg)
	options := packageOptions(arg)

	// Wasn't a command; must be a package.
	// If it is a local import path but names a standard package,
	// we treat it as if the user specified the standard package.
	// This lets you run go test ./ioutil in package io and be
	// referring to io/ioutil rather than a hypothetical import of
	// "./ioutil".
	if build.IsLocalImport(base) {
		bp, _ := build.Default.ImportDir(filepath.Join(cwd, base), build.FindOnly)
		if bp.ImportPath != "" && bp.ImportPath != "." {
			base = bp.ImportPath
		}
	}

	buildContext := build.Default
	if contains(options, "race") {
		if buildContext.InstallSuffix != "" {
			buildContext.InstallSuffix += "_"
		}
		buildContext.InstallSuffix += "race"
		buildContext.BuildTags = append(buildContext.BuildTags, "race")
	}

	return loadImport(&buildContext, base, cwd, stk, nil)
}

// packagesForBuild is like 'packages' but fails if any of
// the packages or their dependencies have errors
// (cannot be built).
func packagesForBuild(args []string) []*Package {
	if len(args) == 0 {
		args = []string{"."}
	}
	var pkgs []*Package
	var stk importStack
	var set = make(map[string]bool)

	for _, arg := range args {
		if !set[arg] {
			pkgs = append(pkgs, loadPackage(arg, &stk))
			set[arg] = true
		}
	}
	computeStale(pkgs)

	errors := 0
	printed := map[*PackageError]bool{}
	for _, pkg := range pkgs {
		if pkg.Error != nil {
			log.Printf("can't load package: %s", pkg.Error)
			errors++
		}
		for _, dep := range pkg.deps {
			if err := dep.Error; err != nil {
				// Since these are errors in dependencies,
				// the same error might show up multiple times,
				// once in each package that depends on it.
				// Only print each once.
				if !printed[err] {
					printed[err] = true
					log.Printf("%s", err)
					errors++
				}
			}
		}
	}
	if errors > 0 {
		os.Exit(1)
	}
	return pkgs
}

func loadAll(args []string) []*Package {
	roots := packagesForBuild(args)

	seen := map[*Package]bool{}
	all := []*Package{}
	for _, root := range roots {
		if !seen[root] {
			seen[root] = true
			all = append(all, root)
			for _, dep := range root.deps {
				if !seen[dep] {
					seen[dep] = true
					all = append(all, dep)
				}
			}
		}
	}

	sort.Sort(packageList(all))
	return all
}

// shortPath returns an absolute or relative name for path, whatever is shorter.
func shortPath(path string) string {
	if rel, err := filepath.Rel(cwd, path); err == nil && len(rel) < len(path) {
		return rel
	}
	return path
}

// stringList's arguments should be a sequence of string or []string values.
// stringList flattens them into a single []string.
func stringList(args ...interface{}) []string {
	var x []string
	for _, arg := range args {
		switch arg := arg.(type) {
		case []string:
			x = append(x, arg...)
		case string:
			x = append(x, arg)
		default:
			panic("stringList: invalid argument")
		}
	}
	return x
}

func contains(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
