package skills

import (
	"context"
	"time"
)

// Index is the library as npx skills discovers it, at
// /.well-known/agent-skills/index.json (v0.1.0 of the discovery draft,
// github.com/cloudflare/agent-skills-discovery-rfc), and as opencode does
// (skills.urls): each skill with the paths of its files, which they fetch from
// <name>/<path> next to the index. Newer opencode releases download a skill
// again when its version changes.
type Index struct {
	Skills []IndexEntry `json:"skills"`
}

// IndexEntry is one skill of the index.
type IndexEntry struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Files       []string  `json:"files"`
	Version     time.Time `json:"version"` // when it last changed
}

// Index lists the library for npx skills.
func (l *Library) Index(ctx context.Context) (*Index, error) {
	all, err := l.records.AllSkills(ctx)
	if err != nil {
		return nil, err
	}
	index := &Index{Skills: make([]IndexEntry, len(all))}
	for i, s := range all {
		e := IndexEntry{Name: s.Name, Description: s.Description, Version: s.UpdatedAt}
		for _, f := range s.Files {
			e.Files = append(e.Files, f.Path)
		}
		index.Skills[i] = e
	}
	return index, nil
}
