package opensearch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"golang.org/x/oauth2"
)

// Caller is who the store's calls are made for. There is no service
// identity: every call carries Caller's bearer token, and OpenSearch's
// document-level security limits it to the token's control sets. The store
// confines every read and write to ControlSets as well, so a run, whose
// ControlSets are its thread's one, never reads another control set's
// documents even when its user may.
type Caller struct {
	Token       oauth2.TokenSource // the user's own token, or, inside a run, the thread token
	Username    string             // preferred_username: user_created_by and user_updated_by
	ControlSets []string           // what reads and writes are confined to
	// ControlSet labels a new document that names none: the request's
	// X-Control-Set, or the run's thread's.
	ControlSet string
}

type callerKey struct{}

// WithCaller returns ctx whose store calls are made for c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns who ctx's store calls are made for.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok && c.Token != nil
}

// Errors for a call made without a caller (a bug: it is never sent), a
// write or change the caller may not make, and a new document that has no
// control set.
var (
	ErrNoCaller     = errors.New("opensearch: no user token in the context")
	ErrForbidden    = errors.New("forbidden")
	ErrNoControlSet = errors.New("no control set")
)

// Labels are the fields every document carries for document-level security
// and ownership: its control set, and who created and last changed it. The
// store sets them on every write (stamp).
type Labels struct {
	ControlSetID  string `json:"controlset_id,omitempty"`
	UserCreatedBy string `json:"user_created_by,omitempty"`
	UserUpdatedBy string `json:"user_updated_by,omitempty"`
}

func (l *Labels) labels() *Labels { return l }

// labelled is a document with Labels.
type labelled interface{ labels() *Labels }

// stamp labels a document about to be written: a new one gets the caller's
// control set and is created by them, and every write is updated by them. A
// label outside the caller's control sets is refused: OpenSearch stores any
// label it is given.
func stamp(ctx context.Context, l *Labels) error {
	c, ok := CallerFrom(ctx)
	if !ok {
		return ErrNoCaller
	}
	l.ControlSetID = cmp.Or(l.ControlSetID, c.ControlSet)
	switch {
	case l.ControlSetID == "":
		return fmt.Errorf("%w: send X-Control-Set", ErrNoControlSet)
	case !slices.Contains(c.ControlSets, l.ControlSetID):
		return fmt.Errorf("%w: control set %s", ErrForbidden, l.ControlSetID)
	}
	l.UserCreatedBy = cmp.Or(l.UserCreatedBy, c.Username)
	l.UserUpdatedBy = c.Username
	return nil
}

// CheckOwner refuses a change to a document the caller did not create:
// deleting, renaming or editing a thread, workspace, file or skill is its
// creator's alone, whoever else may read it.
func CheckOwner(ctx context.Context, l Labels) error {
	c, ok := CallerFrom(ctx)
	if !ok || l.UserCreatedBy == "" || l.UserCreatedBy != c.Username {
		return fmt.Errorf("%w: only its creator, %s, can change it", ErrForbidden, cmp.Or(l.UserCreatedBy, "unknown"))
	}
	return nil
}

// confine ANDs the caller's control sets into a search body: into a k-NN
// query's own filter, so the k nearest are found among the allowed documents
// rather than filtered after, and around any other query.
func confine(body map[string]any, sets []string) {
	only := map[string]any{"terms": map[string]any{"controlset_id": sets}}
	q := body["query"]
	if m, ok := q.(map[string]any); ok {
		if knn, ok := m["knn"].(map[string]any); ok {
			for _, field := range knn {
				f := field.(map[string]any)
				if f["filter"] == nil {
					f["filter"] = only
				} else {
					f["filter"] = map[string]any{"bool": map[string]any{"filter": []any{f["filter"], only}}}
				}
			}
			return
		}
	}
	b := map[string]any{"filter": []any{only}}
	if q != nil {
		b["must"] = q // scored as before
	}
	body["query"] = map[string]any{"bool": b}
}

// bearer authenticates every request with its context's caller's token
// (oslib.Config.Signer, applied to each attempt). A request without a caller
// fails before it is sent: none goes out unauthenticated.
type bearer struct{}

func (bearer) SignRequest(r *http.Request) error {
	c, ok := CallerFrom(r.Context())
	if !ok {
		return ErrNoCaller
	}
	t, err := c.Token.Token()
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+t.AccessToken)
	return nil
}

func (bearer) OverrideSigningPort(uint16) {}
