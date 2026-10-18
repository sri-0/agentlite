package files

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"agentlite/internal/refs"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// inlineText is how much of a text file a #reference puts in the model's
// context, in characters; a longer file is searched instead.
const inlineText = 20_000

// activity is the file activity's content: the file a #id named.
type activity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
}

var _ refs.Resolver = (*Store)(nil)

// Resolve resolves #id, a file's id, to what reference says the model gets,
// and a file activity.
func (s *Store) Resolve(ctx context.Context, id string) (refs.Ref, bool, error) {
	if uuid.Validate(id) != nil {
		return refs.Ref{}, false, nil
	}
	f, err := s.records.File(ctx, id)
	if errors.Is(err, opensearch.ErrNotFound) {
		return refs.Ref{}, false, nil
	}
	if err != nil {
		return refs.Ref{}, false, err
	}
	var body []byte
	if k := kind(f.MediaType); k == kindImage || k == kindPDF || k == kindText && f.Size <= inlineText*utf8.UTFMax {
		if body, err = s.read(ctx, f); err != nil {
			return refs.Ref{}, false, err
		}
	}
	return reference(f, body), true, nil
}

// reference is what the model gets for a file: an image or a PDF as it is,
// a text of up to inlineText characters inline, and anything else (a longer
// text, audio, video) as a note that names it. body is nil when it was not
// read.
func reference(f *opensearch.File, body []byte) refs.Ref {
	ref := refs.Ref{Kind: stream.ActivityFile, ID: f.ID, Activity: activity{ID: f.ID, Name: f.Name, MediaType: f.MediaType}}
	switch k := kind(f.MediaType); {
	case k == kindImage || k == kindPDF:
		ref.Content = fmt.Sprintf("#%s is the file %q (%s), attached next.", f.ID, f.Name, base(f.MediaType))
		ref.Part = genai.NewPartFromBytes(body, base(f.MediaType))
	case k == kindText && body != nil && utf8.RuneCount(body) <= inlineText:
		ref.Content = fmt.Sprintf("<file id=%q name=%q>\n%s\n</file>", f.ID, f.Name, body)
	default:
		ref.Content = note(f)
	}
	return ref
}

// note tells the model about a file it cannot read here. A ready file can be
// searched.
func note(f *opensearch.File) string {
	about := fmt.Sprintf("#%s is the file %q (%s, %d bytes)", f.ID, f.Name, base(f.MediaType), f.Size)
	if f.Status != opensearch.FileReady {
		return about + "; it can't be read or searched here."
	}
	return fmt.Sprintf("%s, too long or not a kind you can read here: search it with search_userfiles, files [%q].",
		about, f.ID)
}
