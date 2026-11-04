//go:build linux && (amd64 || arm64) && cgo

package audio

import (
	"errors"
	"fmt"
	"os"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

func NewSherpaKeywordEngine(config SherpaConfig) (KeywordEngine, error) {
	for name, path := range map[string]string{
		"encoder": config.Encoder, "decoder": config.Decoder, "joiner": config.Joiner,
		"tokens": config.Tokens, "keywords": config.KeywordsFile,
	} {
		if path == "" {
			return nil, fmt.Errorf("wake-word %s path is required", name)
		}
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("wake-word %s file: %w", name, err)
		}
	}
	threads := config.NumThreads
	if threads <= 0 {
		threads = 1
	}
	spotter := sherpa.NewKeywordSpotter(&sherpa.KeywordSpotterConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OnlineModelConfig{
			Transducer: sherpa.OnlineTransducerModelConfig{Encoder: config.Encoder, Decoder: config.Decoder, Joiner: config.Joiner},
			Tokens:     config.Tokens, NumThreads: threads, Provider: "cpu",
		},
		KeywordsFile: config.KeywordsFile, KeywordsScore: config.KeywordsScore,
		KeywordsThreshold: config.KeywordsThreshold,
	})
	if spotter == nil {
		return nil, errors.New("sherpa-onnx failed to load the wake-word model")
	}
	stream := sherpa.NewKeywordStream(spotter)
	if stream == nil {
		sherpa.DeleteKeywordSpotter(spotter)
		return nil, errors.New("sherpa-onnx failed to create a keyword stream")
	}
	return &sherpaKeywordEngine{spotter: spotter, stream: stream}, nil
}

type sherpaKeywordEngine struct {
	spotter *sherpa.KeywordSpotter
	stream  *sherpa.OnlineStream
}

func (e *sherpaKeywordEngine) Process(samples []float32) (string, error) {
	// Sherpa 要求归一化 float32 PCM；每个设备拥有独立 stream，避免会话状态互相污染。
	e.stream.AcceptWaveform(16000, samples)
	for e.spotter.IsReady(e.stream) {
		e.spotter.Decode(e.stream)
		result := e.spotter.GetResult(e.stream)
		if result.Keyword != "" {
			e.spotter.Reset(e.stream)
			return result.Keyword, nil
		}
	}
	return "", nil
}

func (e *sherpaKeywordEngine) Close() {
	sherpa.DeleteOnlineStream(e.stream)
	sherpa.DeleteKeywordSpotter(e.spotter)
}
