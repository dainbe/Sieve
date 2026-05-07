package context

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// initBranches replaces the placeholder in builder.go.
// Called at the end of Build() to populate branches.
func (b *Builder) computeBranches(includedIDs map[string]bool) []Branch {
	allIDs, err := b.store.GetAllFileNodeIDs()
	if err != nil {
		return nil
	}

	type dirStat struct {
		total   int
		covered int
		files   []string // sampled file IDs for summary generation
	}
	dirs := map[string]*dirStat{}

	for _, id := range allIDs {
		dir := filepath.Dir(id)
		if dir == "." {
			dir = "(root)"
		}
		if dirs[dir] == nil {
			dirs[dir] = &dirStat{}
		}
		dirs[dir].total++
		if len(dirs[dir].files) < 5 {
			dirs[dir].files = append(dirs[dir].files, id)
		}
		if includedIDs[id] {
			dirs[dir].covered++
		}
	}

	var branches []Branch
	for dir, stat := range dirs {
		uncovered := stat.total - stat.covered
		if uncovered == 0 {
			continue // fully covered — no need to drill down
		}
		hint := fmt.Sprintf("%d file(s) not yet in context", uncovered)
		summary := dirSummary(dir, stat.files)
		branches = append(branches, Branch{
			Path:      dir,
			FileCount: stat.total,
			Summary:   summary,
			Hint:      hint,
		})
	}

	sort.Slice(branches, func(i, j int) bool {
		return branches[i].FileCount > branches[j].FileCount
	})
	if len(branches) > 8 {
		branches = branches[:8]
	}
	return branches
}

// dirSummary generates a one-line description of a directory from its file names.
// This mirrors PageIndex's node description: what lives here, at a glance.
func dirSummary(dir string, files []string) string {
	if len(files) == 0 {
		return dir
	}
	// Collect base names without extension
	names := make([]string, 0, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		ext := filepath.Ext(base)
		names = append(names, strings.TrimSuffix(base, ext))
	}
	joined := strings.Join(names, ", ")
	if len(files) < 5 {
		return joined
	}
	return joined + ", …"
}
