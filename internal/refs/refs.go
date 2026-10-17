// Package refs reads the references a user's message makes: a word that is a
// sigil and a name, such as /name for a command (a skill, so far), and in
// time @agent for an agent and #id for a file. Each sigil has a Resolver. A
// reference that resolves gives the model content ahead of the message,
// which stays as typed, and the thread an activity that shows it.
package refs

import (
	"context"
	"strings"
	"unicode/utf8"

	"google.golang.org/genai"

	"agentlite/internal/stream"
)

// maxWords bounds the words of one message looked up as references.
const maxWords = 10

// Ref is what a reference resolves to.
type Ref struct {
	Kind     string // the activity type that shows it, e.g. stream.ActivitySkill
	ID       string // what it refers to, unique within its kind
	Content  string // what the model gets ahead of the message
	Activity any    // the activity's content
}

// A Resolver resolves the references of one sigil: ok is false when name
// refers to nothing, and the word stays plain text.
type Resolver interface {
	Resolve(ctx context.Context, name string) (ref Ref, ok bool, err error)
}

// Resolvers resolve references by sigil.
type Resolvers map[rune]Resolver

// Expand is a turn's user content: what each reference in text resolves to,
// in order and once each, then text as typed; and the activities that show
// them, their message ids <kind>:<turnID>:<id>.
func (rs Resolvers) Expand(ctx context.Context, turnID, text string) (*genai.Content, []stream.Event, error) {
	var parts []*genai.Part
	var events []stream.Event
	seen := map[string]bool{}
	for _, word := range strings.Fields(text) {
		sigil, size := utf8.DecodeRuneInString(word)
		r, ok := rs[sigil]
		if !ok || len(word) == size || seen[word] || len(seen) == maxWords {
			continue
		}
		seen[word] = true
		ref, ok, err := r.Resolve(ctx, word[size:])
		if err != nil {
			return nil, nil, err
		}
		if ok {
			parts = append(parts, genai.NewPartFromText(ref.Content))
			events = append(events, stream.Activity(ref.Kind+":"+turnID+":"+ref.ID, ref.Kind, ref.Activity))
		}
	}
	parts = append(parts, genai.NewPartFromText(text))
	return genai.NewContentFromParts(parts, genai.RoleUser), events, nil
}
