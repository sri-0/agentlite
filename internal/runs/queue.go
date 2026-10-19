package runs

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Queue names.
const (
	requestsKey = "runs:" // + agent id: run requests consumed by workers
	workerGroup = "workers"
	staleAfter  = 2 * leaseTTL // longer than a dead worker's lease survives
	// maxInFlight is how many entries one consumer executes at once. A parent
	// waiting on a sub-agent gives its slot back (Yield).
	maxInFlight = 32
	ackTimeout  = 5 * time.Second
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

// createGroup makes the queue's consumer group if it is missing, reading
// from the stream's start: handled entries are deleted, so only requests
// waiting for a consumer are there.
func (q *queue) createGroup(ctx context.Context) error {
	err := q.db.Do(ctx, q.db.B().XgroupCreate().Key(q.key).Group(q.group).Id("0").Mkstream().Build()).Error()
	if err != nil && !valkey.IsValkeyBusyGroup(err) {
		return fmt.Errorf("queue %s: create group: %w", q.key, err)
	}
	return nil
}

type slotKey struct{}

// Yield gives the caller's worker slot back while it waits on other runs, so
// parents waiting on their sub-agents never fill an agent's slots; the
// returned func takes a slot again before the caller goes on.
func Yield(ctx context.Context) (resume func()) {
	slots, ok := ctx.Value(slotKey{}).(chan struct{})
	if !ok {
		return func() {}
	}
	<-slots
	return func() { slots <- struct{}{} }
}

// consume runs handler for every entry until ctx ends, up to maxInFlight at
// a time, and returns once the entries in flight have finished. A nil return
// acknowledges the entry; an error leaves it pending, and pending entries
// older than staleAfter are retried whenever the queue is idle.
func (q *queue) consume(ctx context.Context, handler func(context.Context, map[string]string) error) {
	slots := make(chan struct{}, maxInFlight)
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	dispatch := func(x valkey.XRangeEntry) {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return // left pending for the next consumer
		}
		inFlight.Go(func() {
			defer func() { <-slots }()
			q.handle(context.WithValue(ctx, slotKey{}, slots), x, handler)
		})
	}
	q.reclaim(ctx, dispatch)
	for ctx.Err() == nil {
		res, err := q.db.Do(ctx, q.db.B().Xreadgroup().Group(q.group, q.consumer).Count(1).Block(readBlock.Milliseconds()).
			Streams().Key(q.key).Id(">").Build()).AsXRead()
		if err != nil {
			switch {
			case valkey.IsValkeyNil(err):
				q.reclaim(ctx, dispatch)
			case ctx.Err() == nil:
				q.log.Error().Err(err).Str("queue", q.key).Msg("read")
				select { // back off, unless shutting down
				case <-time.After(time.Second):
				case <-ctx.Done():
				}
			}
			continue
		}
		for _, x := range res[q.key] {
			dispatch(x)
		}
	}
}

// reclaim takes over entries left pending by a consumer that died or failed.
func (q *queue) reclaim(ctx context.Context, dispatch func(valkey.XRangeEntry)) {
	res, err := q.db.Do(ctx, q.db.B().Xautoclaim().Key(q.key).Group(q.group).Consumer(q.consumer).
		MinIdleTime(strconv.FormatInt(staleAfter.Milliseconds(), 10)).Start("0-0").Count(100).Build()).ToArray()
	var entries []valkey.XRangeEntry
	if err == nil && len(res) > 1 {
		entries, err = res[1].AsXRange()
	}
	if err != nil {
		if ctx.Err() == nil {
			q.log.Error().Err(err).Str("queue", q.key).Msg("reclaim")
		}
		return
	}
	for _, x := range entries {
		dispatch(x)
	}
}

func (q *queue) handle(ctx context.Context, x valkey.XRangeEntry, handler func(context.Context, map[string]string) error) {
	if err := handler(ctx, x.FieldValues); err != nil {
		q.log.Error().Err(err).Str("queue", q.key).Str("entry", x.ID).Msg("handler failed; entry stays pending")
		return
	}
	// Each queue has one consumer group, so a handled entry can go: the
	// stream stays small and an entry's fields (a user's token) do not linger.
	// The ack must land during a shutdown too, or the entry runs again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
	defer cancel()
	ack := q.db.B().Xack().Key(q.key).Group(q.group).Id(x.ID).Build()
	del := q.db.B().Xdel().Key(q.key).Id(x.ID).Build()
	for _, res := range q.db.DoMulti(ctx, ack, del) {
		if err := res.Error(); err != nil {
			q.log.Error().Err(err).Str("queue", q.key).Str("entry", x.ID).Msg("ack and delete")
		}
	}
}

// enqueue queues a segment of the run for its agent's workers, carrying the
// trace context so the worker continues the same trace.
func (m *Manager) enqueue(ctx context.Context, run *Run, fields map[string]string) error {
	fields["run_id"] = run.ID
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(fields))
	q := &queue{db: m.db, key: requestsKey + run.AgentID}
	return q.enqueue(ctx, fields)
}
