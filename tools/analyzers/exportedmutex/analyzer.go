package exportedmutex

import (
	"go/ast"
	"strings"
	"unicode"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "exportedmutex",
	Doc:      "checks for exported sync.Mutex or sync.RWMutex fields in structs",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	inspect.Preorder([]ast.Node{(*ast.StructType)(nil)}, func(n ast.Node) {
		st := n.(*ast.StructType)
		for _, field := range st.Fields.List {
			typ := pass.TypesInfo.TypeOf(field.Type)
			if typ == nil {
				continue
			}
			typStr := typ.String()
			if typStr != "sync.Mutex" && typStr != "sync.RWMutex" {
				continue
			}
			for _, name := range field.Names {
				if unicode.IsUpper(rune(name.Name[0])) {
					pass.Reportf(name.Pos(),
						"mutex field %s is exported; use %s%s instead",
						name.Name,
						strings.ToLower(name.Name[:1]),
						name.Name[1:],
					)
				}
			}
		}
	})
	return nil, nil
}
