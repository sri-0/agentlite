package runs

import (
	"context"
	"errors"
	"sync"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"

	"agentlite/config"
	"agentlite/internal/hitl"
	"agentlite/internal/stream"
	"agentlite/pkg/auth"
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

// Deps are the services the run manager uses.
type Deps struct {
	DB         valkey.Client // the run lifecycle: logs, run records, leases, queues
	Interrupts *hitl.Store   // kept apart, in a database of their own
	History    History
	Sessions   session.Service
	Runtime    Runtime
	Tokens     Tokens // the thread tokens runs call OpenSearch with (caller.go)
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
// thread's, whoever runs a turn on it, so it is the same for every user:
// a session per user would split a shared thread's history.
const SessionUser = "default"
