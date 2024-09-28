package logutils

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"time"
)

type CtxKey string

const SlogFields CtxKey = "slog_fields"

type SlogHandler struct {
	slog.Handler
	l     *log.Logger
	attrs []slog.Attr
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
	for _, v := range h.attrs {
		r.AddAttrs(v)
	}
	fields := make(map[string]interface{}, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	attrs := []byte(" ")
	for key, value := range fields {
		attrs = append(attrs, []byte(fmt.Sprintf(`%s="%v"; `, key, value))...)
	}
	// If there are no attributes this byte array is empty.
	if len(attrs) != 1 {
		attrs = attrs[:len(attrs)-2] // remove last semicolon and space
	}
	timeStr := r.Time.Format(time.RFC3339)
	level := r.Level.String() + " "
	h.l.Println(timeStr, level, r.Message, string(attrs))
	return nil
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SlogHandler{
		Handler: h.Handler,
		l:       h.l,
		attrs:   attrs,
	}
}

func CtxWithAttr(ctx context.Context, attr slog.Attr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if attrs, ok := ctx.Value(SlogFields).([]slog.Attr); ok {
		attrs = append(attrs, attr)
		return context.WithValue(ctx, SlogFields, attrs)
	}
	var attrs []slog.Attr
	attrs = append(attrs, attr)
	return context.WithValue(ctx, SlogFields, attrs)
}
