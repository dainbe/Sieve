package indexer

import (
	"strings"
)

// extractSymbolsHeuristic extracts symbols from non-Go source files
// using line-by-line pattern matching. Used as a fallback when no
// Wasm parser (ParserManager) is configured.
func extractSymbolsHeuristic(ext, content string) []Symbol {
	switch ext {
	case ".py":
		return extractPythonSymbols(content)
	case ".ts", ".tsx", ".js", ".jsx":
		return extractTSSymbols(content)
	case ".rs":
		return extractRustSymbols(content)
	default:
		return nil
	}
}

// extractPythonSymbols extracts def/class declarations including decorators.
// It also tracks unqualified function calls inside each def body for Calls edges.
func extractPythonSymbols(content string) []Symbol {
	lines := strings.Split(content, "\n")
	var syms []Symbol
	var pending []string // accumulated decorator lines

	// first pass: collect symbol definitions and their start lines
	type defEntry struct {
		name    string
		symType string
		sig     string
		line    int
		indent  int
	}
	var defs []defEntry

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if strings.HasPrefix(trimmed, "@") {
			pending = append(pending, trimmed)
			continue
		}
		if strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "class(") {
			name := extractPythonName(trimmed, "class")
			if name != "" {
				sig := buildSig(pending, trimmed)
				defs = append(defs, defEntry{name: name, symType: "class", sig: sig, line: i + 1, indent: indent})
			}
			pending = nil
			continue
		}
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ") {
			prefix := "def"
			if strings.HasPrefix(trimmed, "async ") {
				prefix = "async def"
			}
			name := extractPythonName(trimmed, prefix)
			if name != "" {
				sig := buildSig(pending, trimmed)
				defs = append(defs, defEntry{name: name, symType: "function", sig: sig, line: i + 1, indent: indent})
			}
			pending = nil
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pending = nil
		}
	}

	// second pass: for each def, collect body lines using indent-based scope.
	// Using next-def line as boundary was broken for nested classes; indent is authoritative.
	for _, d := range defs {
		var bodyLines []string
		for li := d.line; li < len(lines); li++ {
			line := lines[li]
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				bodyLines = append(bodyLines, line)
				continue
			}
			lineIndent := len(line) - len(strings.TrimLeft(line, " \t"))
			if lineIndent <= d.indent {
				break
			}
			bodyLines = append(bodyLines, line)
		}
		calls := extractPythonCalls(bodyLines)
		syms = append(syms, Symbol{
			Name:    d.name,
			Type:    d.symType,
			Line:    d.line,
			Content: d.sig,
			Calls:   calls,
		})
	}
	return syms
}

// extractPythonCalls scans body lines for bare function call names (unqualified).
func extractPythonCalls(lines []string) []string {
	seen := map[string]bool{}
	var calls []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Skip definition lines — the `name(` pattern would otherwise look like a call.
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ") ||
			strings.HasPrefix(trimmed, "class ") {
			continue
		}
		// Find patterns: identifier( at start of expression or after assignment
		// Simple heuristic: look for word( patterns
		rest := trimmed
		for {
			idx := strings.Index(rest, "(")
			if idx <= 0 {
				break
			}
			// Extract word before '('
			word := extractCallName(rest[:idx])
			rest = rest[idx+1:]
			if word == "" || strings.Contains(word, ".") {
				continue // qualified or empty
			}
			// Skip keywords
			if isPythonKeyword(word) {
				continue
			}
			if !seen[word] {
				seen[word] = true
				calls = append(calls, word)
			}
		}
	}
	return calls
}

func extractCallName(s string) string {
	s = strings.TrimSpace(s)
	// Walk backwards from end to find the identifier
	end := len(s)
	for i := end - 1; i >= 0; i-- {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return strings.TrimSpace(s[i+1 : end])
	}
	return strings.TrimSpace(s)
}

var pythonKeywords = map[string]bool{
	"if": true, "else": true, "elif": true, "while": true, "for": true,
	"with": true, "try": true, "except": true, "finally": true,
	"return": true, "yield": true, "raise": true, "assert": true,
	"print": true, "super": true, "isinstance": true, "type": true,
	"len": true, "range": true, "list": true, "dict": true, "set": true,
	"tuple": true, "str": true, "int": true, "float": true, "bool": true,
	"open": true, "vars": true, "dir": true, "hasattr": true, "getattr": true,
}

func isPythonKeyword(s string) bool { return pythonKeywords[s] }

func extractPythonName(line, prefix string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	// name ends at '(' or ':'
	for i, c := range rest {
		if c == '(' || c == ':' {
			return strings.TrimSpace(rest[:i])
		}
	}
	return strings.TrimSpace(rest)
}

func buildSig(decorators []string, decl string) string {
	if len(decorators) == 0 {
		return decl
	}
	return strings.Join(decorators, "\n") + "\n" + decl
}

