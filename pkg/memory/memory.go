// Package memory implements adk's memory.Service on OpenSearch: every text
// turn of a finished session is embedded and stored, and searches return the
// nearest entries so agents can recall earlier conversations.
package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"agentlite/internal/proxy"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

const searchLimit = 5

// Service is the OpenSearch-backed memory. Runs use it through ForThread.
type Service struct {
	store    *opensearch.Client
	embedder *proxy.Client
	model    string
	log      zerolog.Logger
}

// New prepares the index and the embedding client.
func New(ctx context.Context, store *opensearch.Client, embedder *proxy.Client, model string, dimensions int, log zerolog.Logger) (*Service, error) {
	if err := store.EnsureMemoryIndex(ctx, dimensions); err != nil {
		return nil, err
	}
	return &Service{store: store, embedder: embedder, model: model, log: log}, nil
}

// AddSessionToMemory stores the text turns of the session's latest turn: the
// last user message and everything after it. Ids are derived from the event,
// so storing a turn twice overwrites rather than duplicates.
func (s *Service) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	var turn []*session.Event
	for ev := range sess.Events().All() {
		if ev.Author == "user" && stream.TextOf(ev.Content) != "" {
			turn = turn[:0]
		}
		turn = append(turn, ev)
	}
	for _, ev := range turn {
		text := stream.TextOf(ev.Content)
		if text == "" {
			continue
		}
		id := memoryID(sess.ID(), ev.ID)
		vector, err := s.embedder.Embed(ctx, s.model, text)
		if err != nil {
			return fmt.Errorf("embed: %w", err)
		}
		if err := s.store.SaveMemory(ctx, &opensearch.Memory{
			ID: id, ThreadID: sess.ID(), Author: ev.Author, Text: text, Embedding: vector, CreatedAt: ev.Timestamp,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ForThread is the memory as one thread sees it: searches skip the thread's
// own turns, which the model already has in context.
func (s *Service) ForThread(threadID string) memory.Service {
	return &threadView{Service: s, threadID: threadID}
}

type threadView struct {
	*Service
	threadID string
}

// SearchMemory returns the entries nearest to the query. Memory is an
// enhancement: when embedding or search fails the turn proceeds without
// memories and the failure is logged.
func (v *threadView) SearchMemory(ctx context.Context, req *memory.SearchRequest) (*memory.SearchResponse, error) {
	ctx, span := otel.Tracer("agentlite").Start(ctx, "memory.search")
	defer span.End()
	s := v.Service
	vector, err := s.embedder.Embed(ctx, s.model, req.Query)
	if err != nil {
		s.log.Warn().Err(err).Msg("memory search skipped: embedding failed")
		return &memory.SearchResponse{}, nil
	}
	found, err := s.store.SearchMemories(ctx, vector, searchLimit, v.threadID)
	if err != nil {
		s.log.Warn().Err(err).Msg("memory search skipped")
		return &memory.SearchResponse{}, nil
	}
	resp := &memory.SearchResponse{}
	for _, m := range found {
		role := genai.Role(genai.RoleModel)
		if m.Author == "user" {
			role = genai.RoleUser
		}
		resp.Memories = append(resp.Memories, memory.Entry{
			ID: m.ID, Author: m.Author, Timestamp: m.CreatedAt, Content: genai.NewContentFromText(m.Text, role),
		})
	}
	return resp, nil
}

func memoryID(sessionID, eventID string) string {
	sum := sha1.Sum([]byte(sessionID + ":" + eventID))
	return hex.EncodeToString(sum[:])
}
