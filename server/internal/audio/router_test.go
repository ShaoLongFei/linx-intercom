package audio

import (
	"errors"
	"testing"
)

type fakeDetector struct {
	detected bool
	err      error
}

func (d *fakeDetector) Process([]byte) (bool, error) { return d.detected, d.err }

func TestRouterBuffersRoomAudioAndSeparatesWakeCommand(t *testing.T) {
	detector := &fakeDetector{}
	router := NewRouter(detector, 2)
	router.Process([]byte("room-1"))
	router.Process([]byte("room-2"))
	if got := receiveAll(router.RoomAudio()); len(got) != 0 {
		t.Fatalf("room audio escaped detection buffer early: %v", got)
	}
	router.Process([]byte("room-3"))
	if got := receiveAll(router.RoomAudio()); len(got) != 1 || string(got[0]) != "room-1" {
		t.Fatalf("room lookback flush = %q", got)
	}
	detector.detected = true
	if transition := router.Process([]byte("wake phrase")); transition != TransitionWakeDetected {
		t.Fatalf("transition = %v, want wake detected", transition)
	}
	if got := receiveAll(router.RoomAudio()); len(got) != 0 {
		t.Fatalf("wake phrase/lookback leaked to room: %q", got)
	}
	if router.Mode() != ModeControl {
		t.Fatalf("mode = %v, want control", router.Mode())
	}
	detector.detected = false
	router.Process([]byte("command"))
	if got := receiveAll(router.LinxAudio()); len(got) != 1 || string(got[0]) != "command" {
		t.Fatalf("control audio = %q", got)
	}
	if got := receiveAll(router.RoomAudio()); len(got) != 0 {
		t.Fatalf("command leaked to room: %q", got)
	}
}

func TestRouterDetectorErrorDoesNotRouteUnclassifiedAudio(t *testing.T) {
	detector := &fakeDetector{err: errors.New("model failed")}
	router := NewRouter(detector, 2)
	if transition := router.Process([]byte("frame")); transition != TransitionDetectionError {
		t.Fatalf("transition = %v", transition)
	}
	if len(router.RoomAudio()) != 0 || len(router.LinxAudio()) != 0 {
		t.Fatal("audio was routed after detector error")
	}
}

func TestRouterReturnsToRoomAndClearsStaleDownlink(t *testing.T) {
	detector := &fakeDetector{detected: true}
	router := NewRouter(detector, 2)
	router.Process([]byte("wake"))
	router.EnqueueJanusDownlink([]byte("stale room"))
	if !router.CompleteControl() {
		t.Fatal("control mode completion was ignored")
	}
	if router.Mode() != ModeRoom {
		t.Fatalf("mode = %v, want room", router.Mode())
	}
	if got := receiveAll(router.JanusDownlink()); len(got) != 0 {
		t.Fatalf("stale Janus audio remained after control mode: %q", got)
	}
	if router.CompleteControl() {
		t.Fatal("duplicate completion should not change state")
	}
}

func TestRouterRepeatedWakeDoesNotRestartControl(t *testing.T) {
	detector := &fakeDetector{detected: true}
	router := NewRouter(detector, 2)
	if router.Process([]byte("wake")) != TransitionWakeDetected {
		t.Fatal("first wake was not detected")
	}
	if router.Process([]byte("command")) != TransitionNone {
		t.Fatal("repeated wake in control mode triggered another transition")
	}
	if got := receiveAll(router.LinxAudio()); len(got) != 1 || string(got[0]) != "command" {
		t.Fatalf("audio after repeated wake = %q", got)
	}
}

func TestClearRoomAudioDropsPreTransitionLookbackAndQueuedFrames(t *testing.T) {
	router := NewRouter(&fakeDetector{}, 2)
	router.Process([]byte("before-room-1"))
	router.Process([]byte("before-room-2"))
	router.Process([]byte("before-room-3"))
	router.ClearRoomAudio()
	router.Process([]byte("after-room-1"))
	router.Process([]byte("after-room-2"))
	if got := receiveAll(router.RoomAudio()); len(got) != 0 {
		t.Fatalf("pre-transition audio escaped after room transition: %q", got)
	}
	router.Process([]byte("after-room-3"))
	if got := receiveAll(router.RoomAudio()); len(got) != 1 || string(got[0]) != "after-room-1" {
		t.Fatalf("new room lookback = %q, want only post-transition audio", got)
	}
}

func receiveAll[T any](queue <-chan T) []T {
	var result []T
	for {
		select {
		case item := <-queue:
			result = append(result, item)
		default:
			return result
		}
	}
}
