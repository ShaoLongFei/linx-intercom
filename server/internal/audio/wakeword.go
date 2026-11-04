package audio

import (
	"encoding/binary"
	"errors"
	"sync"
)

// WakeWordDetector 由可部署的关键字模型实现；未配置模型时必须显式禁用，不能用 VAD 冒充唤醒词识别。
type WakeWordDetector interface {
	Process(frame []byte) (bool, error)
}

type DisabledDetector struct{}

func (DisabledDetector) Process([]byte) (bool, error) {
	return false, nil
}

type KeywordEngine interface {
	Process([]float32) (string, error)
	Close()
}

type SherpaConfig struct {
	Encoder           string
	Decoder           string
	Joiner            string
	Tokens            string
	KeywordsFile      string
	NumThreads        int
	KeywordsScore     float32
	KeywordsThreshold float32
}

type PCMKeywordDetector struct {
	mu     sync.Mutex
	engine KeywordEngine
	closed bool
}

func NewPCMKeywordDetector(engine KeywordEngine) (*PCMKeywordDetector, error) {
	if engine == nil {
		return nil, errors.New("keyword engine is required")
	}
	return &PCMKeywordDetector{engine: engine}, nil
}

func (d *PCMKeywordDetector) Process(frame []byte) (bool, error) {
	if len(frame) == 0 || len(frame)%2 != 0 {
		return false, errors.New("wake-word input must be non-empty PCM16")
	}
	samples := make([]float32, len(frame)/2)
	for i := range samples {
		sample := int16(binary.LittleEndian.Uint16(frame[i*2 : i*2+2]))
		samples[i] = float32(sample) / 32768
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false, errors.New("wake-word detector is closed")
	}
	keyword, err := d.engine.Process(samples)
	return keyword != "", err
}

func (d *PCMKeywordDetector) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.engine.Close()
	}
}
