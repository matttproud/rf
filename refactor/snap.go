// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package refactor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

type Snapshot struct {
	parent   *Snapshot
	fset     *token.FileSet
	target   *Package
	packages []*Package
	pkgByID  map[string]*Package
	edits    map[string]*Edit
	files    map[string]*File
	tcErrs   []types.Error

	r      *Refactor
	cache  *buildCache
	sizes  types.Sizes
	errors int
}

type Package struct {
	Name            string
	Dir             string
	ID              string
	PkgPath         string
	ForTest         string
	Files           []*File
	ImportIDs       []string
	InCurrentModule bool
	Export          string
	BuildID         string
	ImportMap       map[string]string

	Types     *types.Package
	TypesInfo *types.Info
	Sizes     types.Sizes
}

func (p *Package) String() string { return p.PkgPath }

type File struct {
	Name     string
	Text     []byte
	Syntax   *ast.File
	Imports  []string
	Created  bool
	Modified bool
	Deleted  bool
	Hash     string // SHA256(Name+Text)
}

type cachedTypeInfo struct {
	pkg  *types.Package
	info *types.Info
}

type buildCache struct {
	dir   string
	r     *Refactor
	fset  *token.FileSet
	types map[string]*cachedTypeInfo
	files map[string]*File
}

func (s *Snapshot) Refactor() *Refactor { return s.r }

func (s *Snapshot) importToID(p *Package, imp string) string {
	// Make sure all packages import the test version of the target,
	// because its objects are what we will be looking for to apply
	// replacements. This is OK as far as import cycles, because
	// (1) if the package's test already depended on p, p's map
	// would say this anyway, and
	// (2) if not, then introducing this edge can't create a cycle.
	//
	// This logic fails as soon as there are two tests involved, though.
	// For example, both encoding/json and math/big have tests that
	// import the other package. This implies that we have to keep
	// s.target to a single package!
	if imp == s.target.PkgPath {
		return s.target.ID
	}

	new, ok := p.ImportMap[imp]
	if ok {
		return new
	}
	return imp
}

func (s *Snapshot) Errors() int {
	return s.errors
}

func (s *Snapshot) ErrorAt(pos token.Pos, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	msg = strings.TrimRight(msg, "\n")
	msg = strings.Replace(msg, "\n", "\n\t", -1)
	if pos == token.NoPos {
		fmt.Fprintf(s.r.Stderr, "%s\n", msg)
	} else {
		fmt.Fprintf(s.r.Stderr, "%s: %s\n", s.Addr(pos), msg)
	}
	s.errors++
}

func (s *Snapshot) saveErrors(err error) {
	switch err := err.(type) {
	case scanner.ErrorList:
		for _, e := range err {
			s.saveErrors(e)
		}
		return
	}
	s.ErrorAt(token.NoPos, "%v", err)
}

func (s *Snapshot) typecheckError(err error) {
	switch err := err.(type) {
	case scanner.ErrorList:
		for _, e := range err {
			s.typecheckError(e)
		}

	case types.Error:
		s.tcErrs = append(s.tcErrs, err)

	default:
		panic(fmt.Sprintf("typecheck %T", err))
		s.saveErrors(err)
	}
}

func (s *Snapshot) flushTypecheckErrors() {
	count := make(map[string]int)
	for _, e := range s.tcErrs {
		count[e.Msg]++
	}

	for _, e := range s.tcErrs {
		switch {
		case count[e.Msg] > 3:
			n := count[e.Msg]
			count[e.Msg] = -1
			e.Msg += fmt.Sprintf(" [× %d]", n)

		case count[e.Msg] < 0:
			continue
		}
		s.saveErrors(e)
	}
}

func (s *Snapshot) Fset() *token.FileSet { return s.fset }

func (s *Snapshot) Target() *Package {
	return s.target
}

func (s *Snapshot) Packages() []*Package {
	return s.packages
}

