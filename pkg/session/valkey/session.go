// Package valkeysession is an adk session.Service on Valkey: one hash of
// metadata, one list of events and one hash of state per session, plus the
// app: and user: state the sessions of an app or a user share.
package valkeysession

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

// SessionService is adk's session.Service on Valkey. Sessions are keyed by
// app, user and id (agentlite's ids are thread ids), so a thread's LLM
// context survives restarts and instances.
type SessionService struct{ db valkey.Client }

// New wraps a client.
func New(db valkey.Client) *SessionService { return &SessionService{db: db} }

func sessionKey(app, user, id string) string { return "session:" + app + ":" + user + ":" + id }
func eventsKey(key string) string            { return key + ":events" }
func stateKey(key string) string             { return key + ":state" }
func appStateKey(app string) string          { return "state:" + app }
func userStateKey(app, user string) string   { return "state:" + app + ":" + user }

// sessionsIndex lists an app's sessions as "user\x00id", for List.
func sessionsIndex(app string) string { return "sessions:" + app }

func (s *SessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	id := req.SessionID
	if id == "" {
		id = uuid.Must(uuid.NewV7()).String()
	}
	key, now := sessionKey(req.AppName, req.UserID, id), time.Now()
	created, err := s.db.Do(ctx, s.db.B().Hsetnx().Key(key).Field("updated").Value(now.Format(time.RFC3339Nano)).Build()).AsBool()
	if err != nil {
		return nil, fmt.Errorf("session create: %w", err)
	}
	if !created {
		return nil, fmt.Errorf("session create: %q already exists", id)
	}
	cmds := valkey.Commands{
		s.db.B().Expire().Key(key).Seconds(int64(sessionTTL.Seconds())).Build(),
		s.db.B().Sadd().Key(sessionsIndex(req.AppName)).Member(req.UserID + "\x00" + id).Build(),
	}
	cmds = append(cmds, s.setState(req.AppName, req.UserID, key, req.State)...)
	for _, r := range s.db.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return nil, fmt.Errorf("session create: %w", err)
		}
	}
	got, err := s.Get(ctx, &session.GetRequest{AppName: req.AppName, UserID: req.UserID, SessionID: id})
	if err != nil {
		return nil, err
	}
	return &session.CreateResponse{Session: got.Session}, nil
}

func (s *SessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	key := sessionKey(req.AppName, req.UserID, req.SessionID)
	start := int64(0)
	if req.NumRecentEvents > 0 && req.After.IsZero() {
		start = -int64(req.NumRecentEvents) // only the tail is wanted
	}
	res := s.db.DoMulti(ctx,
		s.db.B().Hget().Key(key).Field("updated").Build(),
		s.db.B().Hgetall().Key(stateKey(key)).Build(),
		s.db.B().Hgetall().Key(appStateKey(req.AppName)).Build(),
		s.db.B().Hgetall().Key(userStateKey(req.AppName, req.UserID)).Build(),
		s.db.B().Lrange().Key(eventsKey(key)).Start(start).Stop(-1).Build(),
	)
	updated, err := res[0].ToString()
	if valkey.IsValkeyNil(err) {
		return nil, fmt.Errorf("%w: %q", session.ErrNotFound, req.SessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("session get: %w", err)
	}
	updatedAt, _ := time.Parse(time.RFC3339Nano, updated)
	sess := &localSession{app: req.AppName, user: req.UserID, id: req.SessionID, state: map[string]any{}, updatedAt: updatedAt}
	for _, r := range res[1:4] { // session, app and user state; their keys carry their prefixes
		raw, err := r.AsStrMap()
		if err != nil {
			return nil, fmt.Errorf("session state: %w", err)
		}
		for k, v := range raw {
			var val any
			if err := json.Unmarshal([]byte(v), &val); err == nil {
				sess.state[k] = val
			}
		}
	}
	rawEvents, err := res[4].AsStrSlice()
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

// List lists the user's sessions in the app, or every user's when UserID is
// empty, without their events.
func (s *SessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	members, err := s.db.Do(ctx, s.db.B().Smembers().Key(sessionsIndex(req.AppName)).Build()).AsStrSlice()
	if err != nil {
		return nil, fmt.Errorf("session list: %w", err)
	}
	resp := &session.ListResponse{Sessions: []session.Session{}}
	for _, m := range members {
		user, id, _ := strings.Cut(m, "\x00")
		if req.UserID != "" && user != req.UserID {
			continue
		}
		got, err := s.Get(ctx, &session.GetRequest{AppName: req.AppName, UserID: user, SessionID: id})
		if err != nil {
			continue // expired since it was indexed
		}
		got.Session.(*localSession).events = nil
		resp.Sessions = append(resp.Sessions, got.Session)
	}
	return resp, nil
}

func (s *SessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	key := sessionKey(req.AppName, req.UserID, req.SessionID)
	for _, r := range s.db.DoMulti(ctx,
		s.db.B().Del().Key(key, eventsKey(key), stateKey(key)).Build(),
		s.db.B().Srem().Key(sessionsIndex(req.AppName)).Member(req.UserID+"\x00"+req.SessionID).Build(),
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
		event.ID = uuid.Must(uuid.NewV7()).String()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	stored := stripTempState(event)
	raw, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("session event: %w", err)
	}
	key, secs := sessionKey(sess.app, sess.user, sess.id), int64(sessionTTL.Seconds())
	// Refreshing the expiry first also finds a deleted session.
	exists, err := s.db.Do(ctx, s.db.B().Expire().Key(key).Seconds(secs).Build()).AsBool()
	if err != nil {
		return fmt.Errorf("session append: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %q", session.ErrNotFound, sess.id)
	}
	cmds := valkey.Commands{ // one transaction: the event, its state and the refreshed expiry land together
		s.db.B().Multi().Build(),
		s.db.B().Rpush().Key(eventsKey(key)).Element(string(raw)).Build(),
		s.db.B().Hset().Key(key).FieldValue().FieldValue("updated", event.Timestamp.Format(time.RFC3339Nano)).Build(),
		s.db.B().Expire().Key(eventsKey(key)).Seconds(secs).Build(),
	}
	cmds = append(cmds, s.setState(sess.app, sess.user, key, stored.Actions.StateDelta)...)
	cmds = append(cmds, s.db.B().Exec().Build())
	for _, r := range s.db.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return fmt.Errorf("session append: %w", err)
		}
	}
	sess.append(stored)
	return nil
}

// setState writes state by its prefix: app: keys to the app's hash, user:
// keys to the user's, temp: keys nowhere, the rest to the session's own.
func (s *SessionService) setState(app, user, key string, state map[string]any) valkey.Commands {
	var cmds valkey.Commands
	for k, v := range state {
		target := stateKey(key)
		switch {
		case strings.HasPrefix(k, session.KeyPrefixTemp):
			continue
		case strings.HasPrefix(k, session.KeyPrefixApp):
			target = appStateKey(app)
		case strings.HasPrefix(k, session.KeyPrefixUser):
			target = userStateKey(app, user)
		}
		raw, _ := json.Marshal(v)
		cmds = append(cmds, s.db.B().Hset().Key(target).FieldValue().FieldValue(k, string(raw)).Build())
	}
	if len(cmds) > 0 {
		cmds = append(cmds, s.db.B().Expire().Key(stateKey(key)).Seconds(int64(sessionTTL.Seconds())).Build())
	}
	return cmds
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
