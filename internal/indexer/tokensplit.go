package indexer

import (
	"path/filepath"
	"strings"

	"github.com/dainbe/Sieve/internal/store"
)

// SplitIdentifiers extracts vocabulary tokens from text by splitting camelCase,
// PascalCase, acronyms, snake_case, kebab-case, dots, slashes, and digits-as-
// boundaries. Returns lowercase tokens ≥3 chars, deduplicated.
func SplitIdentifiers(text string) []string {
	seen := map[string]bool{}
	var result []string

	add := func(s string) {
		s = strings.ToLower(s)
		if len(s) < 3 || seen[s] {
			return
		}
		seen[s] = true
		result = append(result, s)
	}

	runes := []rune(text)
	start := -1

	flushWord := func(end int) {
		if start < 0 || end <= start {
			return
		}
		for _, part := range splitCamelRunes(runes[start:end]) {
			add(part)
		}
		start = -1
	}

	for i, r := range runes {
		if isIdentChar(r) {
			if start < 0 {
				start = i
			}
		} else {
			flushWord(i)
		}
	}
	flushWord(len(runes))

	return result
}

// augmentContent appends a block of identifier-split tokens after the sentinel.
// FTS5 indexes the augmented content; callers must strip it on read via
// store.StripAugment so users never see the extra tokens.
func augmentContent(content, path string) string {
	tokens := SplitIdentifiers(content)

	// Also include tokens derived from the filename stem (e.g. "heuristic" from "heuristic.go").
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	stemTokens := SplitIdentifiers(stem)
	seen := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		seen[t] = true
	}
	for _, t := range stemTokens {
		if !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		return content
	}
	return content + store.FTSAugmentSentinel + strings.Join(tokens, "\n")
}

// splitCamelRunes splits a single identifier word (no separators) on camelCase
// and PascalCase boundaries, including acronym-to-word transitions like "FTSSearch".
func splitCamelRunes(rs []rune) []string {
	if len(rs) == 0 {
		return nil
	}
	var parts []string
	segStart := 0
	for i := 1; i < len(rs); i++ {
		cur := rs[i]
		if !isUpperRune(cur) {
			continue
		}
		prev := rs[i-1]
		nextIsLower := i+1 < len(rs) && isLowerRune(rs[i+1])
		// Split when: previous char is lowercase (camelCase), OR
		// previous is uppercase but next is lowercase (acronym end: "FTSSearch" → "FTS"+"Search").
		if isLowerRune(prev) || (isUpperRune(prev) && nextIsLower) {
			parts = append(parts, string(rs[segStart:i]))
			segStart = i
		}
	}
	parts = append(parts, string(rs[segStart:]))
	return parts
}

func isIdentChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isUpperRune(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLowerRune(r rune) bool { return r >= 'a' && r <= 'z' }
