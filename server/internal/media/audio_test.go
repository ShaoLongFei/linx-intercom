package media

import (
	"encoding/binary"
	"testing"
)

func TestPCM16ToPCMUAndBackAtDeviceRate(t *testing.T) {
	pcm := make([]byte, 640)
	for i := 0; i < len(pcm)/2; i++ {
		sample := int16((i%80 - 40) * 400)
		pcm[i*2] = byte(sample)
		pcm[i*2+1] = byte(sample >> 8)
	}
	encoded := pcm16ToPCMU(pcm)
	if len(encoded) != 160 {
		t.Fatalf("PCMU bytes = %d, want 160 for 20ms at 8 kHz", len(encoded))
	}
	decoded := pcmuToPCM16(encoded)
	if len(decoded) != len(pcm) {
		t.Fatalf("decoded bytes = %d, want %d", len(decoded), len(pcm))
	}
	for i := 0; i < len(encoded); i++ {
		left := int16(binary.LittleEndian.Uint16(decoded[i*4 : i*4+2]))
		right := int16(binary.LittleEndian.Uint16(decoded[i*4+2 : i*4+4]))
		if left != right {
			t.Fatalf("8 kHz sample %d was not expanded to device rate", i)
		}
	}
}

func TestPCMConversionRejectsIncompleteSamples(t *testing.T) {
	if got := pcm16ToPCMU([]byte{1}); got != nil {
		t.Fatalf("odd PCM byte count converted to %v", got)
	}
}
