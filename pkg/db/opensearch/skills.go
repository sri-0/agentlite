package opensearch

import (
	"context"
	"fmt"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// IndexSkills is the skills library: one document per skill, its name as id,
// with every file of the skill in it.
const IndexSkills = "skills"

// Skill is a skill of the library: its name and description, from its
// SKILL.md, and its files, SKILL.md among them.
type Skill struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Files       []SkillFile `json:"files"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// SkillFile is one file of a skill.
type SkillFile struct {
	Path    string `json:"path"`              // relative to the skill: SKILL.md, references/guide.md
	Content []byte `json:"content,omitempty"` // base64 in the document
}

const skillsMapping = `{"mappings":{"properties":{
	"name":{"type":"keyword"},"description":{"type":"text"},
	"files":{"type":"object","enabled":false},"updated_at":{"type":"date"}}}}`

// SaveSkill stores a skill, replacing any of the same name.
func (c *Client) SaveSkill(ctx context.Context, s *Skill) error {
	return c.index(ctx, IndexSkills, s.Name, s)
}

// Skill fetches a skill with its files.
func (c *Client) Skill(ctx context.Context, name string) (*Skill, error) {
	return get[Skill](ctx, c, IndexSkills, name)
}

// Skills lists the library by name, each skill's files without their content.
func (c *Client) Skills(ctx context.Context) ([]Skill, error) {
	q := fmt.Sprintf(`{"size":%d,"sort":[{"name":"asc"}],"_source":{"excludes":["files.content"]}}`, listCap)
	return search[Skill](ctx, c, IndexSkills, q)
}

// DeleteSkill removes a skill; ErrNotFound when there is none.
func (c *Client) DeleteSkill(ctx context.Context, name string) error {
	_, err := c.api.Document.Delete(ctx, opensearchapi.DeleteReq{
		Index: IndexSkills, ID: name, Params: &opensearchapi.DeleteParams{Refresh: "true"},
	})
	switch {
	case errStatus(err) == 404:
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("delete %s/%s: %w", IndexSkills, name, err)
	}
	return nil
}
