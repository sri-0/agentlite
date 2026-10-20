package opensearch

import (
	"context"
	"maps"
	"slices"
	"strings"
)

// indexSkills is the skills library: one document per skill, a uuid its id.
// The files themselves are in S3.
const indexSkills = "skills"

// Skill is a skill of the library: its name and description, from its
// SKILL.md, the title, category and source the library adds, and its files,
// SKILL.md among them.
type Skill struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"` // unique in the library
	Title       string      `json:"title,omitempty"`
	Description string      `json:"description"`
	Category    string      `json:"category,omitempty"` // free-form, e.g. Writing
	Source      string      `json:"source,omitempty"`   // where it comes from, e.g. Internal, Open source
	Files       []SkillFile `json:"files"`
	Marks
}

// SkillFile is one file of a skill.
type SkillFile struct {
	Path string `json:"path"` // relative to the skill: SKILL.md, references/guide.md
	Size int    `json:"size"`
}

// SkillFilter narrows a listing of the library: Q searches names, titles and
// descriptions; Category and Source match exactly. Empty fields match all.
type SkillFilter struct {
	Q, Category, Source string
}

// Facet is one value of a category or source, with how many skills have it.
type Facet struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// SkillList is a listing of the library, and the categories and sources of
// the skills Q matches, whatever the Category and Source chosen.
type SkillList struct {
	Skills     []Skill
	Categories []Facet
	Sources    []Facet
}

// SaveSkill stores a skill under its id.
func (c *Client) SaveSkill(ctx context.Context, s *Skill) error {
	return c.index(ctx, indexSkills, s.ID, s)
}

// Skill fetches a skill by id.
func (c *Client) Skill(ctx context.Context, id string) (*Skill, error) {
	return get[Skill](ctx, c, indexSkills, id)
}

// SkillNamed fetches the skill with a name; ErrNotFound when there is none.
func (c *Client) SkillNamed(ctx context.Context, name string) (*Skill, error) {
	found, err := search[Skill](ctx, c, indexSkills, map[string]any{"size": 1, "query": map[string]any{"term": map[string]any{"name": name}}})
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return &found[0], nil
}

// AllSkills lists the whole library by name, without counts: what agents and
// npx skills see. Fields, when given, are the only ones fetched.
func (c *Client) AllSkills(ctx context.Context, fields ...string) ([]Skill, error) {
	q := map[string]any{"size": listCap, "sort": []any{map[string]any{"name": "asc"}}}
	if len(fields) > 0 {
		q["_source"] = fields
	}
	return search[Skill](ctx, c, indexSkills, q)
}

// Skills lists the library: by name, or best match first when Q is set
// (every word must match one field). The category and source counts are
// each under the other choice only: a category's count follows the chosen
// source, the source counts follow the chosen category, so every count is
// what choosing it would show. The library is small (listCap), so they are
// counted here from every skill Q matches.
func (c *Client) Skills(ctx context.Context, f SkillFilter) (*SkillList, error) {
	body := map[string]any{"size": listCap, "sort": []any{map[string]any{"name": "asc"}}}
	if q := strings.TrimSpace(f.Q); q != "" {
		body["query"] = map[string]any{"multi_match": map[string]any{
			"query": q, "fields": []string{"name", "title^2", "description"}, "type": "bool_prefix", "operator": "and",
		}}
		body["sort"] = []any{"_score", map[string]any{"name": "asc"}}
	}
	matched, err := search[Skill](ctx, c, indexSkills, body)
	if err != nil {
		return nil, err
	}
	out := &SkillList{}
	categories, sources := map[string]int64{}, map[string]int64{}
	for _, s := range matched {
		inCategory := f.Category == "" || s.Category == f.Category
		inSource := f.Source == "" || s.Source == f.Source
		if inSource && s.Category != "" {
			categories[s.Category]++
		}
		if inCategory && s.Source != "" {
			sources[s.Source]++
		}
		if inCategory && inSource {
			out.Skills = append(out.Skills, s)
		}
	}
	out.Categories, out.Sources = facets(categories), facets(sources)
	return out, nil
}

// facets lists counted values in order.
func facets(counts map[string]int64) []Facet {
	out := make([]Facet, 0, len(counts))
	for _, v := range slices.Sorted(maps.Keys(counts)) {
		out = append(out, Facet{Value: v, Count: counts[v]})
	}
	return out
}

// DeleteSkill removes a skill the caller created; ErrNotFound when there is
// none they can see.
func (c *Client) DeleteSkill(ctx context.Context, id string) error {
	return c.delete(ctx, indexSkills, id)
}
