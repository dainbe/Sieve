package store

import (
	"strings"
	"testing"
)

func TestSanitizeFTS_OrAndPrefix(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantOR   bool   // result should contain " OR "
		wantStar bool   // result should contain "*" (prefix)
		wantNot  string // this substring must NOT appear in result
	}{
		{
			name:   "empty",
			input:  "",
			wantOR: false,
		},
		{
			name:     "multi-word produces OR",
			input:    "sqlite schema definition",
			wantOR:   true,
			wantStar: true,
		},
		{
			name:    "stopwords removed: and",
			input:   "schema and table",
			wantNot: `"and"`,
		},
		{
			name:    "stopwords removed: the",
			input:   "the context window",
			wantNot: `"the"`,
		},
		{
			name:   "single token no OR",
			input:  "authentication",
			wantOR: false,
		},
		{
			name:     "single token has prefix star",
			input:    "authentication",
			wantStar: true,
		},
		{
			name:    "single char token dropped",
			input:   "a b c schema",
			wantNot: `"a"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFTS(tc.input)
			if tc.input == "" && got != "" {
				t.Errorf("empty input: want empty, got %q", got)
				return
			}
			if tc.wantOR && !strings.Contains(got, " OR ") {
				t.Errorf("want OR separator in %q, got %q", tc.input, got)
			}
			if tc.wantStar && !strings.Contains(got, "*") {
				t.Errorf("want prefix * in %q, got %q", tc.input, got)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("want %q absent from result for input %q, got %q", tc.wantNot, tc.input, got)
			}
		})
	}
}

func TestSanitizeFTS_PartialMatchHits(t *testing.T) {
	// Integration-style: partial word should still produce a non-empty query.
	// "authentic" is a prefix of "authentication".
	q := sanitizeFTS("authentic handler")
	if q == "" {
		t.Fatal("expected non-empty query for partial-word input")
	}
	if !strings.Contains(q, `"authentic"*`) {
		t.Errorf("expected prefix match for 'authentic', got %q", q)
	}
}

func TestTokenizeFTS(t *testing.T) {
	t.Run("stopwords removed", func(t *testing.T) {
		tokens := TokenizeFTS("and the or schema")
		for _, tok := range tokens {
			if tok == "and" || tok == "the" || tok == "or" {
				t.Errorf("stopword %q should be removed", tok)
			}
		}
		if len(tokens) != 1 || tokens[0] != "schema" {
			t.Errorf("want [schema], got %v", tokens)
		}
	})

	t.Run("stem long tokens", func(t *testing.T) {
		// "compression" (11 chars) → trim 3 = "compress" (8 chars)
		tokens := TokenizeFTS("compression")
		if len(tokens) != 1 || tokens[0] != "compress" {
			t.Errorf("want [compress], got %v", tokens)
		}
	})

	t.Run("short token preserved", func(t *testing.T) {
		tokens := TokenizeFTS("schema")
		if len(tokens) != 1 || tokens[0] != "schema" {
			t.Errorf("want [schema], got %v", tokens)
		}
	})

	t.Run("duplicates deduplicated", func(t *testing.T) {
		tokens := TokenizeFTS("schema schema definition")
		count := 0
		for _, tok := range tokens {
			if tok == "schema" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("want schema once, got %d times in %v", count, tokens)
		}
	})

	t.Run("order preserved", func(t *testing.T) {
		tokens := TokenizeFTS("sqlite schema definition")
		if len(tokens) < 3 {
			t.Fatalf("want 3 tokens, got %v", tokens)
		}
		if tokens[0] != "sqlite" || tokens[1] != "schema" {
			t.Errorf("want sqlite schema first, got %v", tokens)
		}
	})

	t.Run("single char tokens dropped", func(t *testing.T) {
		tokens := TokenizeFTS("a b c schema")
		for _, tok := range tokens {
			if len(tok) <= 1 {
				t.Errorf("single char token %q should be dropped", tok)
			}
		}
	})
}
