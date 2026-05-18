package indexer

import (
	"log/slog"

	"github.com/dainbe/Sieve/internal/store"
)

// pendingCall is a cross-file callee reference that could not be resolved
// during pass 1 (same-file only). Resolved in pass 2 after all files are indexed.
type pendingCall struct {
	callerID   string // e.g. "pkg/a.go:RunA"
	calleeName string // unqualified callee name, e.g. "RunB"
	callerDir  string // filepath.Dir of caller file, e.g. "pkg"
}

// symbolEntry records a symbol's node ID and its directory for disambiguation.
type symbolEntry struct {
	id  string // e.g. "pkg/b.go:RunB"
	dir string // e.g. "pkg"
}

// resolveCrossFileCalls writes calls edges for pending entries where the callee
// can be unambiguously identified in nameIndex.
//
// Resolution rules (in priority order):
//  1. Exactly one same-directory candidate → emit edge.
//  2. No same-directory candidates and exactly one global candidate → emit edge.
//  3. Everything else (ambiguous or unknown) → silently skip.
func resolveCrossFileCalls(b *store.Batch, pending []pendingCall, nameIndex map[string][]symbolEntry) {
	for _, pc := range pending {
		candidates := nameIndex[pc.calleeName]
		if len(candidates) == 0 {
			continue
		}
		var sameDir []symbolEntry
		for _, c := range candidates {
			if c.dir == pc.callerDir {
				sameDir = append(sameDir, c)
			}
		}
		var target string
		switch {
		case len(sameDir) == 1:
			target = sameDir[0].id
		case len(sameDir) == 0 && len(candidates) == 1 && pc.callerDir != "":
			// Root-level callers skip global resolution to avoid false-positive cross-package edges.
			target = candidates[0].id
		default:
			continue // ambiguous or root caller with no same-dir match
		}
		if err := b.UpsertEdge(pc.callerID, target, "calls"); err != nil {
			slog.Warn("indexer: upsert cross-file calls edge failed",
				"from", pc.callerID, "to", target, "err", err)
		}
	}
}
