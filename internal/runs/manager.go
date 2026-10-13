package runs

import (
	"context"
	"errors"
	"sync"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"

	"agentlite/internal/config"
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
	// Authorization is the user's Authorization header from the request that
	// queued the segment; connectors that forward identity send it on.
	Authorization string
}

// Sink persists threads and messages (OpenSearch now, a producer later).
type Sink interface {
	SaveThread(ctx context.Context, t *opensearch.Thread) error
	SaveMessage(ctx context.Context, m *opensearch.Message) error
}

// History reads what the sink persisted.
type History interface {
	Thread(ctx context.Context, id string) (*opensearch.Thread, error)
	Messages(ctx context.Context, threadID string, before int64, limit int) ([]opensearch.Message, error)
}

// Manager owns the run lifecycle.
type Manager struct {
	db       valkey.Client
	store    *store
	log      Log
	sink     Sink
	history  History
	sessions session.Service
	runtime  Runtime
	hitl     *hitl.Store
	defs     map[string]config.AgentDef
	cfg      *config.Config
	appName  string
	model    string // the model a run uses when the request names none
	logger   zerolog.Logger
	workers  sync.WaitGroup
}

// New wires the run lifecycle.
// db holds the run lifecycle (logs, run records, leases, queues); interrupts
// are kept apart in their own store.
func New(cfg *config.Config, db valkey.Client, interrupts *hitl.Store, sink Sink, history History, sessions session.Service, runtime Runtime, logger zerolog.Logger) *Manager {
	return &Manager{
		db: db, store: &store{db: db}, log: newValkeyLog(db), sink: sink, history: history, sessions: sessions, runtime: runtime,
		hitl: interrupts, defs: cfg.Defs(), cfg: cfg, appName: cfg.AppName, model: cfg.Models.DefaultModel, logger: logger,
	}
}

const (
	// SessionUser is adk's required user id; agentlite has no identity yet.
	SessionUser = "default"

	// DefaultAgent answers when no agent is named.
	DefaultAgent = "assistant"
)
