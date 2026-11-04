package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"linx-intercom/server/internal/audio"
	"linx-intercom/server/internal/device"
	"linx-intercom/server/internal/linx"
	"linx-intercom/server/internal/mcp"
	"linx-intercom/server/internal/media"
	"linx-intercom/server/internal/store"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		if err := checkHealth(addr); err != nil {
			os.Exit(1)
		}
		return
	}
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
	databasePath := os.Getenv("DATABASE_PATH")
	if databasePath == "" {
		databasePath = "intercom.db"
	}
	db, err := store.Open(databasePath)
	if err != nil {
		logger.Error("database startup failed", "path", databasePath, "error", err)
		os.Exit(1)
	}
	defer db.Close()
	sessions := device.NewSessionManager(60 * time.Second)
	sessions.SetIdentityResolver(db.EnsureUser)
	sessions.SetLinxClientIDResolver(db.LinxClientIDForDevice)
	wakeConfig, wakeEnabled, err := parseWakeWordConfig()
	if err != nil {
		logger.Error("wake-word configuration invalid", "error", err)
		os.Exit(1)
	}
	if wakeEnabled {
		sessions.SetWakeWordFactory(func() (audio.WakeWordDetector, error) {
			engine, err := audio.NewSherpaKeywordEngine(wakeConfig)
			if err != nil {
				return nil, err
			}
			detector, err := audio.NewPCMKeywordDetector(engine)
			if err != nil {
				engine.Close()
				return nil, err
			}
			logger.Info("device wake-word detector initialized", "engine", "sherpa-onnx", "sample_rate", 16000, "device_model_instances", 1)
			return detector, nil
		}, device.WakeWordLookbackFrames)
	} else {
		logger.Warn("wake-word detection disabled; configure KWS_ENCODER, KWS_DECODER, KWS_JOINER, KWS_TOKENS, and KWS_KEYWORDS_FILE")
	}
	linxURL := os.Getenv("LINX_WS_URL")
	linxTokens, err := parseDeviceTokens(os.Getenv("LINX_DEVICE_TOKENS"))
	if err != nil {
		logger.Error("Linx device token configuration invalid", "error", err)
		os.Exit(1)
	}
	sessions.SetOnCreate(func(session *device.Session) {
		device.StartAudioRouting(session)
		token := linxTokens[session.DeviceID]
		if linxURL == "" || token == "" {
			logger.Warn("Linx virtual device not configured", "device_id", session.DeviceID)
			return
		}
		device.StartLinxSession(session, linx.Client{URL: linxURL, Token: token, ClientID: session.LinxClientID})
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("GET /device/ws", device.NewHandler(sessions, tokens))
	mcpToken := os.Getenv("MCP_API_TOKEN")
	if mcpToken == "" {
		logger.Warn("MCP_API_TOKEN is unset; MCP endpoints will reject requests")
	}
	janusConfig := media.Config{
		WebSocketURL: os.Getenv("JANUS_WS_URL"),
		APISecret:    os.Getenv("JANUS_API_SECRET"),
		AdminKey:     os.Getenv("JANUS_ADMIN_KEY"),
	}
	if janusConfig.WebSocketURL == "" {
		logger.Warn("JANUS_WS_URL is unset; room creation and joining will fail")
	}
	mcpHandler := mcp.NewHandler(db, func(userID string) bool { return sessions.IsUserOnline(userID) }, mcpToken, mcp.RoomHooks{
		Join: func(ctx context.Context, userID string, room store.Room) error {
			session := sessions.OnlineSession(userID)
			if session == nil {
				return errors.New("device session is offline")
			}
			joinCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := device.JoinMedia(joinCtx, session, janusConfig, room); err != nil {
				logger.Error("device room join failed", "device_id", session.DeviceID, "session_id", session.ID, "room_id", room.ID, "janus_room_id", room.JanusID, "error", err)
				return err
			}
			return nil
		},
		Destroy: func(ctx context.Context, room store.Room) error {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return media.DestroyRoom(cleanupCtx, janusConfig, room.JanusID)
		},
	})
	mux.Handle("GET /mcp", mcpHandler)
	mux.Handle("POST /mcp/message", mcpHandler)

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

func parseWakeWordConfig() (audio.SherpaConfig, bool, error) {
	config := audio.SherpaConfig{
		Encoder: os.Getenv("KWS_ENCODER"), Decoder: os.Getenv("KWS_DECODER"), Joiner: os.Getenv("KWS_JOINER"),
		Tokens: os.Getenv("KWS_TOKENS"), KeywordsFile: os.Getenv("KWS_KEYWORDS_FILE"), NumThreads: 1,
		KeywordsScore: 3.0, KeywordsThreshold: 0.1,
	}
	paths := []string{config.Encoder, config.Decoder, config.Joiner, config.Tokens, config.KeywordsFile}
	configured := 0
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			configured++
		}
	}
	if configured == 0 {
		return config, false, nil
	}
	if configured != len(paths) {
		return audio.SherpaConfig{}, false, errors.New("KWS encoder, decoder, joiner, tokens, and keywords file must be configured together")
	}
	if raw := os.Getenv("KWS_NUM_THREADS"); raw != "" {
		threads, err := strconv.Atoi(raw)
		if err != nil || threads < 1 || threads > 64 {
			return audio.SherpaConfig{}, false, errors.New("KWS_NUM_THREADS must be an integer from 1 to 64")
		}
		config.NumThreads = threads
	}
	return config, true, nil
}

func checkHealth(addr string) error {
	probeAddr := addr
	if strings.HasPrefix(probeAddr, ":") {
		probeAddr = "127.0.0.1" + probeAddr
	}
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + probeAddr + "/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("health endpoint returned non-200 status")
	}
	return nil
}

func parseDeviceTokens(raw string) (device.TokenMap, error) {
	tokens := make(device.TokenMap)
	if strings.TrimSpace(raw) == "" {
		return tokens, nil
	}
	if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
		return nil, errors.New("token configuration must be a JSON object mapping device IDs to bearer tokens")
	}
	for deviceID, token := range tokens {
		if strings.TrimSpace(deviceID) == "" || strings.TrimSpace(token) == "" {
			return nil, errors.New("token configuration cannot contain blank device IDs or tokens")
		}
	}
	return tokens, nil
}
