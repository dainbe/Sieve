package context

import (
	"fmt"
	"path/filepath"
	"sort"
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
		branches = append(branches, Branch{
			Path:      dir,
			FileCount: stat.total,
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