func (r *Refactor) Load() (*Snapshot, error) {
	dir := r.dir
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)

	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PWD="+dir)
	bmod, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("loading module: %v\n%s", err, bmod)
	}
	mod := strings.TrimSpace(string(bmod))
	if filepath.Base(mod) != "go.mod" {
		return nil, fmt.Errorf("no module found for " + dir)
	}
	r.modRoot = filepath.Dir(mod)

	cmd = exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PWD="+dir)
	js, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("loading module: %v\n%s", err, bmod)
	}
	var m struct {
		Module struct {
			Path string
		}
	}
	if err := json.Unmarshal(js, &m); err != nil {
		return nil, fmt.Errorf("loading module: %v\n%s", err, bmod)
	}
	r.modPath = m.Module.Path

	cmd = exec.Command("go", "list", "-e", "-json", "-compiled", "-export", "-test", "-deps", "./...")
	cmd.Dir = r.modRoot
	cmd.Env = append(os.Environ(), "PWD="+r.modRoot)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil && !(bytes.HasPrefix(stdout.Bytes(), []byte("{")) || !bytes.HasPrefix(stdout.Bytes(), []byte("}"))) {
		return nil, fmt.Errorf("listing module: %v\n%s%s", err, stderr.Bytes(), stdout.Bytes())
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))

	fset := token.NewFileSet()
	s := &Snapshot{
		r:       r,
		pkgByID: make(map[string]*Package),
		edits:   make(map[string]*Edit),
		files:   make(map[string]*File),
		fset:    fset,
		cache: &buildCache{
			r:     r,
			fset:  fset,
			types: make(map[string]*cachedTypeInfo),
			files: make(map[string]*File),
		},
	}
	if true {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.LoadFiles | packages.NeedImports | packages.NeedDeps,

			Context: context.Background(),

			Fset: fset,

			Tests: true,
		}
		pkgs, err := packages.Load(cfg, "./...")
		if err != nil {
			return nil, fmt.Errorf("could not query packages: %w", err)
		}
		for _, pkg := range pkgs {
			if pkg.Name == "" {
				continue
			}
			// "internal/abi"
			// "rsc.io/rf [rsc.io/rf.test]"
			id := pkg.ID
			pkgPath := pkg.PkgPath
			if strings.HasSuffix(pkgPath, ".test") {
				continue
			}
			p := s.pkgByID[id]
			if p == nil {
				p = new(Package)
				s.pkgByID[id] = p
			}
			p.Name = pkg.Name
		}
	}
	if false {
		for {
			var jp jsonPackage
			err := dec.Decode(&jp)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("loading packages: %v", err)
			}
			if jp.Name == "" {
				continue
			}

			// The forms we expect to see are:
			//	p (the regular package)
			//	p [p.test] (p compiled with test code for the test binary)
			//	p_test [p.test] (p_test compiled for the test binary)
			id := jp.ImportPath
			pkgPath := strings.TrimSuffix(id, " ["+jp.ForTest+".test]")
			if strings.HasSuffix(pkgPath, ".test") {
				// Ignore test binaries.
				continue
			}
			p := s.pkgByID[id]
			if p == nil {
				p = new(Package)
				s.pkgByID[id] = p
			}
			p.Name = jp.Name
			p.Dir = jp.Dir
			p.ID = id
			p.PkgPath = pkgPath
			p.ForTest = jp.ForTest

			// Remember the target package,
			// which is the one in the current directory.
			// If it is compiled twice, once with test code and once without,
			// then prefer the one with test code, which should be strictly larger.
			if p.Dir == dir && !strings.HasSuffix(p.PkgPath, "_test") && (p.ForTest != "" || s.target == nil) {
				s.target = p
			}
			p.InCurrentModule = !jp.DepOnly
			p.Export = jp.Export
			p.ImportIDs = jp.Imports
			p.ImportMap = jp.ImportMap
			if len(p.Files) > 0 {
				// We see the same packages multiple times
				// under certain error conditions, like import cycles.
				continue
			}
			if false && !p.InCurrentModule && p.Export != "" {
				// Outside current module, so not updating.
				// Load from export data.
				// Don't bother with source files.
				continue
			}

			// Set up for loading from source code.
			p.Export = "" // Remember NOT to load from export data.
			for _, name := range jp.CompiledGoFiles {
				if strings.HasSuffix(name, ".s") { // surprise!
					continue
				}
				if !filepath.IsAbs(name) {
					name = filepath.Join(p.Dir, name)
				}
				name = s.r.shortPath(name)
				f, err := s.cache.newFile(name)
				if err != nil {
					s.saveErrors(err)
					continue
				}
				p.Files = append(p.Files, f)
				s.files[f.Name] = f
			}
		}
	}
	s.packages = packagesOf(s.pkgByID)

	// Update ImportIDs to force use of s.target
	// by all imports of s.target.PkgPath,
	// just as s.importToID will.
	// See s.importToID for rationale.
	// Also remove "C" - cgo has been compiled out.
	var pkgs []*Package
	for _, p := range s.packages {
		if p.PkgPath == s.target.PkgPath && p.ID != s.target.ID {
			delete(s.pkgByID, p.ID)
			continue
		}
		pkgs = append(pkgs, p)
		var save []string
		for _, imp := range p.ImportIDs {
			if imp == "C" {
				continue
			}
			if imp == s.target.PkgPath {
				imp = s.target.ID
			}
			save = append(save, imp)
		}
		p.ImportIDs = save
	}
	s.packages = pkgs

	if s.Errors() == 0 {
		s.typeCheck()
	}
	return s, nil
}

