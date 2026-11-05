package speech

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConvertWAVSplitsSixteenKHzPCMIntoDeviceFrames(t *testing.T) {
	pcm := makePCM(320)
	frames, err := convertWAV(makeWAV(16000, 1, 16, pcm))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || len(frames[0]) != 640 || !bytes.Equal(frames[0], pcm) {
		t.Fatalf("frames = %d, first size = %d", len(frames), len(frames[0]))
	}
}

func TestConvertWAVResamplesAndPadsFinalDeviceFrame(t *testing.T) {
	pcm := makePCM(441)
	frames, err := convertWAV(makeWAV(22050, 1, 16, pcm))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || len(frames[0]) != 640 {
		t.Fatalf("resampled frames = %d, first size = %d", len(frames), len(frames[0]))
	}
}

func TestConvertWAVRejectsInvalidFormatsAndChunks(t *testing.T) {
	valid := makeWAV(16000, 1, 16, makePCM(320))
	badChunk := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint32(badChunk[40:44], uint32(len(valid)))
	missingData := append([]byte(nil), valid[:36]...)
	binary.LittleEndian.PutUint32(missingData[4:8], uint32(len(missingData)-8))
	oddPCM := makeWAV(16000, 1, 16, []byte{1})
	tooLarge := make([]byte, maxWAVBytes+1)
	cases := []struct {
		name string
		wav  []byte
	}{
		{name: "empty", wav: nil},
		{name: "invalid RIFF", wav: []byte("NOPE")},
		{name: "non PCM", wav: makeWAV(16000, 1, 3, makePCM(320))},
		{name: "stereo", wav: makeWAV(16000, 2, 16, makePCM(320))},
		{name: "odd sample bytes", wav: oddPCM},
		{name: "missing data chunk", wav: missingData},
		{name: "malformed data chunk", wav: badChunk},
		{name: "oversized", wav: tooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convertWAV(tc.wav); err == nil {
				t.Fatal("invalid WAV was accepted")
			}
		})
	}
}

func TestEspeakRunsChinesePromptAsOneArgument(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "fixture.wav")
	argsPath := filepath.Join(dir, "args.txt")
	wav := makeWAV(16000, 1, 16, makePCM(320))
	if err := os.WriteFile(wavPath, wav, 0600); err != nil {
		t.Fatal(err)
	}
	binaryPath := writeFakeEspeak(t, dir, "printf '%s\\n' \"$@\" > \"$ARGS_FILE\"\ncat \"$WAV_FILE\"")
	t.Setenv("ARGS_FILE", argsPath)
	t.Setenv("WAV_FILE", wavPath)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	frames, err := (Espeak{binary: binaryPath}).Synthesize(context.Background(), "123456")
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "-v\ncmn\n--stdout\n灵矽激活码：一，二，三，四，五，六\n"
	if string(args) != want {
		t.Fatalf("espeak args = %q, want %q", args, want)
	}
	for _, encoded := range []string{base64.StdEncoding.EncodeToString(wav), base64.StdEncoding.EncodeToString(frames[0])} {
		if !strings.Contains(logs.String(), encoded) {
			t.Fatalf("logs omit complete audio value %q", encoded)
		}
	}
}

func TestEspeakRejectsInvalidCodeWithoutExecutingCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	binaryPath := writeFakeEspeak(t, t.TempDir(), "touch \"$MARKER\"\n")
	t.Setenv("MARKER", marker)
	if _, err := (Espeak{binary: binaryPath}).Synthesize(context.Background(), "123456; touch /tmp/pwned"); err == nil {
		t.Fatal("invalid activation code was accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fake executable ran for invalid code: %v", err)
	}
}

func TestEspeakEnforcesTimeoutAndOutputLimit(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		binaryPath := writeFakeEspeak(t, t.TempDir(), "exec sleep 5\n")
		if _, err := (Espeak{binary: binaryPath, timeout: 30 * time.Millisecond}).Synthesize(context.Background(), "123456"); err == nil {
			t.Fatal("timed out command was accepted")
		}
	})
	t.Run("output limit", func(t *testing.T) {
		binaryPath := writeFakeEspeak(t, t.TempDir(), "dd if=/dev/zero bs=1048576 count=3 2>/dev/null\n")
		if _, err := (Espeak{binary: binaryPath}).Synthesize(context.Background(), "123456"); err == nil {
			t.Fatal("oversized command output was accepted")
		}
	})
}

func TestEspeakReportsNonzeroExit(t *testing.T) {
	binaryPath := writeFakeEspeak(t, t.TempDir(), "echo synthesis-failed >&2\nexit 7\n")
	if _, err := (Espeak{binary: binaryPath}).Synthesize(context.Background(), "123456"); err == nil || !strings.Contains(err.Error(), "synthesis-failed") {
		t.Fatalf("synthesis error = %v", err)
	}
}

func makePCM(samples int) []byte {
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(i*10-1000)))
	}
	return pcm
}

func makeWAV(sampleRate uint32, channels, bits uint16, pcm []byte) []byte {
	format := make([]byte, 16)
	binary.LittleEndian.PutUint16(format[0:2], 1)
	binary.LittleEndian.PutUint16(format[2:4], channels)
	binary.LittleEndian.PutUint32(format[4:8], sampleRate)
	binary.LittleEndian.PutUint32(format[8:12], sampleRate*uint32(channels)*uint32(bits)/8)
	binary.LittleEndian.PutUint16(format[12:14], channels*bits/8)
	binary.LittleEndian.PutUint16(format[14:16], bits)
	wav := []byte("RIFF")
	wav = append(wav, make([]byte, 4)...)
	wav = append(wav, []byte("WAVEfmt ")...)
	wav = append(wav, 16, 0, 0, 0)
	wav = append(wav, format...)
	wav = append(wav, []byte("data")...)
	dataLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(dataLen, uint32(len(pcm)))
	wav = append(wav, dataLen...)
	wav = append(wav, pcm...)
	binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
	return wav
}

func writeFakeEspeak(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "espeak-ng")
	script := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
