package linx

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDownlinkDecodesOpusAndFramesDevicePCM(t *testing.T) {
	// FFmpeg/libopus：24 kHz mono、440 Hz 合成音、VoIP 16 kbit/s CBR，分别编码 20/40/60 ms。
	// 覆盖真实压缩包到 16 kHz PCM 的转换；不依赖外部录音、FFmpeg 或本机 libopus 才能跑测试。
	for _, tc := range []struct {
		name   string
		packet string
		frames int
	}{
		{"20ms", "aIICsDXtEa4Hiu5g6R4d385gDeJLmQ0Pw3kC8PgaMl5qWLDEO490pQ==", 1},
		{"40ms", "a0IBgi+DNe0RrgmBanEswrJgcMfUS9FSq0qYTl9Uc3v+/DNQOqMfulKqfoDZcDBlR/PsrJUIR7WQvJPmA/JgLifA9gbKpcOWoxw2rIFxtgA=", 2},
		{"60ms", "a0MDgi+DNe0RrgmBanEswrJgcMfUS9FSq0qYTl9Uc3v+/DNQOqMfulKqfoDZcDBlR/PsrJUIR7WQvJPmA/JgLifA9gbKpcOWoxw2rIFxtpuzvnR0IEAZYKSj9rCAhNa+3WqFQxvn3M/+1GLXky8TTanwCKnCAAAA", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoder, err := newDownlinkDecoder(audioParams{Format: "opus", SampleRate: 24000, Channels: 1})
			if err != nil {
				t.Fatal(err)
			}
			packet, err := base64.StdEncoding.DecodeString(tc.packet)
			if err != nil {
				t.Fatal(err)
			}
			frames, err := decoder.decode(packet)
			if err != nil || len(frames) != tc.frames {
				t.Fatalf("decoded frames = %d, want %d; error = %v", len(frames), tc.frames, err)
			}
			var output []byte
			for _, frame := range frames {
				if len(frame) != 640 {
					t.Fatalf("device PCM frame bytes = %d, want 640", len(frame))
				}
				output = append(output, frame...)
			}
			if bytes.Equal(output, make([]byte, len(output))) {
				t.Fatal("non-silent Opus input became silent PCM")
			}
		})
	}
}

func TestDownlinkRejectsUnsupportedParametersAndInvalidPackets(t *testing.T) {
	for _, params := range []audioParams{
		{Format: "mp3"},
		{Format: "opus", Channels: 2},
		{Format: "opus", SampleRate: 12345},
		{Format: "pcm", SampleRate: 24000, Channels: 1},
	} {
		if _, err := newDownlinkDecoder(params); err == nil {
			t.Fatalf("unsupported audio parameters accepted: %+v", params)
		}
	}
	decoder, err := newDownlinkDecoder(audioParams{})
	if err != nil {
		t.Fatal(err)
	}
	if frames, err := decoder.decode(nil); err == nil || len(frames) != 0 {
		t.Fatalf("invalid Opus was passed through: %d frames, %v", len(frames), err)
	}
	pcmDecoder, err := newDownlinkDecoder(audioParams{Format: "pcm", SampleRate: 16000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	if frames, err := pcmDecoder.decode([]byte{1, 2}); err == nil || len(frames) != 0 {
		t.Fatalf("partial PCM frame accepted: %d frames, %v", len(frames), err)
	}
	input := bytes.Repeat([]byte{1, 2}, 640)
	frames, err := pcmDecoder.decode(input)
	if err != nil || len(frames) != 2 || !bytes.Equal(bytes.Join(frames, nil), input) {
		t.Fatalf("PCM frames = %d, error = %v", len(frames), err)
	}
}
