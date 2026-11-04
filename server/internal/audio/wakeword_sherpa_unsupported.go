//go:build !linux || (!amd64 && !arm64) || !cgo

package audio

import "errors"

func NewSherpaKeywordEngine(SherpaConfig) (KeywordEngine, error) {
	return nil, errors.New("sherpa-onnx wake-word detection requires Linux amd64/arm64 with CGO enabled")
}
