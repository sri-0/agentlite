package modelcatalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

// Cards are model cards by lower-case id and alias. A card is offline
// context about one model, shared by every provider that serves it: one
// Markdown file per model, YAML frontmatter with the Model fields, then notes.
// Its path mirrors its id (anthropic/claude-sonnet-4.5.md).
type Cards map[string]Model

// LoadCards reads every card under dir. A card that cannot be read is left
// out, and bad joins the reasons; the cards that loaded are still returned.
func LoadCards(dir string) (cards Cards, bad error) {
	cards = Cards{}
	var errs []error
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" || strings.EqualFold(d.Name(), "README.md") {
			return err
		}
		card, err := readCard(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		for _, key := range append([]string{card.ID}, card.Aliases...) {
			cards[strings.ToLower(key)] = card
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return cards, errors.Join(errs...)
}

func readCard(path string) (Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Model{}, err
	}
	front, body, ok := strings.Cut(strings.TrimPrefix(string(raw), "---\n"), "\n---\n")
	if !ok || !strings.HasPrefix(string(raw), "---\n") {
		return Model{}, errors.New("missing frontmatter")
	}
	var card Model
	if err := yaml.Unmarshal([]byte(front), &card); err != nil {
		return Model{}, err
	}
	if card.ID == "" {
		return Model{}, errors.New("missing id")
	}
	card.Notes = strings.TrimSpace(body)
	return card, nil
}

// For finds the card of a model by its upstream id, or, for providers that
// drop the vendor prefix (LiteLLM, vLLM), by the id's last segment.
func (c Cards) For(upstreamID string) (Model, bool) {
	id := strings.ToLower(upstreamID)
	if card, ok := c[id]; ok {
		return card, true
	}
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		card, ok := c[id[i+1:]]
		return card, ok
	}
	return Model{}, false
}
