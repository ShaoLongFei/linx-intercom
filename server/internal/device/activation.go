package device

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"linx-intercom/server/internal/linx"
	"linx-intercom/server/internal/ota"
)

type OTAProvider interface {
	Report(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error)
	CheckActivation(context.Context, string) (bool, error)
}

type ActivationSpeaker interface {
	Synthesize(context.Context, string) ([][]byte, error)
}

type activationTiming struct {
	retryInitial   time.Duration
	retryMax       time.Duration
	pollInitial    time.Duration
	pollMax        time.Duration
	promptInterval time.Duration
}

var defaultActivationTiming = activationTiming{
	retryInitial:   time.Second,
	retryMax:       30 * time.Second,
	pollInitial:    5 * time.Second,
	pollMax:        30 * time.Second,
	promptInterval: 60 * time.Second,
}

func StartOTADeviceSession(session *Session, provider OTAProvider, speaker ActivationSpeaker) {
	startOTADeviceSession(session, provider, speaker, defaultActivationTiming)
}

func startOTADeviceSession(session *Session, provider OTAProvider, speaker ActivationSpeaker, timing activationTiming) {
	session.activationOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-session.Done:
				cancel()
			case <-ctx.Done():
			}
		}()
		go func() {
			defer cancel()
			runOTADeviceSession(ctx, session, provider, speaker, timing)
		}()
	})
}

func runOTADeviceSession(ctx context.Context, session *Session, provider OTAProvider, speaker ActivationSpeaker, timing activationTiming) {
	if provider == nil {
		slog.Error("Linx OTA session cannot start without provider", "device_id", session.DeviceID, "session_id", session.ID)
		return
	}
	if timing.retryInitial <= 0 {
		timing = defaultActivationTiming
	}
	var info ota.DeviceInfo
	var spokenCode string
	var nextPrompt time.Time
	for {
		if info.Report.MACAddress == "" {
			select {
			case <-ctx.Done():
				return
			case info = <-session.DeviceInfo:
				slog.Info("device OTA metadata ready", "device_id", session.DeviceID, "session_id", session.ID,
					"activation_version", info.ActivationVersion, "serial_number", info.SerialNumber,
					"metadata", info)
			}
		}

		backoff := timing.retryInitial
		for {
			userAgent := info.Report.Application.Name + "/" + info.Report.Application.Version
			slog.Info("Linx OTA registration started", "device_id", session.DeviceID, "session_id", session.ID,
				"client_id", session.LinxClientID, "user_agent", userAgent)
			binding, err := provider.Report(ctx, session.DeviceID, session.LinxClientID, userAgent, info)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("Linx OTA registration failed", "device_id", session.DeviceID, "session_id", session.ID,
					"error", err, "retry_seconds", backoff.Seconds())
				info, _ = waitRetryOrMetadata(ctx, session, info, backoff)
				if ctx.Err() != nil {
					return
				}
				backoff = min(backoff*2, timing.retryMax)
				continue
			}
			slog.Info("Linx OTA registration state", "device_id", session.DeviceID, "session_id", session.ID,
				"activation_code", binding.ActivationCode, "websocket_url", binding.WebSocketURL,
				"websocket_token", binding.WebSocketToken)
			if binding.WebSocketURL != "" && binding.WebSocketToken != "" {
				startLinxPumps(ctx, session)
				client := linx.Client{URL: binding.WebSocketURL, Token: binding.WebSocketToken, ClientID: session.LinxClientID}
				if runLinxUntilRefresh(ctx, session, client, timing) {
					backoff = timing.retryInitial
					continue
				}
				return
			}
			if binding.ActivationCode == "" {
				slog.Error("Linx OTA returned incomplete binding state", "device_id", session.DeviceID,
					"session_id", session.ID, "activation_code", binding.ActivationCode,
					"websocket_url", binding.WebSocketURL, "websocket_token", binding.WebSocketToken,
					"retry_seconds", backoff.Seconds())
				info, _ = waitRetryOrMetadata(ctx, session, info, backoff)
				if ctx.Err() != nil {
					return
				}
				backoff = min(backoff*2, timing.retryMax)
				continue
			}
			slog.Info("Linx device is waiting for console binding", "device_id", session.DeviceID,
				"session_id", session.ID, "activation_code", binding.ActivationCode)
			var shouldRetry bool
			info, shouldRetry = waitForActivation(ctx, session, provider, speaker, info, binding.ActivationCode,
				&spokenCode, &nextPrompt, timing)
			if ctx.Err() != nil {
				return
			}
			if shouldRetry {
				backoff = timing.retryInitial
			}
			if info.Report.MACAddress == "" {
				return
			}
			break
		}
	}
}

