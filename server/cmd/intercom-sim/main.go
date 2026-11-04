package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const (
	pcmSampleRate  = 16000
	frameDuration  = 20 * time.Millisecond
	frameSizeBytes = pcmSampleRate * 2 * int(frameDuration/time.Millisecond) / 1000
)

type wavFormat struct {
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
}

func main() {
	serverURL := flag.String("server", "ws://localhost:8080/device/ws", "device WebSocket URL")
	deviceID := flag.String("device-id", "", "registered device ID")
	token := flag.String("token", os.Getenv("DEVICE_TOKEN"), "device bearer token (or DEVICE_TOKEN)")
	audioFile := flag.String("audio-file", "", "PCM16 mono 16 kHz WAV file to stream")
	reconnectAfter := flag.Duration("reconnect-after", 0, "force reconnect after this duration; 0 disables")
	outputFile := flag.String("output-file", "downlink.pcm", "file for received binary audio")
	flag.Parse()

	if *deviceID == "" || *token == "" || *audioFile == "" {
		flag.Usage()
		os.Exit(2)
	}
	pcm, _, err := readPCM16WAV(*audioFile)
	if err != nil {
		slog.Error("simulator audio input rejected", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *serverURL, *deviceID, *token, pcm, *reconnectAfter, *outputFile); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("simulator stopped", "device_id", *deviceID, "error", err)
		os.Exit(1)
	}
}

func readPCM16WAV(path string) ([]byte, wavFormat, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, wavFormat{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<20))
	if err != nil {
		return nil, wavFormat{}, err
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, wavFormat{}, errors.New("expected RIFF/WAVE file")
	}
	var format wavFormat
	var pcm []byte
	var encoding uint16
	for offset := 12; offset+8 <= len(data); {
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + chunkSize
		if chunkSize < 0 || end > len(data) {
			return nil, wavFormat{}, errors.New("truncated WAV chunk")
		}
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if chunkSize < 16 {
				return nil, wavFormat{}, errors.New("invalid WAV format chunk")
			}
			encoding = binary.LittleEndian.Uint16(data[start : start+2])
			format.Channels = binary.LittleEndian.Uint16(data[start+2 : start+4])
			format.SampleRate = binary.LittleEndian.Uint32(data[start+4 : start+8])
			format.BitsPerSample = binary.LittleEndian.Uint16(data[start+14 : start+16])
		case "data":
			pcm = append([]byte(nil), data[start:end]...)
		}
		offset = end + chunkSize%2
	}
	if encoding != 1 || format.Channels != 1 || format.SampleRate != pcmSampleRate || format.BitsPerSample != 16 || len(pcm) == 0 || len(pcm)%2 != 0 {
		return nil, wavFormat{}, errors.New("WAV must contain non-empty PCM16 mono 16000 Hz audio")
	}
	return pcm, format, nil
}

func run(ctx context.Context, serverURL, deviceID, token string, pcm []byte, reconnectAfter time.Duration, outputPath string) error {
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	header := httpHeader(deviceID, token)
	started := time.Now()
	for offset := 0; offset < len(pcm); {
		conn, _, err := websocket.Dial(ctx, serverURL, &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			return fmt.Errorf("connect device websocket: %w", err)
		}
		conn.SetReadLimit(4096)
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"listen","state":"start","mode":"realtime"}`)); err != nil {
			_ = conn.CloseNow()
			return fmt.Errorf("send listen event: %w", err)
		}
		receiveDone := make(chan struct{})
		go receiveDownlink(ctx, conn, output, deviceID, receiveDone)

		reconnect := reconnectAfter > 0 && time.Since(started) < reconnectAfter
		limit := len(pcm)
		if reconnect {
			framesUntilReconnect := int((reconnectAfter - time.Since(started)) / frameDuration)
			reconnectOffset := offset + max(framesUntilReconnect, 1)*frameSizeBytes
			if reconnectOffset < limit {
				limit = reconnectOffset
			} else {
				reconnect = false
			}
		}
		for offset < limit {
			end := min(offset+frameSizeBytes, limit)
			if err := conn.Write(ctx, websocket.MessageBinary, pcm[offset:end]); err != nil {
				_ = conn.CloseNow()
				return fmt.Errorf("send audio frame: %w", err)
			}
			offset = end
			select {
			case <-ctx.Done():
				_ = conn.CloseNow()
				return ctx.Err()
			case <-time.After(frameDuration):
			}
		}
		_ = conn.CloseNow()
		<-receiveDone
		if reconnect {
			slog.Info("simulator reconnecting", "device_id", deviceID, "audio_offset_bytes", offset)
		}
	}
	slog.Info("simulator audio input complete", "device_id", deviceID, "audio_bytes", len(pcm), "downlink_file", outputPath)
	return nil
}

func receiveDownlink(ctx context.Context, conn *websocket.Conn, output io.Writer, deviceID string, done chan<- struct{}) {
	defer close(done)
	defer conn.CloseNow()
	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if messageType == websocket.MessageBinary {
			if _, err := output.Write(payload); err != nil {
				slog.Error("simulator downlink save failed", "device_id", deviceID, "error", err)
				return
			}
		} else if messageType == websocket.MessageText {
			slog.Info("simulator control received", "device_id", deviceID, "message", strings.TrimSpace(string(payload)))
		}
	}
}

func httpHeader(deviceID, token string) http.Header {
	return http.Header{"Device-Id": []string{deviceID}, "Authorization": []string{"Bearer " + token}}
}
