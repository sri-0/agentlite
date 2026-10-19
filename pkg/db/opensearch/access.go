package opensearch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"golang.org/x/oauth2"
)

// Caller is who the store's calls are made for. There is no service
// identity: every call carries Caller's bearer token, and OpenSearch's
// document-level security limits it to the token's control sets.
type Caller struct {
	Token    oauth2.TokenSource // the user's own token, or, inside a run, the thread token
	Username string             // preferred_username: user_created_by and user_updated_by
	// Controlset labels a new document that names none: the request's
	// X-Control-Set, or the run's thread's.
	Controlset string
	// Controlsets, set inside a run, confines every read and write to the
	// thread's control set, so a run never reads another control set's
	// documents even when its user may. Nil for a user's own request, which
	// DLS alone governs.
	Controlsets []string
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
// write or change the caller may not make (or a read OpenSearch refused:
// a token without control sets), and a new document that has no control
// set.
var (
	ErrNoCaller     = errors.New("opensearch: no user token in the context")
	ErrForbidden    = errors.New("forbidden")
	ErrNoControlset = errors.New("no control set")
)

// Marks are the fields every document carries for document-level security,
// ownership and audit: its control set, who created and last changed it,
// and when. The store sets them on every write (stamp).
type Marks struct {
	ControlsetID  string    `json:"controlset_id,omitempty"`
	UserCreatedBy string    `json:"user_created_by,omitempty"`
	UserUpdatedBy string    `json:"user_updated_by,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitzero"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
}

func (m *Marks) marks() *Marks { return m }

// marked is a document with Marks.
type marked interface{ marks() *Marks }

// stamp marks a document about to be written: a new one gets the caller's
// control set, is created by them, now; every write is updated by them,
// now. Inside a run, a label outside its control set is refused: OpenSearch
// stores any label it is given. A user's own request labels only with the
// control set it named, checked against their token, or one of a document
// DLS let them read.
func stamp(ctx context.Context, m *Marks) error {
	c, ok := CallerFrom(ctx)
	if !ok {
		return ErrNoCaller
	}
	m.ControlsetID = cmp.Or(m.ControlsetID, c.Controlset)
	switch {
	case m.ControlsetID == "":
		return fmt.Errorf("%w: send X-Control-Set", ErrNoControlset)
	case c.Controlsets != nil && !slices.Contains(c.Controlsets, m.ControlsetID):
		return fmt.Errorf("%w: control set %s", ErrForbidden, m.ControlsetID)
	}
	now := time.Now()
	m.UserCreatedBy, m.UserUpdatedBy = cmp.Or(m.UserCreatedBy, c.Username), c.Username
	m.CreatedAt, m.UpdatedAt = cmp.Or(m.CreatedAt, now), now
	return nil
}

// CheckOwner refuses a change to a document the caller did not create:
// deleting, renaming or editing a thread, workspace, file or skill is its
// creator's alone, whoever else may read it.
func CheckOwner(ctx context.Context, m Marks) error {
	c, ok := CallerFrom(ctx)
	if !ok || m.UserCreatedBy == "" || m.UserCreatedBy != c.Username {
		return fmt.Errorf("%w: only its creator, %s, can change it", ErrForbidden, cmp.Or(m.UserCreatedBy, "unknown"))
	}
	return nil
}

// confine ANDs the control sets, when there are any (a run's), into a
// search body: into a k-NN query's own filter, so the k nearest are found
// among the allowed documents rather than filtered after, and around any
// other query.
func confine(body map[string]any, sets []string) {
	if sets == nil {
		return
	}
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
