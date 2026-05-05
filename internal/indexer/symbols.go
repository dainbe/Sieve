package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

type Symbol struct {
	Name    string
	Type    string // "function", "type", "variable"
	Line    int
	Content string // signature line(s) for use in context compression
}

func extractGoSymbols(content string) []Symbol {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, parser.ParseComments)
	if err != nil {
		return nil
	}

	lines := strings.Split(content, "\n")
	lineAt := func(pos token.Pos) string {
		l := fset.Position(pos).Line - 1
		if l >= 0 && l < len(lines) {
			return strings.TrimSpace(lines[l])
		}
		return ""
	}

	// Extract signature: from decl start to opening brace (exclusive)
	sigOf := func(start, end token.Pos) string {
		s := fset.Position(start).Line - 1
		e := fset.Position(end).Line - 1
		if s < 0 || s >= len(lines) {
			return ""
		}
		// Single-line decl
		if s == e {
			return strings.TrimSpace(lines[s])
		}
		// Multi-line: collect until we hit the opening brace
		var sb strings.Builder
		for i := s; i <= e && i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			sb.WriteString(trimmed)
			if strings.HasSuffix(trimmed, "{") {
				break
			}
			sb.WriteString(" ")
		}
		return strings.TrimSpace(sb.String())
	}

	var symbols []Symbol
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			sig := sigOf(x.Pos(), x.Body.Lbrace)
			symbols = append(symbols, Symbol{
				Name:    x.Name.Name,
				Type:    "function",
				Line:    fset.Position(x.Pos()).Line,
				Content: sig,
			})
		case *ast.TypeSpec:
			symbols = append(symbols, Symbol{
				Name:    x.Name.Name,
				Type:    "type",
				Line:    fset.Position(x.Pos()).Line,
				Content: lineAt(x.Pos()),
			})
		case *ast.ValueSpec:
			for _, name := range x.Names {
				symbols = append(symbols, Symbol{
					Name:    name.Name,
					Type:    "variable",
					Line:    fset.Position(name.Pos()).Line,
					Content: lineAt(name.Pos()),
				})
			}
		}
		return true
	})
	return symbols
}
