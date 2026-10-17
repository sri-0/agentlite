package skills

import (
	"context"

	"agentlite/pkg/db/opensearch"
)

// Index is the library as npx skills discovers it, at
// /.well-known/agent-skills/index.json (v0.1.0 of the discovery draft,
// github.com/cloudflare/agent-skills-discovery-rfc): each skill with the
// paths of its files, which it fetches from <name>/<path> next to the index.
type Index struct {
	Skills []IndexEntry `json:"skills"`
}

// IndexEntry is one skill of the index.
type IndexEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

// Index lists the library for npx skills.
func (l *Library) Index(ctx context.Context) (*Index, error) {
	list, err := l.records.Skills(ctx, opensearch.SkillFilter{})
	if err != nil {
		return nil, err
	}
	index := &Index{Skills: make([]IndexEntry, len(list.Skills))}
	for i, s := range list.Skills {
		e := IndexEntry{Name: s.Name, Description: s.Description}
		for _, f := range s.Files {
			e.Files = append(e.Files, f.Path)
		}
		index.Skills[i] = e
	}
	return index, nil
}
