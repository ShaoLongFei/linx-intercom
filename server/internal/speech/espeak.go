package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"strings"
	"time"

	"linx-intercom/server/internal/logctx"
)

const (
	maxWAVBytes  = 2 << 20
	pcmFrameSize = 640
	pcmRate      = 16000
)

type Espeak struct {
	binary  string
	timeout time.Duration
	logger  *slog.Logger
}

func (e Espeak) Synthesize(ctx context.Context, activationCode string) ([][]byte, error) {
	if !validActivationCode(activationCode) {
		return nil, errors.New("activation code must contain exactly six digits")
	}
	binaryPath := e.binary
	if binaryPath == "" {
		binaryPath = "espeak-ng"
	}
	timeout := e.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	logger := e.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logctx.Logger(ctx, logger)
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt := "灵矽激活码：" + digitWords(activationCode)
	cmd := exec.CommandContext(ctx, binaryPath, "-v", "cmn", "--stdout", prompt)
	stderr := &limitedBuffer{limit: 16 * 1024}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open espeak-ng output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start espeak-ng: %w", err)
	}
	wav, readErr := io.ReadAll(io.LimitReader(stdout, maxWAVBytes+1))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("read espeak-ng output: %w", readErr)
	}
	if len(wav) > maxWAVBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("espeak-ng WAV output exceeds %d bytes", maxWAVBytes)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("espeak-ng timed out or was canceled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("espeak-ng failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	frames, err := convertWAV(wav)
	if err != nil {
		return nil, fmt.Errorf("convert espeak-ng WAV: %w", err)
	}
	logger.Info("activation prompt synthesized", "activation_code", activationCode, "prompt", prompt,
		"wav_bytes", len(wav), "pcm_frames", len(frames),
		"duration_ms", time.Since(startedAt).Milliseconds())
	for i, frame := range frames {
		logger.Info("activation prompt PCM frame", "activation_code", activationCode, "frame", i,
			"bytes", len(frame))
	}
	return frames, nil
}

func digitWords(code string) string {
	words := [...]string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}
	var result strings.Builder
	for i, digit := range code {
		if i > 0 {
			result.WriteRune('，')
		}
		result.WriteString(words[digit-'0'])
	}
	return result.String()
}

func validActivationCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func convertWAV(wav []byte) ([][]byte, error) {
	if len(wav) < 12 || len(wav) > maxWAVBytes || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("invalid RIFF/WAVE header or size")
	}
	riffEnd := uint64(binary.LittleEndian.Uint32(wav[4:8])) + 8
	if riffEnd < 12 || riffEnd > uint64(len(wav)) {
		return nil, errors.New("RIFF length exceeds WAV data")
	}
	var sampleRate uint32
	var pcm []byte
	hasFormat := false
	hasData := false
	for offset := uint64(12); offset < riffEnd; {
		if riffEnd-offset < 8 {
			return nil, errors.New("truncated WAV chunk header")
		}
		chunkStart := int(offset)
		chunkSize := uint64(binary.LittleEndian.Uint32(wav[chunkStart+4 : chunkStart+8]))
		chunkDataStart := offset + 8
		chunkEnd := chunkDataStart + chunkSize
		paddedEnd := chunkEnd + chunkSize%2
		if chunkEnd < chunkDataStart || paddedEnd > riffEnd {
			return nil, errors.New("WAV chunk exceeds RIFF length")
		}
		chunk := wav[int(chunkDataStart):int(chunkEnd)]
		switch string(wav[chunkStart : chunkStart+4]) {
		case "fmt ":
			if hasFormat || len(chunk) < 16 {
				return nil, errors.New("invalid or duplicate WAV format chunk")
			}
			format := binary.LittleEndian.Uint16(chunk[0:2])
			channels := binary.LittleEndian.Uint16(chunk[2:4])
			sampleRate = binary.LittleEndian.Uint32(chunk[4:8])
			byteRate := binary.LittleEndian.Uint32(chunk[8:12])
			blockAlign := binary.LittleEndian.Uint16(chunk[12:14])
			bits := binary.LittleEndian.Uint16(chunk[14:16])
			if format != 1 || channels != 1 || bits != 16 || sampleRate == 0 || byteRate != sampleRate*2 || blockAlign != 2 {
				return nil, errors.New("WAV must contain PCM16 mono audio")
			}
			hasFormat = true
		case "data":
			if hasData || len(chunk)%2 != 0 {
				return nil, errors.New("invalid or duplicate WAV data chunk")
			}
			pcm = chunk
			hasData = true
		}
		offset = paddedEnd
	}
	if !hasFormat || !hasData || len(pcm) == 0 {
		return nil, errors.New("WAV is missing a format or non-empty data chunk")
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
	}
	resampled := resample(samples, sampleRate)
	if len(resampled) == 0 {
		return nil, errors.New("WAV is too short to resample")
	}
	output := make([]byte, len(resampled)*2)
	for i, sample := range resampled {
		binary.LittleEndian.PutUint16(output[i*2:i*2+2], uint16(sample))
	}
	frames := make([][]byte, 0, (len(output)+pcmFrameSize-1)/pcmFrameSize)
	for offset := 0; offset < len(output); offset += pcmFrameSize {
		frame := make([]byte, pcmFrameSize)
		copy(frame, output[offset:min(offset+pcmFrameSize, len(output))])
		frames = append(frames, frame)
	}
	return frames, nil
}

func resample(input []int16, inputRate uint32) []int16 {
	outputSize := int(uint64(len(input)) * pcmRate / uint64(inputRate))
	output := make([]int16, outputSize)
	for i := range output {
		position := float64(i) * float64(inputRate) / pcmRate
		left := int(position)
		right := min(left+1, len(input)-1)
		fraction := position - float64(left)
		value := float64(input[left])*(1-fraction) + float64(input[right])*fraction
		output[i] = int16(math.Round(value))
	}
	return output
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = b.buffer.Write(data[:remaining])
		} else {
			_, _ = b.buffer.Write(data)
		}
	}
	return len(data), nil
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}
