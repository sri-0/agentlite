package runs

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"

	"agentlite/pkg/auth"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/logging"
)

// Tokens are the thread tokens runs call OpenSearch with (internal/sts):
// one per conversation and user, minted from the user's access token when
// none is kept.
type Tokens interface {
	Source(ctx context.Context, rootID, controlset string, user auth.Principal) (oauth2.TokenSource, error)
}

// caller is who a segment's OpenSearch calls are made for: the user who
// queued it, with the conversation's thread token for them (minted from
// their access token if none is kept), confined to the thread's control
// set. With no thread token to be had it fails with sts.ErrSignIn.
func (m *Manager) caller(ctx context.Context, run *Run, u auth.Principal) (opensearch.Caller, error) {
	if run.Controlset == "" { // queued before control sets: it could read and write nothing
		return opensearch.Caller{}, fmt.Errorf("%w: the run has no control set; start a new turn", ErrInvalid)
	}
	src, err := m.tokens.Source(ctx, run.RootID, run.Controlset, u)
	if err != nil {
		return opensearch.Caller{}, err
	}
	return opensearch.Caller{
		Token: src, Username: u.Username,
		Controlsets: []string{run.Controlset}, Controlset: run.Controlset,
	}, nil
}

// entry is what a queued segment carries of its user. The access token is
// what an executing replica that keeps no thread token mints one from; the
// entry is deleted once handled, and the token expires in minutes.
func entry(u auth.Principal, fields map[string]string) map[string]string {
	fields["user_id"], fields["username"], fields["authorization"] = u.Subject, u.Username, u.Bearer()
	return fields
}

// entryUser is the user an entry carries.
func entryUser(fields map[string]string) auth.Principal {
	token, _ := strings.CutPrefix(fields["authorization"], "Bearer ")
	return auth.Principal{Subject: fields["user_id"], Username: fields["username"], Token: token}
}

// audit starts an audit entry about a run, for its user (pkg/logging).
func (m *Manager) audit(run *Run, u auth.Principal) *zerolog.Event {
	return logging.Audit(m.logger).Str("agent_id", run.AgentID).Str("user_id", u.Subject).Str("username", u.Username).
		Str("run_id", run.ID).Str("thread_id", run.ThreadID).Str("root_id", run.RootID).Str("controlset_id", run.Controlset)
}
