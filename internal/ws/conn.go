package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"agentlite/internal/runs"
	"agentlite/pkg/db/opensearch"
)

const (
	// readLimit caps a client frame.
	readLimit = 64 << 10
	// requestTimeout bounds a run, answer or cancel. Requests outlive the
	// socket: a client that disconnects mid-request still gets it completed.
	requestTimeout = 30 * time.Second
)

// Threads looks a thread up as the user sees it. With OpenSearch
// document-level security on, a thread the user may not read is not found.
type Threads interface {
	Thread(ctx context.Context, id string) (*opensearch.Thread, error)
}

// client serves one socket. Writes go through the socket's own context:
// coder/websocket closes the connection when a write's context ends, so a
// subscription that is replaced or unsubscribed must not write with its own.
type client struct {
	conn    *websocket.Conn
	ctx     context.Context // the socket's lifetime
	runs    *runs.Manager
	threads Threads

	mu      sync.Mutex
	subs    map[string]*subscription // thread id → its live subscription
	allowed map[string]bool          // threads this socket was let in to
}

type subscription struct{ cancel context.CancelFunc }

// Serve reads frames until the socket closes. Every frame names a thread the
// socket must be let in to (see allow). Each thread has at most one
// subscription; a new subscribe on the same thread replaces it. Runs,
// answers and cancels are handled off the read loop, so a slow one never
// holds up the frames behind it.
func Serve(ctx context.Context, conn *websocket.Conn, m *runs.Manager, threads Threads) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	c := &client{conn: conn, ctx: ctx, runs: m, threads: threads, subs: map[string]*subscription{}, allowed: map[string]bool{}}
	conn.SetReadLimit(readLimit)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f ClientFrame
		if err := json.Unmarshal(data, &f); err != nil {
			c.sendError(f, CodeBadRequest, "invalid frame")
			continue
		}
		switch f.Type {
		case FrameRun:
			go c.request(f, c.run)
		case FrameAnswer:
			go c.request(f, c.answer)
		case FrameCancel:
			go c.request(f, c.cancel)
		case FrameSubscribe:
			c.subscribe(f.ThreadID, f.After)
		case FrameUnsubscribe:
			c.unsubscribe(f.ThreadID)
		default:
			c.sendError(f, CodeBadRequest, "unknown frame type "+f.Type)
		}
	}
}

// request handles one request frame and replies with its ack or error.
func (c *client) request(f ClientFrame, handle func(context.Context, ClientFrame) (ServerFrame, error)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), requestTimeout)
	defer cancel()
	err := c.allow(ctx, f.ThreadID)
	var ack ServerFrame
	if err == nil {
		ack, err = handle(ctx, f)
	}
	if err != nil {
		c.sendError(f, code(err), err.Error())
		return
	}
	ack.Type, ack.RequestID = FrameAck, f.RequestID
	c.send(ack)
}

// run starts a turn on the thread; the ack has the cursor to subscribe from.
func (c *client) run(ctx context.Context, f ClientFrame) (ServerFrame, error) {
	in, err := f.StartInput(f.ThreadID)
	if err != nil {
		return ServerFrame{}, err
	}
	run, err := c.runs.Start(ctx, in)
	if err != nil {
		return ServerFrame{}, err
	}
	return ServerFrame{ThreadID: run.ThreadID, RunID: run.ID, Seq: run.SegmentFrom}, nil
}

// answer decides one interrupt on the thread it belongs to (a sub-agent's
// thread for a request shown on an agent card).
func (c *client) answer(ctx context.Context, f ClientFrame) (ServerFrame, error) {
	run, err := c.runs.Answer(ctx, f.ThreadID, f.InterruptID, f.Decision)
	if err != nil {
		return ServerFrame{}, err
	}
	return ServerFrame{ThreadID: f.ThreadID, RunID: run.ID, Seq: run.SegmentFrom}, nil
}

