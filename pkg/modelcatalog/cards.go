package modelcatalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/adrg/frontmatter"
	"github.com/goccy/go-yaml"
)

// Cards are model cards by lower-case id and alias, and by the id's last
// segment for providers that drop the vendor prefix (LiteLLM, vLLM). A card
// is offline context about one model, shared by every provider that serves
// it: one Markdown file per model, YAML frontmatter with the Model fields,
// then notes. Its path mirrors its id (anthropic/claude-sonnet-4.5.md).
type Cards map[string]Model

var yamlFrontmatter = frontmatter.NewFormat("---", "---", yaml.Unmarshal)

// LoadCards reads every card under dir. A card that cannot be read is left
// out, and bad joins the reasons; the cards that loaded are still returned.
func LoadCards(dir string) (cards Cards, bad error) {
	var loaded []Model
	var errs []error
	err := filepath.WalkDir(dir, func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(file) != ".md" || strings.EqualFold(d.Name(), "README.md") {
			return err
		}
		card, err := readCard(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			return nil
		}
		loaded = append(loaded, card)
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	cards = Cards{}
	for _, card := range loaded {
		for _, key := range append([]string{card.ID}, card.Aliases...) {
			cards[strings.ToLower(key)] = card
		}
	}
	// A last segment finds a card only when no id or alias is that segment
	// and one card alone ends with it: which vendor's card a bare name means
	// is otherwise a guess.
	bases := map[string][]Model{}
	for _, card := range loaded {
		base := strings.ToLower(path.Base(card.ID))
		bases[base] = append(bases[base], card)
	}
	for base, same := range bases {
		if _, taken := cards[base]; !taken && len(same) == 1 {
			cards[base] = same[0]
		}
	}
	return cards, errors.Join(errs...)
}

func readCard(file string) (Model, error) {
	f, err := os.Open(file)
	if err != nil {
		return Model{}, err
	}
	defer f.Close()
	var card Model
	body, err := frontmatter.MustParse(f, &card, yamlFrontmatter)
	if err != nil {
		return Model{}, fmt.Errorf("frontmatter: %w", err)
	}
	if card.ID == "" {
		return Model{}, errors.New("missing id")
	}
	// Where a model's requests go is models.yaml's to say: a card comes from
	// another repository and is shared by every provider of the model.
	card.BaseURL, card.API = "", ""
	card.Notes = strings.TrimSpace(string(body))
	return card, nil
}

// For finds the card of a model by its upstream id, or by the id's last
// segment (a provider's "gpt-4.1-mini" or "azure/gpt-4.1-mini" finds
// openai/gpt-4.1-mini).
func (c Cards) For(upstreamID string) (Model, bool) {
	id := strings.ToLower(upstreamID)
	if card, ok := c[id]; ok {
		return card, true
	}
	card, ok := c[path.Base(id)]
	return card, ok
}
