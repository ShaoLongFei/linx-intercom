package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"linx-intercom/server/internal/device"
)

func TestReadPCM16WAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.wav")
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := os.WriteFile(path, makeWAV(pcm, 16000, 1, 16), 0600); err != nil {
		t.Fatal(err)
	}

	got, format, err := readPCM16WAV(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("PCM = %v, want %v", got, pcm)
	}
	if format.SampleRate != 16000 || format.Channels != 1 || format.BitsPerSample != 16 {
		t.Fatalf("format = %+v", format)
	}
}

func TestReadPCM16WAVRejectsUnsupportedFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stereo.wav")
	if err := os.WriteFile(path, makeWAV([]byte{0, 0, 0, 0}, 16000, 2, 16), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPCM16WAV(path); err == nil {
		t.Fatal("stereo PCM accepted")
	}
}

func TestSimulatorStreamsAndReconnects(t *testing.T) {
	manager := device.NewSessionManager(time.Second)
	server := httptest.NewServer(device.NewHandler(manager, device.TokenMap{"sim-device": "sim-secret"}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/device/ws"
	pcm := make([]byte, 640*8)
	output := filepath.Join(t.TempDir(), "downlink.pcm")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- run(ctx, url, "sim-device", "sim-secret", pcm, 40*time.Millisecond, output)
	}()
	deadline := time.After(2 * time.Second)
	var initialGeneration uint64
	reconnected := false
	audioFrames := 0
	for {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
			if manager.Current("sim-device") == nil {
				t.Fatal("simulator did not create a device session")
			}
			if !reconnected {
				t.Fatal("simulator did not establish a second connection")
			}
			if audioFrames == 0 {
				t.Fatal("simulator did not send any binary audio frames")
			}
			return
		case <-deadline:
			t.Fatal("simulator did not finish audio and reconnect")
		case <-time.After(time.Millisecond):
			if session := manager.Current("sim-device"); session != nil {
				select {
				case <-session.AudioIn:
					audioFrames++
				default:
				}
				generation := session.ConnectionGeneration()
				if initialGeneration == 0 {
					initialGeneration = generation
				}
				if generation > initialGeneration {
					reconnected = true
				}
			}
		}
	}
}

func makeWAV(pcm []byte, sampleRate uint32, channels, bits uint16) []byte {
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(36+len(pcm)))
	out.WriteString("WAVEfmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint32(16))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, channels)
	_ = binary.Write(&out, binary.LittleEndian, sampleRate)
	_ = binary.Write(&out, binary.LittleEndian, sampleRate*uint32(channels)*uint32(bits)/8)
	_ = binary.Write(&out, binary.LittleEndian, channels*bits/8)
	_ = binary.Write(&out, binary.LittleEndian, bits)
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(pcm)))
	out.Write(pcm)
	return out.Bytes()
}
