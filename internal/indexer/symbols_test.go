package indexer

import (
	"testing"
)

func TestExtractGoSymbols_CallsEdges(t *testing.T) {
	src := `package main

func helper() string { return "ok" }

func runner() {
	result := helper()
	_ = result
	process()
}

func process() {
	helper()
}
`
	syms := extractGoSymbols(src)

	byName := map[string]Symbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}

	runner, ok := byName["runner"]
	if !ok {
		t.Fatal("expected 'runner' symbol")
	}
	if !contains(runner.Calls, "helper") {
		t.Errorf("runner.Calls should include 'helper', got %v", runner.Calls)
	}
	if !contains(runner.Calls, "process") {
		t.Errorf("runner.Calls should include 'process', got %v", runner.Calls)
	}

	// helper() calls nothing unqualified
	helperSym, ok := byName["helper"]
	if !ok {
		t.Fatal("expected 'helper' symbol")
	}
	if len(helperSym.Calls) != 0 {
		t.Errorf("helper.Calls should be empty, got %v", helperSym.Calls)
	}
}

func TestExtractGoSymbols_QualifiedCallsExcluded(t *testing.T) {
	src := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`
	syms := extractGoSymbols(src)
	for _, s := range syms {
		if s.Name == "main" {
			// fmt.Println is qualified; should not appear in Calls
			if contains(s.Calls, "fmt.Println") || contains(s.Calls, "Println") {
				t.Errorf("qualified call should not appear in Calls: %v", s.Calls)
			}
		}
	}
}

func contains(slice []string, val string) bool {
	for _, v := range slice {
		if v == val {
			return true
		}
	}
	return false
}
