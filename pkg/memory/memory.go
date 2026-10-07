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
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"agentlite/internal/proxy"
	"agentlite/pkg/db/opensearch"
)

const searchLimit = 5

// Service is the OpenSearch-backed memory.
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

// AddSessionToMemory stores every text turn of the session not stored before.
func (s *Service) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	known, err := s.store.MemoryIDs(ctx, sess.ID())
	if err != nil {
		return err
	}
	for ev := range sess.Events().All() {
		text := textOf(ev.Content)
		if text == "" {
			continue
		}
		id := memoryID(sess.ID(), ev.ID)
		if known[id] {
			continue
		}
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

// SearchMemory returns the entries nearest to the query.
func (s *Service) SearchMemory(ctx context.Context, req *memory.SearchRequest) (*memory.SearchResponse, error) {
	vector, err := s.embedder.Embed(ctx, s.model, req.Query)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	found, err := s.store.SearchMemories(ctx, vector, searchLimit)
	if err != nil {
		return nil, err
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

// textOf joins a content's plain text parts; tool calls and thoughts are skipped.
func textOf(c *genai.Content) string {
	if c == nil {
		return ""
	}
	out := ""
	for _, p := range c.Parts {
		if p.Text != "" && !p.Thought {
			out += p.Text
		}
	}
	return out
}
