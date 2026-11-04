package linx

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/opus"
)

const deviceFrameSamples = 320 // 16 kHz × 20 ms，和 ESP32/模拟器的 PCM 帧保持一致。

type audioParams struct {
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type downlinkDecoder struct {
	format string
	opus   opus.Decoder
	pcm    [1920]int16 // Opus 单包最多 120 ms，按设备输出的 16 kHz 计算容量。
}

func newDownlinkDecoder(params audioParams) (*downlinkDecoder, error) {
	// hello 可以省略音频参数；官方 TTS 下行为 Opus，码流自身携带带宽/帧时长。
	if params.Format == "" {
		params.Format = "opus"
	}
	if params.Channels != 0 && params.Channels != 1 {
		return nil, fmt.Errorf("unsupported Linx downlink channels: %d", params.Channels)
	}
	decoder := &downlinkDecoder{format: params.Format}
	switch params.Format {
	case "opus":
		if params.SampleRate != 0 && params.SampleRate != 16000 && params.SampleRate != 24000 {
			return nil, fmt.Errorf("unsupported Linx Opus sample rate: %d", params.SampleRate)
		}
		// Opus 解码器直接输出 16 kHz，包含 24 kHz TTS 到设备采样率的转换。
		var err error
		decoder.opus, err = opus.NewDecoderWithOutput(16000, 1)
		if err != nil {
			return nil, fmt.Errorf("initialize Linx Opus decoder: %w", err)
		}
	case "pcm":
		if params.SampleRate != 16000 || params.Channels != 1 {
			return nil, fmt.Errorf("Linx PCM downlink must be mono 16000 Hz")
		}
	default:
		return nil, fmt.Errorf("unsupported Linx downlink format %q", params.Format)
	}
	return decoder, nil
}

func (d *downlinkDecoder) decode(packet []byte) ([][]byte, error) {
	pcm := packet
	if d.format == "opus" {
		n, err := d.opus.DecodeToInt16(packet, d.pcm[:])
		if err != nil {
			return nil, fmt.Errorf("decode Linx Opus packet: %w", err)
		}
		pcm = make([]byte, n*2)
		for i, sample := range d.pcm[:n] {
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
		}
	}
	if len(pcm) == 0 || len(pcm)%(deviceFrameSamples*2) != 0 {
		return nil, fmt.Errorf("Linx PCM output is not a whole 20 ms frame: bytes=%d", len(pcm))
	}
	// 40/60 ms TTS 包需拆成 2/3 帧；不能把压缩包或变长 PCM 交给固定帧长的 I2S 播放队列。
	frames := make([][]byte, 0, len(pcm)/(deviceFrameSamples*2))
	for len(pcm) != 0 {
		frames = append(frames, append([]byte(nil), pcm[:deviceFrameSamples*2]...))
		pcm = pcm[deviceFrameSamples*2:]
	}
	return frames, nil
}
