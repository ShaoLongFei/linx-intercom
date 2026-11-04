package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

type fakeKeywordEngine struct {
	samples []float32
	keyword string
	closed  bool
	err     error
}

func (e *fakeKeywordEngine) Process(samples []float32) (string, error) {
	e.samples = append([]float32(nil), samples...)
	return e.keyword, e.err
}

func (e *fakeKeywordEngine) Close() { e.closed = true }

func TestPCMKeywordDetectorConvertsPCM16AndReportsKeyword(t *testing.T) {
	engine := &fakeKeywordEngine{keyword: "wake"}
	detector, err := NewPCMKeywordDetector(engine)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 4)
	minSample := int16(-32768)
	positiveSample := int16(16384)
	binary.LittleEndian.PutUint16(frame, uint16(minSample))
	binary.LittleEndian.PutUint16(frame[2:], uint16(positiveSample))
	detected, err := detector.Process(frame)
	if err != nil || !detected {
		t.Fatalf("Process() = %v, %v; want detected", detected, err)
	}
	if len(engine.samples) != 2 || engine.samples[0] != -1 || math.Abs(float64(engine.samples[1]-0.5)) > 1e-6 {
		t.Fatalf("normalized PCM = %v", engine.samples)
	}
	detector.Close()
	detector.Close()
	if !engine.closed {
		t.Fatal("keyword engine was not released")
	}
}

func TestPCMKeywordDetectorRejectsInvalidFramesAndClosedUse(t *testing.T) {
	engine := &fakeKeywordEngine{}
	detector, err := NewPCMKeywordDetector(engine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := detector.Process([]byte{1}); err == nil {
		t.Fatal("odd-sized PCM16 frame accepted")
	}
	detector.Close()
	if _, err := detector.Process([]byte{0, 0}); err == nil {
		t.Fatal("closed detector accepted audio")
	}
}

func TestPCMKeywordDetectorReturnsEngineError(t *testing.T) {
	detector, err := NewPCMKeywordDetector(&fakeKeywordEngine{err: errors.New("engine failed")})
	if err != nil {
		t.Fatal(err)
	}
	defer detector.Close()
	if _, err := detector.Process([]byte{0, 0}); err == nil {
		t.Fatal("engine error was discarded")
	}
}
