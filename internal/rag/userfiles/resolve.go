package userfiles

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"agentlite/internal/refs"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// activity is the file activity's content: the file a #id named.
type activity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
}

var _ refs.Resolver = (*Store)(nil)

// Resolve resolves #id, any file's id (the user picked it, from any
// workspace), to a file activity and what the model gets: a line naming the
// file, then an image or PDF as it is or a text inline, when small enough
// (files config); anything else stays at the line. Any other #word stays text.
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
	ref := refs.Ref{
		Kind: stream.ActivityFile, ID: f.ID, Activity: activity{ID: f.ID, Name: f.Name, MediaType: f.MediaType},
		Content: fmt.Sprintf("The message names the file %q (%s, %d bytes) by its id; call it %q, never by its id.",
			f.Name, f.MediaType, f.Size, f.Name),
	}
	k := kind(f.MediaType)
	text := k == kindText && f.Size <= s.limits.InlineTextKB<<10
	media := (k == kindImage || k == kindPDF) && f.Size <= s.limits.InlineMediaMB<<20
	switch {
	case text || media:
		body, err := s.Body(ctx, f)
		if errors.Is(err, s3.ErrNotFound) {
			return refs.Ref{}, false, nil // its body is gone: the word stays text
		}
		if err != nil {
			return refs.Ref{}, false, err
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		if err != nil {
			return refs.Ref{}, false, err
		}
		if text {
			ref.Content += fmt.Sprintf("\n<file name=%q>\n%s\n</file>", f.Name, data)
		} else {
			ref.Part = genai.NewPartFromBytes(data, f.MediaType)
		}
	case f.Status == opensearch.FileReady:
		ref.Content += fmt.Sprintf(" It is too big or not a kind you can read here; search it with search_userfiles, files [%q].", f.ID)
	default:
		ref.Content += " It is too big or not a kind you can read here."
	}
	return ref, true, nil
}
