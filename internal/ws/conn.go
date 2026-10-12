package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

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

// client serves one socket. Writes go through the socket's own context:
// coder/websocket closes the connection when a write's context ends, so a
// subscription that is replaced or unsubscribed must not write with its own.
type client struct {
	conn *websocket.Conn
	ctx  context.Context // the socket's lifetime
	runs *runs.Manager

	mu   sync.Mutex
	subs map[string]*subscription // thread id → its live subscription
}

type subscription struct{ cancel context.CancelFunc }

// Serve reads frames until the socket closes. Each thread has at most one
// subscription; a new subscribe on the same thread replaces it. Runs,
// answers and cancels are handled off the read loop, so a slow one never
// holds up the frames behind it.
func Serve(ctx context.Context, conn *websocket.Conn, m *runs.Manager) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	c := &client{conn: conn, ctx: ctx, runs: m, subs: map[string]*subscription{}}
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
	ack, err := handle(ctx, f)
	if err != nil {
		c.sendError(f, code(err), err.Error())
		return
	}
	ack.Type, ack.RequestID = FrameAck, f.RequestID
	c.send(ack)
}

// run starts a turn; the ack names the thread (created when none was given)
// and the cursor to subscribe from.
func (c *client) run(ctx context.Context, f ClientFrame) (ServerFrame, error) {
	if len(f.Messages) == 0 || f.Messages[len(f.Messages)-1].Role != "user" {
		return ServerFrame{}, fmt.Errorf("%w: the last message must be from the user", runs.ErrInvalid)
	}
	last := f.Messages[len(f.Messages)-1]
	run, err := c.runs.Start(ctx, runs.StartInput{
		ThreadID: f.ThreadID, AgentID: f.AgentID, Model: f.Model, Connectors: f.Connectors, MessageID: last.ID, Text: last.Content,
	})
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
		entries, err := c.runs.Subscribe(subCtx, threadID, after)
		if err != nil {
			c.sendError(ClientFrame{ThreadID: threadID}, CodeInternal, err.Error())
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
				c.sendError(ClientFrame{ThreadID: threadID}, CodeInternal, err.Error())
				return
			}
			if !c.send(ServerFrame{Type: FrameEvent, ThreadID: threadID, Seq: e.Seq, Event: e.Raw}) {
				return
			}
		}
	}()
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
