//go:build eval

package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Case is a single evaluation query with expected relevant files.
type Case struct {
	ID             string   `json:"id"`
	Query          string   `json:"query"`
	GroundTruth []string `json:"ground_truth_files"`
	Notes       string   `json:"notes,omitempty"`
}

// LoadCases reads all *.json files from dir and returns the cases they contain.
func LoadCases(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read eval cases dir %q: %w", dir, err)
	}
	var cases []Case
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read case %s: %w", e.Name(), err)
		}
		var batch []Case
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, fmt.Errorf("parse case %s: %w", e.Name(), err)
		}
		cases = append(cases, batch...)
	}
	return cases, nil
}
