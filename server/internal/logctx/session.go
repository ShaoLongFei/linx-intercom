package logctx

import (
	"context"
	"log/slog"
)

type sessionIDKey struct{}

func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

func Logger(ctx context.Context, logger *slog.Logger, attrs ...any) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	if sessionID, _ := ctx.Value(sessionIDKey{}).(string); sessionID != "" {
		attrs = append(attrs, "session_id", sessionID)
	}
	return logger.With(attrs...)
}
