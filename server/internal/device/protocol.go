package device

import (
	"encoding/json"
	"errors"
	"strings"

	"linx-intercom/server/internal/ota"
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
	IsAborted bool   `json:"is_aborted,omitempty"`
}

func decodeControl(data []byte) (ControlMessage, error) {
	var message ControlMessage
	if err := json.Unmarshal(data, &message); err != nil || strings.TrimSpace(message.Type) == "" {
		return ControlMessage{}, errInvalidControl
	}
	return message, nil
}

func decodeDeviceInfo(data []byte, deviceID string) (ota.DeviceInfo, error) {
	var envelope struct {
		Type       string          `json:"type"`
		DeviceInfo json.RawMessage `json:"device_info"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Type != "device_info" || len(envelope.DeviceInfo) == 0 {
		return ota.DeviceInfo{}, errors.New("invalid device_info message")
	}
	var info ota.DeviceInfo
	if err := json.Unmarshal(envelope.DeviceInfo, &info); err != nil {
		return ota.DeviceInfo{}, errors.New("invalid device_info payload")
	}
	if err := ota.ValidateDeviceInfo(deviceID, info); err != nil {
		return ota.DeviceInfo{}, err
	}
	return info, nil
}
