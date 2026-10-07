package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"agentlite/internal/agent"
)

// Socket protocol version; the client must send ?v=1.
const protocolVersion = "1"

// Frames from the client. Every frame names its thread.
type clientFrame struct {
	Type     string `json:"type"` // run | subscribe | unsubscribe | cancel
	ThreadID string `json:"threadId"`
	Message  *struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	} `json:"message,omitempty"`
	AgentID string              `json:"agentId,omitempty"`
	Model   string              `json:"model,omitempty"`
	Resume  []types.ResumeEntry `json:"resume,omitempty"`
	After   string              `json:"after,omitempty"`
}

// Frames to the client: an AG-UI event with its cursor, idle, or an error.
type serverFrame struct {
	Type     string          `json:"type"` // event | idle | error
	ThreadID string          `json:"threadId"`
	Seq      string          `json:"seq,omitempty"`
	Event    json.RawMessage `json:"event,omitempty"`
	Code     string          `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
}

// Chat is the agent WebSocket: one socket per client, any number of thread
// subscriptions multiplexed over it.
func (h *Handler) Chat(c *echo.Context) error {
	if c.QueryParam("v") != protocolVersion {
		return echo.NewHTTPError(http.StatusUpgradeRequired, "unsupported protocol version; use ?v="+protocolVersion)
	}
	conn, err := websocket.Accept(c.Response(), c.Request(), &websocket.AcceptOptions{OriginPatterns: originHosts(h.Config.AllowedOrigins)})
	if err != nil {
		return nil // Accept has already written the HTTP error
	}
	s := &socket{conn: conn, svc: h.Service, subs: map[string]context.CancelFunc{}}
	s.serve(c.Request().Context())
	return nil
}

func originHosts(origins []string) []string {
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}

type socket struct {
	conn *websocket.Conn
	svc  *agent.Service

	writeMu sync.Mutex
	subMu   sync.Mutex
	subs    map[string]context.CancelFunc // thread id → stop its subscription
}

func (s *socket) serve(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer s.conn.CloseNow()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		var f clientFrame
		if err := json.Unmarshal(data, &f); err != nil || f.ThreadID == "" {
			s.sendError(ctx, f.ThreadID, "bad_request", "frame needs a type and threadId")
			continue
		}
		s.handle(ctx, f)
	}
}

func (s *socket) handle(ctx context.Context, f clientFrame) {
	switch f.Type {
	case "run":
		var run *agent.Run
		var err error
		switch {
		case f.Message != nil:
			run, err = s.svc.Start(ctx, agent.StartInput{ThreadID: f.ThreadID, AgentID: f.AgentID, Model: f.Model, MessageID: f.Message.ID, Text: f.Message.Content})
		case len(f.Resume) > 0:
			run, err = s.svc.Resume(ctx, f.ThreadID, f.Resume)
		default:
			err = errors.New("run needs a message or resume entries")
		}
		if err != nil {
			s.sendError(ctx, f.ThreadID, errorCode(err), err.Error())
			return
		}
		s.subscribe(ctx, f.ThreadID, run.SegmentFrom)
	case "subscribe":
		s.subscribe(ctx, f.ThreadID, f.After)
	case "unsubscribe":
		s.unsubscribe(f.ThreadID)
	case "cancel":
		if err := s.svc.Cancel(ctx, f.ThreadID); err != nil {
			s.sendError(ctx, f.ThreadID, errorCode(err), err.Error())
		}
	default:
		s.sendError(ctx, f.ThreadID, "bad_request", "unknown frame type "+f.Type)
	}
}

// subscribe streams the thread's active run from the cursor until the segment
// ends; a new subscription on the same thread replaces the old one.
func (s *socket) subscribe(ctx context.Context, threadID, after string) {
	s.unsubscribe(threadID)
	subCtx, cancel := context.WithCancel(ctx)
	s.subMu.Lock()
	s.subs[threadID] = cancel
	s.subMu.Unlock()
	go func() {
		defer s.unsubscribe(threadID)
		entries, err := s.svc.Subscribe(subCtx, threadID, after)
		if err != nil {
			s.sendError(subCtx, threadID, errorCode(err), err.Error())
			return
		}
		if entries == nil {
			s.send(subCtx, serverFrame{Type: "idle", ThreadID: threadID})
			return
		}
		for e, err := range entries {
			if err != nil {
				s.sendError(subCtx, threadID, "internal", err.Error())
				return
			}
			if !s.send(subCtx, serverFrame{Type: "event", ThreadID: threadID, Seq: e.Seq, Event: e.Raw}) {
				return
			}
		}
	}()
}

func (s *socket) unsubscribe(threadID string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if cancel, ok := s.subs[threadID]; ok {
		cancel()
		delete(s.subs, threadID)
	}
}

func (s *socket) send(ctx context.Context, f serverFrame) bool {
	raw, _ := json.Marshal(f)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, raw) == nil
}

func (s *socket) sendError(ctx context.Context, threadID, code, msg string) {
	s.send(ctx, serverFrame{Type: "error", ThreadID: threadID, Code: code, Message: msg})
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, agent.ErrBusy):
		return "busy"
	case errors.Is(err, agent.ErrNotPaused):
		return "not_paused"
	case errors.Is(err, agent.ErrNotFound):
		return "not_found"
	default:
		return "bad_request"
	}
}
