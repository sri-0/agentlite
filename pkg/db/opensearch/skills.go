package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// IndexSkills is the skills library: one document per skill, a uuid its id.
// The files themselves are in S3.
const IndexSkills = "skills"

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
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// SkillFile is one file of a skill.
type SkillFile struct {
	Path string `json:"path"` // relative to the skill: SKILL.md, references/guide.md
	Size int    `json:"size"`
}

const skillsMapping = `{"mappings":{"properties":{
	"id":{"type":"keyword"},"name":{"type":"keyword"},"title":{"type":"text"},"description":{"type":"text"},
	"category":{"type":"keyword"},"source":{"type":"keyword"},
	"files":{"type":"object","enabled":false},"created_at":{"type":"date"},"updated_at":{"type":"date"}}}}`

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
	return c.index(ctx, IndexSkills, s.ID, s)
}

// Skill fetches a skill by id.
func (c *Client) Skill(ctx context.Context, id string) (*Skill, error) {
	return get[Skill](ctx, c, IndexSkills, id)
}

// SkillNamed fetches the skill with a name; ErrNotFound when there is none.
func (c *Client) SkillNamed(ctx context.Context, name string) (*Skill, error) {
	found, err := search[Skill](ctx, c, IndexSkills, fmt.Sprintf(`{"size":1,"query":{"term":{"name":%q}}}`, name))
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return &found[0], nil
}

// Skills lists the library: by name, or best match first when Q is set
// (every word must match). The
// category and source counts come from the same search, each under the other
// choice only: a category's count follows the chosen source, the source counts
// follow the chosen category, so every count is what choosing it would show.
func (c *Client) Skills(ctx context.Context, f SkillFilter) (*SkillList, error) {
	query := map[string]any{"match_all": map[string]any{}}
	sort := []any{map[string]any{"name": "asc"}}
	if q := strings.TrimSpace(f.Q); q != "" {
		query = map[string]any{"multi_match": map[string]any{
			"query": q, "fields": []string{"name", "title^2", "description"}, "type": "bool_prefix", "operator": "and",
		}}
		sort = append([]any{"_score"}, sort...)
	}
	category, source := term("category", f.Category), term("source", f.Source)
	body := map[string]any{
		"size": listCap, "sort": sort, "query": query,
		"aggs": map[string]any{"categories": facet("category", source), "sources": facet("source", category)},
	}
	if chosen := both(category, source); chosen != nil {
		body["post_filter"] = chosen
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := &opensearchapi.SearchReq{Indices: []string{IndexSkills}, BodyReader: bytes.NewReader(raw)}
	resp, err := c.api.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", IndexSkills, err)
	}
	out := &SkillList{}
	if out.Skills, err = hits[Skill](IndexSkills, resp); err != nil {
		return nil, err
	}
	if out.Categories, err = facets(resp, "categories"); err != nil {
		return nil, err
	}
	if out.Sources, err = facets(resp, "sources"); err != nil {
		return nil, err
	}
	return out, nil
}

// term matches a keyword field's value; nil for no value.
func term(field, value string) map[string]any {
	if value == "" {
		return nil
	}
	return map[string]any{"term": map[string]any{field: value}}
}

// both is the filters given, together; nil for none.
func both(filters ...map[string]any) map[string]any {
	var set []any
	for _, f := range filters {
		if f != nil {
			set = append(set, f)
		}
	}
	if len(set) == 0 {
		return nil
	}
	return map[string]any{"bool": map[string]any{"filter": set}}
}

// facet counts a field's values among the skills the other filter keeps.
func facet(field string, other map[string]any) map[string]any {
	if other == nil {
		other = map[string]any{"match_all": map[string]any{}}
	}
	terms := map[string]any{"field": field, "size": 100, "order": map[string]any{"_key": "asc"}}
	return map[string]any{"filter": other, "aggs": map[string]any{"values": map[string]any{"terms": terms}}}
}

// facets reads a facet's counts.
func facets(resp *opensearchapi.SearchResp, name string) ([]Facet, error) {
	var agg struct {
		Values struct {
			Buckets []struct {
				Key      string `json:"key"`
				DocCount int64  `json:"doc_count"`
			} `json:"buckets"`
		} `json:"values"`
	}
	a := resp.Aggregations[name]
	if err := json.Unmarshal(a.RawJSON(), &agg); err != nil {
		return nil, fmt.Errorf("search %s: %s: %w", IndexSkills, name, err)
	}
	out := make([]Facet, len(agg.Values.Buckets))
	for i, b := range agg.Values.Buckets {
		out[i] = Facet{Value: b.Key, Count: b.DocCount}
	}
	return out, nil
}

// DeleteSkill removes a skill; ErrNotFound when there is none.
func (c *Client) DeleteSkill(ctx context.Context, id string) error {
	_, err := c.api.Document.Delete(ctx, opensearchapi.DeleteReq{
		Index: IndexSkills, ID: id, Params: &opensearchapi.DeleteParams{Refresh: "true"},
	})
	switch {
	case errStatus(err) == 404:
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("delete %s/%s: %w", IndexSkills, id, err)
	}
	return nil
}
