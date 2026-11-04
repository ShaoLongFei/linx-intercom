package media

import "encoding/binary"

const (
	muLawBias = 0x84
	muLawClip = 32635
)

// PCMU 是 Janus 常见的窄带协商格式；设备侧仍使用 16 kHz PCM。
func pcm16ToPCMU(pcm []byte) []byte {
	if len(pcm) == 0 || len(pcm)%4 != 0 {
		return nil
	}
	encoded := make([]byte, len(pcm)/4)
	for i := range encoded {
		a := int(int16(binary.LittleEndian.Uint16(pcm[i*4 : i*4+2])))
		b := int(int16(binary.LittleEndian.Uint16(pcm[i*4+2 : i*4+4])))
		encoded[i] = linearToMuLaw((a + b) / 2)
	}
	return encoded
}

func pcmuToPCM16(pcmu []byte) []byte {
	decoded := make([]byte, len(pcmu)*4)
	for i, value := range pcmu {
		sample := muLawToLinear(value)
		binary.LittleEndian.PutUint16(decoded[i*4:i*4+2], uint16(sample))
		binary.LittleEndian.PutUint16(decoded[i*4+2:i*4+4], uint16(sample))
	}
	return decoded
}

func linearToMuLaw(sample int) byte {
	sign := 0
	if sample < 0 {
		sign = 0x80
		sample = -sample
	}
	if sample > muLawClip {
		sample = muLawClip
	}
	sample += muLawBias
	exponent := 7
	for mask := 0x4000; exponent > 0 && sample&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (sample >> (exponent + 3)) & 0x0f
	return ^byte(sign | exponent<<4 | mantissa)
}

func muLawToLinear(value byte) int16 {
	value = ^value
	magnitude := ((int(value) & 0x0f) << 3) + muLawBias
	magnitude <<= (int(value) & 0x70) >> 4
	if value&0x80 != 0 {
		return int16(muLawBias - magnitude)
	}
	return int16(magnitude - muLawBias)
}