func packagesOf(pkgs map[string]*Package) []*Package {
	var list []*Package
	for _, p := range pkgs {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].ID < list[j].ID
	})
	return list
}

// TODO: Rename to Reload
func (s *Snapshot) Load() (*Snapshot, error) {
	oldS := s
	s = &Snapshot{
		r:       oldS.r,
		parent:  oldS,
		fset:    oldS.fset,
		edits:   make(map[string]*Edit),
		pkgByID: make(map[string]*Package),
		files:   make(map[string]*File),
		cache:   s.cache,
		// exp:       s.exp, //  should use cache from now on
		sizes: s.sizes,
	}

	createsByDir := make(map[string][]*Edit)
	for name, ed := range oldS.edits {
		if ed.Create {
			dir := filepath.Dir(name)
			createsByDir[dir] = append(createsByDir[dir], ed)
		}
	}

	for _, oldP := range oldS.packages {
		if !oldP.InCurrentModule { // immutable w/ immutable dependencies
			s.pkgByID[oldP.ID] = oldP
			for _, f := range oldP.Files {
				s.files[f.Name] = f
			}
			continue
		}
		p := &Package{
			Name:            oldP.Name,
			Dir:             oldP.Dir,
			ID:              oldP.ID,
			PkgPath:         oldP.PkgPath,
			ForTest:         oldP.ForTest,
			InCurrentModule: oldP.InCurrentModule,
			ImportMap:       oldP.ImportMap,
		}
		s.pkgByID[p.ID] = p
		if oldS.target == oldP {
			s.target = p
		}

		// Build file list.
		for _, oldF := range oldP.Files {
			ed := oldS.edits[oldF.Name]
			if ed == nil {
				p.Files = append(p.Files, oldF)
				continue
			}
			if ed.Delete {
				f := &File{
					Name:    oldF.Name,
					Deleted: true,
				}
				p.Files = append(p.Files, f)
				continue
			}
			// Use modified file.
			text, err := ed.NewText()
			if err != nil {
				s.ErrorAt(token.NoPos, "%s: %v", oldF.Name, err)
				continue
			}
			f, err := s.cache.newFileText(oldF.Name, text, true)
			if err != nil {
				s.saveErrors(err)
				continue
			}
			p.Files = append(p.Files, f)
		}
		for _, ed := range createsByDir[p.Dir] {
			// Use new file.
			text, err := ed.NewText()
			if err != nil {
				// TODO
				continue
			}
			f, err := s.cache.newFileText(ed.Name, text, true)
			if err != nil {
				s.saveErrors(err)
				continue
			}
			p.Files = append(p.Files, f)
		}
		for _, f := range p.Files {
			s.files[f.Name] = f
		}
	}
	s.packages = packagesOf(s.pkgByID)
	for _, p := range s.packages {
		p.ImportIDs = s.pkgImportIDs(p)
	}

	if s.Errors() == 0 {
		s.typeCheck()
	}
	return s, nil
}