func (c *client) cancel(ctx context.Context, f ClientFrame) (ServerFrame, error) {
	return ServerFrame{ThreadID: f.ThreadID}, c.runs.Cancel(ctx, f.ThreadID)
}

// subscribe streams the thread's active run from the cursor until the segment
// ends, replacing any earlier subscription on the thread.
func (c *client) subscribe(threadID, after string) {
	subCtx, cancel := context.WithCancel(c.ctx)
	sub := &subscription{cancel: cancel}
	c.mu.Lock()
	if old := c.subs[threadID]; old != nil {
		old.cancel()
	}
	c.subs[threadID] = sub
	c.mu.Unlock()
	go func() {
		defer c.drop(threadID, sub)
		fail := func(err error) {
			if subCtx.Err() == nil { // a replaced or ended subscription has nothing to report
				c.sendError(ClientFrame{ThreadID: threadID}, code(err), err.Error())
			}
		}
		if err := c.allow(subCtx, threadID); err != nil {
			fail(err)
			return
		}
		entries, err := c.runs.Subscribe(subCtx, threadID, after)
		if err != nil {
			fail(err)
			return
		}
		if entries == nil {
			c.send(ServerFrame{Type: FrameIdle, ThreadID: threadID})
			return
		}
		for e, err := range entries {
			if subCtx.Err() != nil { // replaced or unsubscribed
				return
			}
			if err != nil {
				fail(err)
				return
			}
			if !c.send(ServerFrame{Type: FrameEvent, ThreadID: threadID, Seq: e.Seq, Event: e.Raw}) {
				return
			}
		}
	}()
}

// allow lets the socket in to a thread. The first frame about a thread,
// normally its subscribe, looks the thread up as the user sees it; a thread
// that is not found (missing, or hidden by document-level security) is
// refused. A thread let in stays allowed for the socket's life, so later
// frames cost nothing. Being let in to a thread gives that thread's stream
// and requests only.
func (c *client) allow(ctx context.Context, threadID string) error {
	if threadID == "" {
		return fmt.Errorf("%w: threadId is required; a new thread is created with POST /v1/agent/chat", runs.ErrInvalid)
	}
	if id, err := uuid.Parse(threadID); err != nil || id.Version() != 7 {
		return fmt.Errorf("%w: threadId must be a UUIDv7", runs.ErrInvalid)
	}
	c.mu.Lock()
	ok := c.allowed[threadID]
	c.mu.Unlock()
	if ok {
		return nil
	}
	if _, err := c.threads.Thread(ctx, threadID); err != nil {
		return err
	}
	c.mu.Lock()
	c.allowed[threadID] = true
	c.mu.Unlock()
	return nil
}

func (c *client) unsubscribe(threadID string) {
	c.mu.Lock()
	sub := c.subs[threadID]
	c.mu.Unlock()
	if sub != nil {
		c.drop(threadID, sub)
	}
}

// drop ends a subscription, unless the thread has since been resubscribed.
func (c *client) drop(threadID string, sub *subscription) {
	sub.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs[threadID] == sub {
		delete(c.subs, threadID)
	}
}

// send writes one frame; coder/websocket serialises concurrent writes.
func (c *client) send(f ServerFrame) bool {
	raw, _ := json.Marshal(f)
	return c.conn.Write(c.ctx, websocket.MessageText, raw) == nil
}

func (c *client) sendError(f ClientFrame, code, msg string) {
	c.send(ServerFrame{Type: FrameError, RequestID: f.RequestID, ThreadID: f.ThreadID, Code: code, Message: msg})
}

// code names a service error for the client.
func code(err error) string {
	switch {
	case errors.Is(err, runs.ErrInvalid):
		return CodeBadRequest
	case errors.Is(err, runs.ErrNotFound), errors.Is(err, opensearch.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, runs.ErrBusy):
		return CodeBusy
	case errors.Is(err, runs.ErrNotPaused):
		return CodeNotPaused
	default:
		return CodeInternal
	}
}
