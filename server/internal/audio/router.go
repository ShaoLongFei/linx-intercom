package audio

import (
	"log/slog"
	"sync"
)

type Mode uint8

const (
	ModeRoom Mode = iota
	ModeControl
)

type Transition uint8

const (
	TransitionNone Transition = iota
	TransitionWakeDetected
	TransitionDetectionError
)

type Router struct {
	mu            sync.Mutex
	detector      WakeWordDetector
	lookback      int
	mode          Mode
	buffer        [][]byte
	roomAudio     chan []byte
	linxAudio     chan []byte
	janusDownlink chan []byte
	linxDownlink  chan []byte
}

func NewRouter(detector WakeWordDetector, lookbackFrames int) *Router {
	if lookbackFrames < 1 {
		lookbackFrames = 1
	}
	if detector == nil {
		detector = DisabledDetector{}
	}
	return &Router{
		detector:      detector,
		lookback:      lookbackFrames,
		roomAudio:     make(chan []byte, 32),
		linxAudio:     make(chan []byte, 32),
		janusDownlink: make(chan []byte, 32),
		linxDownlink:  make(chan []byte, 32),
	}
}

func (r *Router) Process(frame []byte) Transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode == ModeControl {
		r.enqueue(r.linxAudio, frame, "Linx uplink")
		return TransitionNone
	}
	copyOfFrame := append([]byte(nil), frame...)
	detected, err := r.detector.Process(copyOfFrame)
	if err != nil {
		r.buffer = nil
		slog.Warn("wake-word detection failed; audio frame withheld", "error", err)
		return TransitionDetectionError
	}
	r.buffer = append(r.buffer, copyOfFrame)
	if detected {
		// 检测器在整帧之后才给出结果，缓存中的帧可能包含唤醒词，整段丢弃以避免泄漏。
		r.buffer = nil
		r.mode = ModeControl
		clearQueue(r.janusDownlink)
		slog.Info("voice route changed", "mode", "control")
		return TransitionWakeDetected
	}
	if len(r.buffer) > r.lookback {
		oldest := r.buffer[0]
		r.buffer[0] = nil
		r.buffer = r.buffer[1:]
		r.enqueue(r.roomAudio, oldest, "Janus uplink")
	}
	return TransitionNone
}

func (r *Router) CompleteControl() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode != ModeControl {
		return false
	}
	r.mode = ModeRoom
	r.buffer = nil
	clearQueue(r.janusDownlink)
	clearQueue(r.linxDownlink)
	slog.Info("voice route changed", "mode", "room", "reason", "control_complete")
	return true
}

func (r *Router) Mode() Mode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode
}

func (r *Router) RoomAudio() <-chan []byte     { return r.roomAudio }
func (r *Router) LinxAudio() <-chan []byte     { return r.linxAudio }
func (r *Router) JanusDownlink() <-chan []byte { return r.janusDownlink }
func (r *Router) LinxDownlink() <-chan []byte  { return r.linxDownlink }

func (r *Router) EnqueueJanusDownlink(frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode == ModeRoom {
		r.enqueue(r.janusDownlink, frame, "device room downlink")
	}
}

func (r *Router) EnqueueLinxDownlink(frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode == ModeControl {
		r.enqueue(r.linxDownlink, frame, "device Linx downlink")
	}
}

func (r *Router) ClearJanusDownlink() {
	r.mu.Lock()
	clearQueue(r.janusDownlink)
	r.mu.Unlock()
}

func (r *Router) enqueue(queue chan []byte, frame []byte, name string) {
	copyOfFrame := append([]byte(nil), frame...)
	select {
	case queue <- copyOfFrame:
	default:
		select {
		case <-queue:
		default:
		}
		select {
		case queue <- copyOfFrame:
		default:
		}
		slog.Warn("audio route queue dropped stale frame", "queue", name)
	}
}

func clearQueue(queue chan []byte) {
	for {
		select {
		case <-queue:
		default:
			return
		}
	}
}