func (s *Snapshot) pkgImportIDs(p *Package) []string {
	var list []string
	have := make(map[string]bool)
	for _, f := range p.Files {
		for _, imp := range f.Imports {
			if imp == "C" {
				continue
			}
			if !have[imp] {
				have[imp] = true
				list = append(list, s.importToID(p, imp))
			}
		}
	}
	sort.Strings(list)
	return list
}

func (s *Snapshot) typeCheck() {
	// Build dependency graph.
	ready := make(map[*Package]bool)
	waiting := make(map[*Package]map[*Package]bool)
	rdeps := make(map[*Package][]*Package)
	for _, p := range s.packages {
		if len(p.ImportIDs) == 0 {
			ready[p] = true
		} else {
			waiting[p] = make(map[*Package]bool)
			for _, id := range p.ImportIDs {
				if id == "C" {
					continue
				}
				p1 := s.pkgByID[id]
				if p1 == nil {
					println("LOST", id)
				}
				waiting[p][p1] = true
				rdeps[p1] = append(rdeps[p1], p)
			}
		}
	}

	// Diagnose cycles (well, at least one).
	walked := make(map[*Package]int)
	var stack []*Package
	var cycle []*Package
	var walk func(*Package)
	walk = func(p *Package) {
		if walked[p] == 2 || cycle != nil {
			return
		}
		if walked[p] == 1 {
			// cycle!
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == p {
					cycle = append(cycle, stack[i:]...)
					return
				}
			}
			return
		}
		walked[p] = 1
		stack = append(stack, p)
		for _, p1 := range rdeps[p] {
			walk(p1)
		}
		stack = stack[:len(stack)-1]
		walked[p] = 2
	}
	for p := range rdeps {
		walk(p)
	}
	if cycle != nil {
		var b bytes.Buffer
		off := 0
		for i := range cycle {
			if cycle[i].PkgPath < cycle[off].PkgPath {
				off = i
			}
		}
		fmt.Fprintf(&b, "%s", cycle[off].PkgPath)
		for i := len(cycle) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, " -> %s", cycle[(off+i)%len(cycle)].PkgPath)
		}
		s.ErrorAt(token.NoPos, "import cycle: %v", b.String())
		return
	}

	// Type check.
	for len(ready) > 0 {
		for p := range ready {
			s.check(p)
			delete(ready, p)
			if p.Types == nil {
				// type-check failed - do not wake importers
				continue
			}
			for _, p1 := range rdeps[p] {
				delete(waiting[p1], p)
				if len(waiting[p1]) == 0 {
					delete(waiting, p1)
					ready[p1] = true
				}
			}
		}
	}

	s.flushTypecheckErrors()

	if len(waiting) > 0 && s.Errors() == 0 {
		fmt.Println("type check stalled:")
		for p, n := range waiting {
			fmt.Println(p.PkgPath, n, rdeps[p])
		}
		panic("type check did not complete")
	}
}

type snapImporter struct {
	s *Snapshot
	p *Package
}

