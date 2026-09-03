package logging

import "context"

// 以下 With* 将结构化字段注入 context 中的 logger，供后续日志自动携带。

func WithNode(ctx context.Context, id int, name string) context.Context {
	l := Ctx(ctx).With().Int("node_id", id).Str("node_name", name).Logger()
	return context.WithValue(ctx, ctxKey{}, &l)
}

func WithChange(ctx context.Context, id int) context.Context {
	l := Ctx(ctx).With().Int("change_id", id).Logger()
	return context.WithValue(ctx, ctxKey{}, &l)
}

func WithTask(ctx context.Context, id int) context.Context {
	l := Ctx(ctx).With().Int("task_id", id).Logger()
	return context.WithValue(ctx, ctxKey{}, &l)
}

func WithTrace(ctx context.Context, traceID string) context.Context {
	l := Ctx(ctx).With().Str("trace_id", traceID).Logger()
	return context.WithValue(ctx, ctxKey{}, &l)
}
