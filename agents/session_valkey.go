package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"
)

// sessionTTL keeps idle thread sessions for a month after their last use.
const sessionTTL = 30 * 24 * time.Hour

// SessionService is adk's session.Service on Valkey: one hash of metadata, a
// list of JSON events and a hash of state per session. Sessions are keyed by
// thread id, so a thread's LLM context survives restarts and instances.
type SessionService struct{ db valkey.Client }

// NewSessionService wraps a client.
func NewSessionService(db valkey.Client) *SessionService { return &SessionService{db: db} }

func sessionKey(id string) string     { return "session:" + id }
func eventsKey(id string) string      { return "session:" + id + ":events" }
func stateKey(id string) string       { return "session:" + id + ":state" }
func sessionsIndex(app string) string { return "sessions:" + app }

func (s *SessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	id := req.SessionID
	if id == "" {
		id = uuid.NewString()
	}
	now := time.Now()
	sess := &localSession{app: req.AppName, user: req.UserID, id: id, state: map[string]any{}, updatedAt: now}
	maps.Copy(sess.state, req.State)
	cmds := valkey.Commands{
		s.db.B().Hset().Key(sessionKey(id)).FieldValue().FieldValue("app", req.AppName).FieldValue("user", req.UserID).
			FieldValue("updated", now.Format(time.RFC3339Nano)).Build(),
		s.db.B().Sadd().Key(sessionsIndex(req.AppName)).Member(id).Build(),
	}
	for k, v := range sess.state {
		raw, _ := json.Marshal(v)
		cmds = append(cmds, s.db.B().Hset().Key(stateKey(id)).FieldValue().FieldValue(k, string(raw)).Build())
	}
	for _, r := range s.db.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return nil, fmt.Errorf("session create: %w", err)
		}
	}
	s.touch(ctx, id)
	return &session.CreateResponse{Session: sess}, nil
}

func (s *SessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	meta, err := s.db.Do(ctx, s.db.B().Hgetall().Key(sessionKey(req.SessionID)).Build()).AsStrMap()
	if err != nil {
		return nil, fmt.Errorf("session get: %w", err)
	}
	if len(meta) == 0 {
		return nil, fmt.Errorf("%w: %q", session.ErrNotFound, req.SessionID)
	}
	updatedAt, _ := time.Parse(time.RFC3339Nano, meta["updated"])
	sess := &localSession{app: meta["app"], user: meta["user"], id: req.SessionID, state: map[string]any{}, updatedAt: updatedAt}

	rawState, err := s.db.Do(ctx, s.db.B().Hgetall().Key(stateKey(req.SessionID)).Build()).AsStrMap()
	if err != nil {
		return nil, fmt.Errorf("session state: %w", err)
	}
	for k, v := range rawState {
		var val any
		if err := json.Unmarshal([]byte(v), &val); err == nil {
			sess.state[k] = val
		}
	}
	rawEvents, err := s.db.Do(ctx, s.db.B().Lrange().Key(eventsKey(req.SessionID)).Start(0).Stop(-1).Build()).AsStrSlice()
	if err != nil {
		return nil, fmt.Errorf("session events: %w", err)
	}
	for _, raw := range rawEvents {
		var ev session.Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil, fmt.Errorf("session event: %w", err)
		}
		if !req.After.IsZero() && ev.Timestamp.Before(req.After) {
			continue
		}
		sess.events = append(sess.events, &ev)
	}
	if n := req.NumRecentEvents; n > 0 && len(sess.events) > n {
		sess.events = sess.events[len(sess.events)-n:]
	}
	return &session.GetResponse{Session: sess}, nil
}

func (s *SessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	ids, err := s.db.Do(ctx, s.db.B().Smembers().Key(sessionsIndex(req.AppName)).Build()).AsStrSlice()
	if err != nil {
		return nil, fmt.Errorf("session list: %w", err)
	}
	resp := &session.ListResponse{}
	for _, id := range ids {
		got, err := s.Get(ctx, &session.GetRequest{AppName: req.AppName, UserID: req.UserID, SessionID: id})
		if err != nil {
			continue // expired since it was indexed
		}
		resp.Sessions = append(resp.Sessions, got.Session)
	}
	return resp, nil
}