func runLinxUntilRefresh(ctx context.Context, session *Session, client linx.Client, timing activationTiming) bool {
	backoff := timing.retryInitial
	for {
		slog.Info("Linx virtual device connection started", "device_id", session.DeviceID, "session_id", session.ID,
			"url", client.URL, "token", client.Token, "client_id", client.ClientID)
		err := client.Run(ctx, session.DeviceID, session.LinxAudioIn, session.LinxEventsIn, session.LinxOut)
		if ctx.Err() != nil {
			return false
		}
		var authErr *linx.AuthError
		if errors.As(err, &authErr) {
			slog.Warn("Linx authentication rejected; refreshing OTA credentials", "device_id", session.DeviceID,
				"session_id", session.ID, "status", authErr.StatusCode, "token", client.Token)
			return true
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("Linx session disconnected", "device_id", session.DeviceID, "session_id", session.ID,
				"url", client.URL, "token", client.Token, "error", err, "retry_seconds", backoff.Seconds())
		}
		if !waitContext(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, timing.retryMax)
	}
}

func waitForActivation(ctx context.Context, session *Session, provider OTAProvider, speaker ActivationSpeaker,
	info ota.DeviceInfo, code string, spokenCode *string, nextPrompt *time.Time, timing activationTiming) (ota.DeviceInfo, bool) {
	pollDelay := timing.pollInitial
	for {
		if code != *spokenCode || !time.Now().Before(*nextPrompt) {
			if err := speakActivationCode(ctx, session, speaker, code); err != nil {
				slog.Error("activation code speech failed", "device_id", session.DeviceID, "session_id", session.ID,
					"activation_code", code, "error", err, "retry_seconds", timing.promptInterval.Seconds())
			} else {
				slog.Info("activation code speech queued", "device_id", session.DeviceID, "session_id", session.ID,
					"activation_code", code)
			}
			*spokenCode = code
			*nextPrompt = time.Now().Add(timing.promptInterval)
		}
		pollTimer := time.NewTimer(pollDelay)
		promptDelay := time.Until(*nextPrompt)
		if promptDelay < 0 {
			promptDelay = 0
		}
		promptTimer := time.NewTimer(promptDelay)
		select {
		case <-ctx.Done():
			stopTimer(pollTimer)
			stopTimer(promptTimer)
			return ota.DeviceInfo{}, false
		case latest := <-session.DeviceInfo:
			stopTimer(pollTimer)
			stopTimer(promptTimer)
			slog.Info("device OTA metadata updated while awaiting binding", "device_id", session.DeviceID,
				"session_id", session.ID, "metadata", latest)
			return latest, false
		case <-promptTimer.C:
			stopTimer(pollTimer)
			continue
		case <-pollTimer.C:
			stopTimer(promptTimer)
		}

		bound, err := provider.CheckActivation(ctx, session.DeviceID)
		if ctx.Err() != nil {
			return ota.DeviceInfo{}, false
		}
		if err != nil {
			slog.Warn("Linx activation check failed", "device_id", session.DeviceID, "session_id", session.ID,
				"activation_code", code, "error", err, "next_poll_seconds", pollDelay.Seconds())
		} else {
			slog.Info("Linx activation state checked", "device_id", session.DeviceID, "session_id", session.ID,
				"activation_code", code, "bound", bound)
		}
		if err == nil && bound {
			slog.Info("Linx console binding detected; refreshing OTA credentials", "device_id", session.DeviceID,
				"session_id", session.ID, "activation_code", code)
			return info, true
		}
		pollDelay = min(pollDelay*2, timing.pollMax)
	}
}

func speakActivationCode(ctx context.Context, session *Session, speaker ActivationSpeaker, code string) error {
	if speaker == nil {
		return errors.New("activation speaker is unavailable")
	}
	frames, err := speaker.Synthesize(ctx, code)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return errors.New("activation speech produced no audio frames")
	}
	for index, frame := range frames {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case session.AudioOut <- downlinkMessage{
			audio: append([]byte(nil), frame...), mode: session.AudioRouter.Mode(), force: true,
		}:
			slog.Info("activation audio queued", "device_id", session.DeviceID, "session_id", session.ID,
				"activation_code", code, "frame", index, "bytes", len(frame))
		}
	}
	return nil
}

func waitRetryOrMetadata(ctx context.Context, session *Session, current ota.DeviceInfo, delay time.Duration) (ota.DeviceInfo, bool) {
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-ctx.Done():
		return ota.DeviceInfo{}, false
	case info := <-session.DeviceInfo:
		slog.Info("device OTA metadata updated", "device_id", session.DeviceID, "session_id", session.ID, "metadata", info)
		return info, true
	case <-timer.C:
		return current, false
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
