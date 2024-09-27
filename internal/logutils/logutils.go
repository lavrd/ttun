package logutils

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
)

const MessageDelimeter = "|"

type CtxKey string

const SlogFields CtxKey = "slog_fields"

type SlogHandler struct {
	slog.Handler
	l *log.Logger
}

func NewSlogHandler(
	out io.Writer,
	opts *slog.HandlerOptions,
) *SlogHandler {
	return &SlogHandler{
		Handler: slog.NewTextHandler(out, opts),
		l:       log.New(out, "", 0),
	}
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(SlogFields).([]slog.Attr); ok {
		for _, v := range attrs {
			r.AddAttrs(v)
		}
	}
	fields := make(map[string]interface{}, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	attrs := []byte(MessageDelimeter + " ")
	for key, value := range fields {
		attrs = append(attrs, []byte(fmt.Sprintf(`%s="%s"; `, key, value))...)
	}
	attrs = attrs[:len(attrs)-2]
	timeStr := r.Time.Format("[15:05:05.000]")
	level := r.Level.String() + " " + MessageDelimeter
	h.l.Println(timeStr, level, r.Message, string(attrs))
	return nil
}

func CtxWithAttr(ctx context.Context, attr slog.Attr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if attrs, ok := ctx.Value(SlogFields).([]slog.Attr); ok {
		attrs = append(attrs, attr)
		return context.WithValue(ctx, SlogFields, attrs)
	}
	attrs := []slog.Attr{}
	attrs = append(attrs, attr)
	return context.WithValue(ctx, SlogFields, attrs)
}