// extractTSSymbols extracts functions, classes, interfaces, type aliases,
// and exported arrow-function constants from TypeScript/JavaScript.
func extractTSSymbols(content string) []Symbol {
	lines := strings.Split(content, "\n")
	var syms []Symbol

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Strip leading export/export default/declare
		normalized := trimmed
		for _, pfx := range []string{"export default ", "export declare ", "declare ", "export "} {
			if after, ok := strings.CutPrefix(normalized, pfx); ok {
				normalized = after
			}
		}

		// function declaration: function name(
		if strings.HasPrefix(normalized, "function ") || strings.HasPrefix(normalized, "async function ") {
			name := extractTSFuncName(normalized)
			if name != "" {
				syms = append(syms, Symbol{Name: name, Type: "function", Line: i + 1, Content: trimmed})
			}
			continue
		}

		// class declaration
		if strings.HasPrefix(normalized, "class ") || strings.HasPrefix(normalized, "abstract class ") {
			name := extractTSClassName(normalized)
			if name != "" {
				syms = append(syms, Symbol{Name: name, Type: "class", Line: i + 1, Content: trimmed})
			}
			continue
		}

		// interface declaration
		if strings.HasPrefix(normalized, "interface ") {
			name := extractTSSimpleName(normalized, "interface ")
			if name != "" {
				syms = append(syms, Symbol{Name: name, Type: "interface", Line: i + 1, Content: trimmed})
			}
			continue
		}

		// type alias
		if strings.HasPrefix(normalized, "type ") && strings.Contains(normalized, "=") {
			name := extractTSSimpleName(normalized, "type ")
			if name != "" {
				syms = append(syms, Symbol{Name: name, Type: "type", Line: i + 1, Content: trimmed})
			}
			continue
		}

		// const/let arrow function: const Foo = ( or const Foo: React.FC =
		if strings.HasPrefix(normalized, "const ") || strings.HasPrefix(normalized, "let ") {
			if strings.Contains(normalized, "=>") || strings.Contains(normalized, "= (") || strings.Contains(normalized, "= async") {
				name := extractTSConstName(normalized)
				if name != "" {
					syms = append(syms, Symbol{Name: name, Type: "function", Line: i + 1, Content: trimmed})
				}
			}
		}
	}
	return syms
}

func extractTSFuncName(line string) string {
	line = strings.TrimPrefix(line, "async ")
	line = strings.TrimPrefix(line, "function ")
	if idx := strings.IndexAny(line, "(<"); idx > 0 {
		return strings.TrimSpace(line[:idx])
	}
	return ""
}

func extractTSClassName(line string) string {
	line = strings.TrimPrefix(line, "abstract ")
	line = strings.TrimPrefix(line, "class ")
	line = strings.TrimSpace(line)
	if idx := strings.IndexAny(line, " {<("); idx > 0 {
		return strings.TrimSpace(line[:idx])
	}
	return strings.TrimSpace(line)
}

func extractTSSimpleName(line, prefix string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if idx := strings.IndexAny(rest, " <=({"); idx > 0 {
		return strings.TrimSpace(rest[:idx])
	}
	return ""
}

func extractTSConstName(line string) string {
	line = strings.TrimPrefix(line, "const ")
	line = strings.TrimPrefix(line, "let ")
	line = strings.TrimSpace(line)
	if idx := strings.IndexAny(line, " :="); idx > 0 {
		return strings.TrimSpace(line[:idx])
	}
	return ""
}

// extractRustSymbols extracts fn, struct, enum, trait, impl declarations.
func extractRustSymbols(content string) []Symbol {
	lines := strings.Split(content, "\n")
	var syms []Symbol

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Strip pub/pub(crate) visibility
		normalized := trimmed
		for _, pfx := range []string{"pub(crate) ", "pub(super) ", "pub "} {
			if strings.HasPrefix(normalized, pfx) {
				normalized = strings.TrimPrefix(normalized, pfx)
				break
			}
		}

		var symType, keyword string
		switch {
		case strings.HasPrefix(normalized, "fn ") || strings.HasPrefix(normalized, "async fn "):
			symType, keyword = "function", "fn "
			if after, ok := strings.CutPrefix(normalized, "async "); ok {
				normalized = after
			}
		case strings.HasPrefix(normalized, "struct "):
			symType, keyword = "struct", "struct "
		case strings.HasPrefix(normalized, "enum "):
			symType, keyword = "enum", "enum "
		case strings.HasPrefix(normalized, "trait "):
			symType, keyword = "trait", "trait "
		case strings.HasPrefix(normalized, "impl "):
			symType, keyword = "impl", "impl "
		default:
			continue
		}

		rest := strings.TrimPrefix(normalized, keyword)
		name := ""
		if idx := strings.IndexAny(rest, " <({"); idx > 0 {
			name = strings.TrimSpace(rest[:idx])
		} else {
			name = strings.TrimSpace(rest)
		}
		if name != "" {
			syms = append(syms, Symbol{Name: name, Type: symType, Line: i + 1, Content: trimmed})
		}
	}
	return syms
}
