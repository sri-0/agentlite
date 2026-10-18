package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"agentlite/internal/refs"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
	"agentlite/pkg/toolx"
)

// What a #reference puts in the model's context as it is: a text of up to
// inlineText characters, an image or PDF of up to inlineBytes. A bigger file
// gets a note, and is searched instead. Each part stays in the conversation's
// session, so a file a provider refuses would break every later turn.
const (
	inlineText  = 20_000
	inlineBytes = 10 << 20
)

// activity is the file activity's content: the file a #id named.
type activity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
}

var _ refs.Resolver = (*Store)(nil)

// Resolve resolves #id, a file's id, to what reference says the model gets,
// and a file activity. Only a file of the conversation's workspace (toolx.Run
// in ctx) resolves; any other #word stays text.
func (s *Store) Resolve(ctx context.Context, id string) (refs.Ref, bool, error) {
	rootID := toolx.RunFrom(ctx).Thread.RootID
	if uuid.Validate(id) != nil || rootID == "" {
		return refs.Ref{}, false, nil
	}
	f, err := s.records.File(ctx, id)
	if errors.Is(err, opensearch.ErrNotFound) {
		return refs.Ref{}, false, nil
	}
	if err != nil {
		return refs.Ref{}, false, err
	}
	root, err := s.records.Thread(ctx, rootID)
	if err != nil {
		return refs.Ref{}, false, err
	}
	if f.WorkspaceID != root.WorkspaceID {
		return refs.Ref{}, false, nil
	}
	if !inline(f) {
		return reference(f, nil), true, nil
	}
	body, err := s.Body(ctx, f)
	if errors.Is(err, s3.ErrNotFound) {
		return refs.Ref{}, false, nil
	}
	if err != nil {
		return refs.Ref{}, false, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return refs.Ref{}, false, err
	}
	return reference(f, data), true, nil
}

// inline reports whether a file may go in the model's context as it is, so
// its body is read: an image or PDF within inlineBytes, a text that may be
// within inlineText characters.
func inline(f *opensearch.File) bool {
	switch kind(f.MediaType) {
	case kindImage, kindPDF:
		return f.Size <= inlineBytes
	case kindText:
		return f.Size <= inlineText*utf8.UTFMax
	}
	return false
}

// reference is what the model gets for a file, named by its name so that the
// answer is too: an image or a PDF as it is, a text of up to inlineText
// characters inline, and anything else (a bigger file, audio, video) as a
// note. body is nil when it was not read.
func reference(f *opensearch.File, body []byte) refs.Ref {
	ref := refs.Ref{Kind: stream.ActivityFile, ID: f.ID, Activity: activity{ID: f.ID, Name: f.Name, MediaType: f.MediaType}}
	named := fmt.Sprintf("File %q (%s), which the message calls #%s; call it %q, never by its id.",
		f.Name, base(f.MediaType), f.ID, f.Name)
	switch k := kind(f.MediaType); {
	case body != nil && (k == kindImage || k == kindPDF):
		ref.Content = named + " It is attached next."
		ref.Part = genai.NewPartFromBytes(body, base(f.MediaType))
	case body != nil && k == kindText && utf8.RuneCount(body) <= inlineText:
		ref.Content = fmt.Sprintf("%s\n<file name=%q>\n%s\n</file>", named, f.Name, body)
	case f.Status == opensearch.FileReady:
		ref.Content = fmt.Sprintf("%s It is %d bytes, too long or not a kind you can read here: search it with "+
			"search_userfiles, files [%q].", named, f.Size, f.ID)
	default:
		ref.Content = fmt.Sprintf("%s It is %d bytes, and can't be read or searched here.", named, f.Size)
	}
	return ref
}
