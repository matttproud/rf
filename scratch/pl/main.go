package main

import (
	"context"
	"fmt"
	"go/token"

	"github.com/kr/pretty"
	"golang.org/x/tools/go/packages"
)

func main() {
	fset := token.NewFileSet()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.LoadFiles | packages.NeedImports | packages.NeedDeps,

		Context: context.Background(),

		Fset: fset,

		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		panic(err)
	}
	for i, pkg := range pkgs {
		fmt.Printf("%d: %v\n", i, pretty.Sprint(pkg))
	}
}
