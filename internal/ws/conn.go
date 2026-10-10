package ws

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/coder/websocket"

	"agentlite/internal/agent"
)

// readLimit caps a client frame.
const readLimit = 64 << 10

// Conn serves one client socket until it closes.
type Conn struct {
	conn *websocket.Conn
	svc  *agent.Service

	writeMu sync.Mutex
	subMu   sync.Mutex
	subs    map[string]*subscription // thread id → its live subscription
}

type subscription struct{ cancel context.CancelFunc }

// Serve reads frames until the socket closes. Each thread has at most one
// subscription; a new subscribe on the same thread replaces it.
func Serve(ctx context.Context, conn *websocket.Conn, svc *agent.Service) {
	c := &Conn{conn: conn, svc: svc, subs: map[string]*subscription{}}
	conn.SetReadLimit(readLimit)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f ClientFrame
		if err := json.Unmarshal(data, &f); err != nil || f.ThreadID == "" {
			c.sendError(ctx, f.ThreadID, CodeBadRequest, "frame needs a type and threadId")
			continue
		}
		switch f.Type {
		case FrameSubscribe:
			c.subscribe(ctx, f.ThreadID, f.After)
		case FrameUnsubscribe:
			c.unsubscribe(f.ThreadID)
		default:
			c.sendError(ctx, f.ThreadID, CodeBadRequest, "unknown frame type "+f.Type)
		}
	}
}

// subscribe streams the thread's active run from the cursor until the segment
// ends, replacing any earlier subscription on the thread.
func (c *Conn) subscribe(ctx context.Context, threadID, after string) {
	subCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{cancel: cancel}
	c.subMu.Lock()
	if old := c.subs[threadID]; old != nil {
		old.cancel()
	}
	c.subs[threadID] = sub
	c.subMu.Unlock()
	go func() {
		defer c.drop(threadID, sub)
		entries, err := c.svc.Subscribe(subCtx, threadID, after)
		if err != nil {
			c.sendError(subCtx, threadID, CodeInternal, err.Error())
			return
		}
		if entries == nil {
			c.send(subCtx, ServerFrame{Type: FrameIdle, ThreadID: threadID})
			return
		}
		for e, err := range entries {
			if err != nil {
				c.sendError(subCtx, threadID, CodeInternal, err.Error())
				return
			}
			if !c.send(subCtx, ServerFrame{Type: FrameEvent, ThreadID: threadID, Seq: e.Seq, Event: e.Raw}) {
				return
			}
		}
	}()
}

func (c *Conn) unsubscribe(threadID string) {
	c.subMu.Lock()
	sub := c.subs[threadID]
	c.subMu.Unlock()
	if sub != nil {
		c.drop(threadID, sub)
	}
}

// drop ends a subscription, unless the thread has since been resubscribed.
func (c *Conn) drop(threadID string, sub *subscription) {
	sub.cancel()
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subs[threadID] == sub {
		delete(c.subs, threadID)
	}
}

func (c *Conn) send(ctx context.Context, f ServerFrame) bool {
	raw, _ := json.Marshal(f)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, raw) == nil
}

func (c *Conn) sendError(ctx context.Context, threadID, code, msg string) {
	c.send(ctx, ServerFrame{Type: FrameError, ThreadID: threadID, Code: code, Message: msg})
}