func (s *SessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	id := req.SessionID
	for _, r := range s.db.DoMulti(ctx,
		s.db.B().Del().Key(sessionKey(id), eventsKey(id), stateKey(id)).Build(),
		s.db.B().Srem().Key(sessionsIndex(req.AppName)).Member(id).Build(),
	) {
		if err := r.Error(); err != nil {
			return fmt.Errorf("session delete: %w", err)
		}
	}
	return nil
}

func (s *SessionService) AppendEvent(ctx context.Context, cur session.Session, event *session.Event) error {
	if event.Partial {
		return nil
	}
	sess, ok := cur.(*localSession)
	if !ok {
		return fmt.Errorf("unexpected session type %T", cur)
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	stored := stripTempState(event)
	raw, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("session event: %w", err)
	}
	cmds := valkey.Commands{
		s.db.B().Rpush().Key(eventsKey(sess.id)).Element(string(raw)).Build(),
		s.db.B().Hset().Key(sessionKey(sess.id)).FieldValue().FieldValue("updated", event.Timestamp.Format(time.RFC3339Nano)).Build(),
	}
	for k, v := range stored.Actions.StateDelta {
		val, _ := json.Marshal(v)
		cmds = append(cmds, s.db.B().Hset().Key(stateKey(sess.id)).FieldValue().FieldValue(k, string(val)).Build())
	}
	for _, r := range s.db.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return fmt.Errorf("session append: %w", err)
		}
	}
	s.touch(ctx, sess.id)
	sess.append(stored)
	return nil
}

func (s *SessionService) touch(ctx context.Context, id string) {
	secs := int64(sessionTTL.Seconds())
	s.db.DoMulti(ctx,
		s.db.B().Expire().Key(sessionKey(id)).Seconds(secs).Build(),
		s.db.B().Expire().Key(eventsKey(id)).Seconds(secs).Build(),
		s.db.B().Expire().Key(stateKey(id)).Seconds(secs).Build(),
	)
}

// stripTempState drops temp: keys, which adk defines as never persisted.
func stripTempState(ev *session.Event) *session.Event {
	if len(ev.Actions.StateDelta) == 0 {
		return ev
	}
	kept := map[string]any{}
	for k, v := range ev.Actions.StateDelta {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			kept[k] = v
		}
	}
	if len(kept) == len(ev.Actions.StateDelta) {
		return ev
	}
	cp := *ev
	cp.Actions.StateDelta = kept
	return &cp
}

// localSession is the in-memory view adk works with during a run.
type localSession struct {
	app, user, id string

	mu        sync.RWMutex
	events    []*session.Event
	state     map[string]any
	updatedAt time.Time
}

func (s *localSession) ID() string      { return s.id }
func (s *localSession) AppName() string { return s.app }
func (s *localSession) UserID() string  { return s.user }
func (s *localSession) State() session.State {
	return &localState{mu: &s.mu, state: s.state}
}
func (s *localSession) Events() session.Events {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return eventList(slices.Clone(s.events))
}
func (s *localSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}
func (s *localSession) append(ev *session.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.state, ev.Actions.StateDelta)
	s.events = append(s.events, ev)
	s.updatedAt = ev.Timestamp
}

type eventList []*session.Event

func (e eventList) All() iter.Seq[*session.Event] { return slices.Values(e) }
func (e eventList) Len() int                      { return len(e) }
func (e eventList) At(i int) *session.Event {
	if i < 0 || i >= len(e) {
		return nil
	}
	return e[i]
}

type localState struct {
	mu    *sync.RWMutex
	state map[string]any
}

func (s *localState) Get(key string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.state[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}
func (s *localState) Set(key string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[key] = v
	return nil
}
func (s *localState) All() iter.Seq2[string, any] {
	s.mu.RLock()
	cp := maps.Clone(s.state)
	s.mu.RUnlock()
	return maps.All(cp)
}
