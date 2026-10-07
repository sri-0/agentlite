package stream

import "context"

// Sink receives events for a run's log. The engine installs one in the run
// context so tools can emit activities and sub-agent events directly.
type Sink interface {
	Emit(Event)
}

type sinkKey struct{}

// WithSink attaches a sink to the context.
func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

// SinkFrom returns the context's sink, or nil outside a run.
func SinkFrom(ctx context.Context) Sink {
	s, _ := ctx.Value(sinkKey{}).(Sink)
	return s
}
