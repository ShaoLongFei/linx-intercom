package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"linx-intercom/server/internal/device"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	tokens, err := parseDeviceTokens(os.Getenv("DEVICE_TOKENS"))
	if err != nil {
		logger.Error("device token configuration invalid", "error", err)
		os.Exit(1)
	}
	if len(tokens) == 0 {
		logger.Warn("no device tokens configured; device websocket authentication will reject all clients")
	}
	sessions := device.NewSessionManager(60 * time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("GET /device/ws", device.NewHandler(sessions, tokens))

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("intercom server starting", "addr", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("intercom server stopped")
}

func parseDeviceTokens(raw string) (device.TokenMap, error) {
	tokens := make(device.TokenMap)
	if strings.TrimSpace(raw) == "" {
		return tokens, nil
	}
	if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
		return nil, errors.New("DEVICE_TOKENS must be a JSON object mapping device IDs to bearer tokens")
	}
	for deviceID, token := range tokens {
		if strings.TrimSpace(deviceID) == "" || strings.TrimSpace(token) == "" {
			return nil, errors.New("DEVICE_TOKENS cannot contain blank device IDs or tokens")
		}
	}
	return tokens, nil
}