func (s *snapImporter) Import(path string) (*types.Package, error) {
	id := s.s.importToID(s.p, path)
	p := s.s.pkgByID[id]
	if p == nil {
		return nil, fmt.Errorf("import not available: %s", path)
	}
	if p.Types == nil {
		// We are running the type-checking in dependency order,
		// so if this happens, it's a mistake.
		return nil, fmt.Errorf("internal error - import not yet available:")
	}
	return p.Types, nil
}

func (s *Snapshot) check(p *Package) {
	if p.PkgPath == "unsafe" {
		p.Types = types.Unsafe
		return
	}
	if p.BuildID == "" {
		if !p.InCurrentModule && p.Export != "" {
			p.BuildID = p.ID
		} else {
			h := sha256.New()
			for _, f := range p.Files {
				if !f.Deleted {
					h.Write([]byte(f.Hash))
				}
			}
			for _, imp := range p.ImportIDs {
				h.Write([]byte(imp + "\x00" + s.pkgByID[s.importToID(p, imp)].BuildID + "\x00"))
			}
			p.BuildID = fmt.Sprintf("%x", h.Sum(nil))
		}
	}

	// If we have the result from a previous snapshot load, use it.
	if c := s.cache.types[p.BuildID]; c != nil {
		p.Types = c.pkg
		p.TypesInfo = c.info
		return
	}

	if !p.InCurrentModule && p.Export != "" {
		tpkg, err := importer.ForCompiler(s.fset, "gc", opener(p.Export)).Import(p.PkgPath)
		if err != nil {
			s.saveErrors(err)
		}
		p.Types = tpkg
		s.cache.types[p.BuildID] = &cachedTypeInfo{p.Types, nil}
		return
	}

	conf := &types.Config{
		Error:    s.typecheckError,
		Importer: &snapImporter{s, p},
		Sizes:    s.sizes,
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:     make(map[ast.Node]*types.Scope),
	}

	var files []*ast.File
	for _, f := range p.Files {
		if !f.Deleted {
			files = append(files, f.Syntax)
		}
	}
	tpkg, err := conf.Check(p.PkgPath, s.fset, files, info)
	if err != nil {
		return
	}
	p.Types = tpkg
	p.TypesInfo = info

	s.cache.types[p.BuildID] = &cachedTypeInfo{p.Types, p.TypesInfo}
}

func opener(name string) func(string) (io.ReadCloser, error) {
	return func(ignored string) (io.ReadCloser, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
}

func (c *buildCache) newFile(name string) (*File, error) {
	abs := name
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(c.r.dir, name)
	}
	name = c.r.shortPath(abs)
	text, err := ioutil.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return c.newFileText(name, text, false)
}

func (c *buildCache) newFileText(name string, text []byte, modified bool) (*File, error) {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte("\x00"))
	h.Write(text)
	sum := fmt.Sprintf("%x", h.Sum(nil))

	// If we have the result from a previous parse, use it.
	// This is especially important for test packages, so that
	// they use the same ast.File as the non-test package
	// when files are shared.
	if f := c.files[sum]; f != nil {
		return f, nil
	}

	syntax, err := parser.ParseFile(c.fset, name, text, 0)
	if err != nil {
		return nil, err
	}
	f := &File{
		Name:     name,
		Text:     text,
		Syntax:   syntax,
		Imports:  importsOf(syntax),
		Modified: modified,
		Hash:     sum,
	}
	c.files[sum] = f
	return f, nil
}

func importsOf(file *ast.File) []string {
	var list []string
	for _, spec := range file.Imports {
		path := importPath(spec)
		if path != "" {
			list = append(list, path)
		}
	}
	return list
}

