package device

import (
	"encoding/json"
	"errors"
	"strings"
)

const maxAudioFrameSize = 4096

var errInvalidControl = errors.New("invalid control message")

type ControlMessage struct {
	Type      string `json:"type"`
	State     string `json:"state,omitempty"`
	Mode      string `json:"mode,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	RoomID    string `json:"room_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

func decodeControl(data []byte) (ControlMessage, error) {
	var message ControlMessage
	if err := json.Unmarshal(data, &message); err != nil || strings.TrimSpace(message.Type) == "" {
		return ControlMessage{}, errInvalidControl
	}
	return message, nil
}
