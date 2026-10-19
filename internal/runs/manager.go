package runs

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
	"golang.org/x/oauth2"
	"google.golang.org/adk/v2/session"

	"agentlite/config"
	"agentlite/internal/auth"
	"agentlite/internal/hitl"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// Errors returned to callers.
var (
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid request")
	ErrBusy      = errors.New("thread has an active run")
	ErrNotPaused = errors.New("run is not waiting for input")
)

// Runtime executes one run segment, reading and writing only Valkey, and seeds
// a thread's session when it is missing.
type Runtime interface {
	Run(ctx context.Context, in Input, sink stream.Sink) (Result, error)
	// CheckModel reports whether a model id names a model its provider serves.
	CheckModel(modelID string) error
	// CheckConnectors refuses connector ids the runtime doesn't serve.
	CheckConnectors(ids []string) error
	Seed(ctx context.Context, threadID, agentID string, history []opensearch.Message) error
}

// Result is how a segment ended: its status, the answer text so far, and the
// interrupts it paused on.
type Result struct {
	Status     Status
	Text       string
	Interrupts []types.Interrupt
}

// Input is one segment: a new user message or the answers to interrupts.
type Input struct {
	Run     *Run
	Message string
	Resume  []types.ResumeEntry
	// User is who queued the segment: the user who sent the message or the
	// answers, with their access token as they sent it; connectors that
	// forward identity send it on.
	User auth.Principal
}

// History persists threads and messages (OpenSearch now, a producer later)
// and reads them back.
type History interface {
	SaveThread(ctx context.Context, t *opensearch.Thread) error
	SaveMessage(ctx context.Context, m *opensearch.Message) error
	CreateMessage(ctx context.Context, m *opensearch.Message) error
	Thread(ctx context.Context, id string) (*opensearch.Thread, error)
	Messages(ctx context.Context, threadID string, before int64, limit int) ([]opensearch.Message, error)
}

// Tokens are the thread tokens runs call OpenSearch with (internal/tokens):
// one per conversation and user, minted from subject, the user's access
// token, when none is kept.
type Tokens interface {
	Source(ctx context.Context, rootID, controlSet, userID, subject string) (oauth2.TokenSource, error)
}

// Deps are the services the run manager uses.
type Deps struct {
	DB         valkey.Client // the run lifecycle: logs, run records, leases, queues
	Interrupts *hitl.Store   // kept apart, in a database of their own
	History    History
	Sessions   session.Service
	Runtime    Runtime
	Tokens     Tokens
	Log        zerolog.Logger
}

// Manager owns the run lifecycle.
type Manager struct {
	db       valkey.Client
	store    *store
	log      *valkeyLog
	history  History
	sessions session.Service
	runtime  Runtime
	tokens   Tokens
	hitl     *hitl.Store
	defs     map[string]config.AgentDef
	appName  string
	model    string // the model a run uses when the request names none
	logger   zerolog.Logger
	workers  sync.WaitGroup
}

// New wires the run lifecycle.
func New(cfg *config.Config, d Deps) *Manager {
	return &Manager{
		db: d.DB, store: &store{db: d.DB}, log: newValkeyLog(d.DB), history: d.History, sessions: d.Sessions,
		runtime: d.Runtime, tokens: d.Tokens, hitl: d.Interrupts, defs: cfg.Defs(), appName: cfg.AppName,
		model: cfg.Models.DefaultModel, logger: d.Log,
	}
}

// SessionUser is adk's required user id. A thread's session is the
// thread's, whoever runs a turn on it, so it is the same for every user.
const SessionUser = "default"

// caller is who a segment's OpenSearch calls are made for: the user who
// queued it, with the conversation's thread token for them (minted from
// their access token if none is kept), confined to the thread's control
// set. With no thread token to be had it fails with tokens.ErrSignIn.
func (m *Manager) caller(ctx context.Context, run *Run, u auth.Principal) (opensearch.Caller, error) {
	src, err := m.tokens.Source(ctx, run.RootID, run.ControlSet, u.Subject, u.Token)
	if err != nil {
		return opensearch.Caller{}, err
	}
	return opensearch.Caller{Token: src, Username: u.Username, ControlSets: []string{run.ControlSet}, ControlSet: run.ControlSet}, nil
}

// entry is what a queued segment carries of its user. The access token is
// what an executing replica that keeps no thread token mints one from; the
// entry is deleted once handled, and the token expires in minutes.
func entry(u auth.Principal, fields map[string]string) map[string]string {
	fields["user_id"], fields["username"] = u.Subject, u.Username
	if u.Token != "" {
		fields["authorization"] = u.Bearer()
	}
	return fields
}

// entryUser is the user an entry carries.
func entryUser(fields map[string]string) auth.Principal {
	token, _ := strings.CutPrefix(fields["authorization"], "Bearer ")
	return auth.Principal{Subject: fields["user_id"], Username: fields["username"], Token: token}
}
