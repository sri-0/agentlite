package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
)

// Queue names.
const (
	requestsKey = "runs:"     // + agent id: run requests consumed by workers
	doneKey     = "runs:done" // pause and terminal notices consumed by finalizers
	workerGroup = "workers"
	finalGroup  = "finalizers"
	staleAfter  = 60 * time.Second
)

// A queue is a Valkey stream with a consumer group: at-least-once delivery,
// entries acknowledged after the handler returns, stale pending entries
// reclaimed from crashed consumers.
type queue struct {
	db       valkey.Client
	key      string
	group    string
	consumer string
	log      zerolog.Logger
}

func (q *queue) enqueue(ctx context.Context, fields map[string]string) error {
	cmd := q.db.B().Xadd().Key(q.key).Id("*").FieldValue()
	for k, v := range fields {
		cmd = cmd.FieldValue(k, v)
	}
	return q.db.Do(ctx, cmd.Build()).Error()
}

// consume runs handler for every entry until ctx ends. A handler error leaves
// the entry pending so it is reclaimed later; a nil return acknowledges it.
func (q *queue) consume(ctx context.Context, handler func(context.Context, map[string]string) error) {
	if err := q.db.Do(ctx, q.db.B().XgroupCreate().Key(q.key).Group(q.group).Id("$").Mkstream().Build()).Error(); err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		q.log.Error().Err(err).Str("queue", q.key).Msg("create consumer group")
		return
	}
	q.reclaim(ctx, handler)
	for ctx.Err() == nil {
		res, err := q.db.Do(ctx, q.db.B().Xreadgroup().Group(q.group, q.consumer).Count(1).Block(readBlock.Milliseconds()).
			Streams().Key(q.key).Id(">").Build()).AsXRead()
		if err != nil {
			if !valkey.IsValkeyNil(err) && !errors.Is(err, context.Canceled) {
				q.log.Error().Err(err).Str("queue", q.key).Msg("read")
				time.Sleep(time.Second)
			}
			continue
		}
		for _, x := range res[q.key] {
			q.handle(ctx, x, handler)
		}
	}
}

// reclaim takes over entries left pending by a consumer that died.
func (q *queue) reclaim(ctx context.Context, handler func(context.Context, map[string]string) error) {
	res, err := q.db.Do(ctx, q.db.B().Xautoclaim().Key(q.key).Group(q.group).Consumer(q.consumer).
		MinIdleTime(strconv.FormatInt(staleAfter.Milliseconds(), 10)).Start("0-0").Count(100).Build()).ToArray()
	if err != nil || len(res) < 2 {
		return
	}
	entries, err := res[1].AsXRange()
	if err != nil {
		return
	}
	for _, x := range entries {
		q.handle(ctx, x, handler)
	}
}

func (q *queue) handle(ctx context.Context, x valkey.XRangeEntry, handler func(context.Context, map[string]string) error) {
	if err := handler(ctx, x.FieldValues); err != nil {
		q.log.Error().Err(err).Str("queue", q.key).Str("entry", x.ID).Msg("handler failed; entry stays pending")
		return
	}
	if err := q.db.Do(ctx, q.db.B().Xack().Key(q.key).Group(q.group).Id(x.ID).Build()).Error(); err != nil {
		q.log.Error().Err(err).Str("queue", q.key).Str("entry", x.ID).Msg("ack")
	}
}