type jsonPackage struct {
	Dir           string      // directory containing package sources
	ImportPath    string      // import path of package in dir
	ImportComment string      // path in import comment on package statement
	Name          string      // package name
	Doc           string      // package documentation string
	Target        string      // install path
	Shlib         string      // the shared library that contains this package (only set when -linkshared)
	Goroot        bool        // is this package in the Go root?
	Standard      bool        // is this package part of the standard Go library?
	Stale         bool        // would 'go install' do anything for this package?
	StaleReason   string      // explanation for Stale==true
	Root          string      // Go root or Go path dir containing this package
	ConflictDir   string      // this directory shadows Dir in $GOPATH
	BinaryOnly    bool        // binary-only package (no longer supported)
	ForTest       string      // package is only for use in named test
	Export        string      // file containing export data (when using -export)
	BuildID       string      // build ID of the compiled package (when using -export)
	Module        *jsonModule // info about package's containing module, if any (can be nil)
	Match         []string    // command-line patterns matching this package
	DepOnly       bool        // package is only a dependency, not explicitly listed

	// Source files
	GoFiles           []string // .go source files (excluding CgoFiles, TestGoFiles, XTestGoFiles)
	CgoFiles          []string // .go source files that import "C"
	CompiledGoFiles   []string // .go files presented to compiler (when using -compiled)
	IgnoredGoFiles    []string // .go source files ignored due to build constraints
	IgnoredOtherFiles []string // non-.go source files ignored due to build constraints
	CFiles            []string // .c source files
	CXXFiles          []string // .cc, .cxx and .cpp source files
	MFiles            []string // .m source files
	HFiles            []string // .h, .hh, .hpp and .hxx source files
	FFiles            []string // .f, .F, .for and .f90 Fortran source files
	SFiles            []string // .s source files
	SwigFiles         []string // .swig files
	SwigCXXFiles      []string // .swigcxx files
	SysoFiles         []string // .syso object files to add to archive
	TestGoFiles       []string // _test.go files in package
	XTestGoFiles      []string // _test.go files outside package

	// Cgo directives
	CgoCFLAGS    []string // cgo: flags for C compiler
	CgoCPPFLAGS  []string // cgo: flags for C preprocessor
	CgoCXXFLAGS  []string // cgo: flags for C++ compiler
	CgoFFLAGS    []string // cgo: flags for Fortran compiler
	CgoLDFLAGS   []string // cgo: flags for linker
	CgoPkgConfig []string // cgo: pkg-config names

	// Dependency information
	Imports      []string          // import paths used by this package
	ImportMap    map[string]string // map from source import to ImportPath (identity entries omitted)
	Deps         []string          // all (recursively) imported dependencies
	TestImports  []string          // imports from TestGoFiles
	XTestImports []string          // imports from XTestGoFiles

	// Error information
	Incomplete bool                // this package or a dependency has an error
	Error      *jsonPackageError   // error loading package
	DepsErrors []*jsonPackageError // errors loading dependencies
}

type jsonPackageError struct {
	ImportStack []string // shortest path from package named on command line to this one
	Pos         string   // position of error (if present, file:line:col)
	Err         string   // the error itself
}

type jsonModule struct {
	Path      string           // module path
	Version   string           // module version
	Versions  []string         // available module versions (with -versions)
	Replace   *jsonModule      // replaced by this module
	Time      *time.Time       // time version was created
	Update    *jsonModule      // available update, if any (with -u)
	Main      bool             // is this the main module?
	Indirect  bool             // is this module only an indirect dependency of main module?
	Dir       string           // directory holding files for this module, if any
	GoMod     string           // path to go.mod file used when loading this module, if any
	GoVersion string           // go version used in module
	Retracted string           // retraction information, if any (with -retracted or -u)
	Error     *jsonModuleError // error loading module
}

type jsonModuleError struct {
	Err string // the error itself
}

// stringList flattens its arguments into a single []string.
// Each argument in args must have type string or []string.
// Copied from cmd/go.
func stringList(args ...interface{}) []string {
	var x []string
	for _, arg := range args {
		switch arg := arg.(type) {
		case []string:
			x = append(x, arg...)
		case string:
			x = append(x, arg)
		default:
			panic("stringList: invalid argument of type " + fmt.Sprintf("%T", arg))
		}
	}
	return x
}
