package ota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"linx-intercom/server/internal/websocketurl"
)

const (
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 1 << 20
)

// DeviceInfo 是设备上报的 OTA 元数据；MAC 必须与已认证的 Device-Id 完全一致。
type DeviceInfo struct {
	ActivationVersion int           `json:"activation_version"`
	SerialNumber      string        `json:"serial_number,omitempty"`
	Report            ReportPayload `json:"report"`
}

type ReportPayload struct {
	Application    Application `json:"application"`
	Board          Board       `json:"board"`
	MACAddress     string      `json:"mac_address"`
	ChipModelName  string      `json:"chip_model_name,omitempty"`
	FlashSize      uint32      `json:"flash_size,omitempty"`
	ChipInfo       ChipInfo    `json:"chip_info,omitempty"`
	PartitionTable []Partition `json:"partition_table,omitempty"`
}

type Application struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	CompileTime string `json:"compile_time,omitempty"`
	IDFVersion  string `json:"idf_version,omitempty"`
	ELFSHA256   string `json:"elf_sha256"`
}

type Board struct {
	Type    string `json:"type"`
	SSID    string `json:"ssid"`
	RSSI    int    `json:"rssi"`
	Channel int    `json:"channel,omitempty"`
	IP      string `json:"ip,omitempty"`
	MAC     string `json:"mac,omitempty"`
}

type ChipInfo struct {
	Model    uint32 `json:"model"`
	Cores    uint32 `json:"cores"`
	Revision uint32 `json:"revision"`
	Features uint32 `json:"features"`
}

type Partition struct {
	Label   string `json:"label"`
	Type    uint32 `json:"type"`
	Subtype uint32 `json:"subtype"`
	Address uint32 `json:"address"`
	Size    uint32 `json:"size"`
}

type Binding struct {
	ActivationCode string
	WebSocketURL   string
	WebSocketToken string
}

type Client struct {
	endpoint string
	http     *http.Client
	logger   *slog.Logger
}

func NewClient() *Client {
	return newClient("https://xrobo.qiniuapi.com", &http.Client{Timeout: requestTimeout})
}

func newClient(endpoint string, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	if clientCopy.Timeout <= 0 {
		clientCopy.Timeout = requestTimeout
	}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), http: &clientCopy, logger: slog.Default()}
}

func (c *Client) Report(ctx context.Context, deviceID, clientID, userAgent string, info DeviceInfo) (Binding, error) {
	if err := validateDeviceInfo(deviceID, clientID, userAgent, info); err != nil {
		return Binding{}, err
	}
	body, err := json.Marshal(struct {
		Version        int               `json:"version"`
		UUID           string            `json:"uuid"`
		Application    Application       `json:"application"`
		OTA            map[string]string `json:"ota"`
		Board          Board             `json:"board"`
		FlashSize      uint32            `json:"flash_size,omitempty"`
		MACAddress     string            `json:"mac_address"`
		ChipModelName  string            `json:"chip_model_name,omitempty"`
		ChipInfo       ChipInfo          `json:"chip_info,omitempty"`
		PartitionTable []Partition       `json:"partition_table,omitempty"`
	}{
		Version: 0, UUID: clientID, Application: info.Report.Application,
		OTA:   map[string]string{"label": info.Report.Application.Name},
		Board: info.Report.Board, FlashSize: info.Report.FlashSize,
		MACAddress: deviceID, ChipModelName: info.Report.ChipModelName,
		ChipInfo: info.Report.ChipInfo, PartitionTable: info.Report.PartitionTable,
	})
	if err != nil {
		return Binding{}, fmt.Errorf("marshal Linx OTA report: %w", err)
	}
	requestURL, err := url.JoinPath(c.endpoint, "v1/ota/")
	if err != nil {
		return Binding{}, fmt.Errorf("build Linx OTA URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return Binding{}, fmt.Errorf("create Linx OTA request: %w", err)
	}
	activationVersion := fmt.Sprint(info.ActivationVersion)
	req.Header.Set("Activation-Version", activationVersion)
	req.Header.Set("Device-Id", deviceID)
	req.Header.Set("Client-Id", clientID)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Content-Type", "application/json")
	requestHeaders := map[string]string{
		"Activation-Version": activationVersion,
		"Device-Id":          deviceID,
		"Client-Id":          clientID,
		"User-Agent":         userAgent,
		"Accept-Language":    "zh-CN",
		"Content-Type":       "application/json",
	}
	if info.ActivationVersion == 2 {
		req.Header.Set("Serial-Number", info.SerialNumber)
		requestHeaders["Serial-Number"] = info.SerialNumber
	}
	c.logger.Info("Linx OTA request", "device_id", deviceID, "url", requestURL, "headers", requestHeaders, "body", string(body))
	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Error("Linx OTA request failed", "device_id", deviceID, "error", err)
		return Binding{}, fmt.Errorf("send Linx OTA request: %w", err)
	}
	responseBody, readErr := readResponse(resp)
	if readErr != nil {
		c.logger.Error("Linx OTA response read failed", "device_id", deviceID, "status", resp.StatusCode, "error", readErr)
		return Binding{}, readErr
	}
	c.logger.Info("Linx OTA response", "device_id", deviceID, "status", resp.StatusCode, "body", string(responseBody))
	if resp.StatusCode != http.StatusOK {
		return Binding{}, fmt.Errorf("Linx OTA returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var result struct {
		Error      string `json:"error"`
		Activation struct {
			Code string `json:"code"`
		} `json:"activation"`
		WebSocket struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		} `json:"websocket"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return Binding{}, fmt.Errorf("decode Linx OTA response: %w", err)
	}
	if strings.TrimSpace(result.Error) != "" {
		return Binding{}, fmt.Errorf("Linx OTA business error: %s", result.Error)
	}
	binding := Binding{
		ActivationCode: result.Activation.Code,
		WebSocketURL:   result.WebSocket.URL,
		WebSocketToken: result.WebSocket.Token,
	}
	if binding.WebSocketURL != "" {
		if err := websocketurl.Validate(binding.WebSocketURL); err != nil {
			return Binding{}, fmt.Errorf("Linx OTA returned invalid WebSocket URL: %w", err)
		}
	}
	if binding.WebSocketURL != "" && binding.WebSocketToken != "" {
		return binding, nil
	}
	if binding.WebSocketToken != "" || binding.ActivationCode == "" || !validActivationCode(binding.ActivationCode) {
		return Binding{}, errors.New("Linx OTA response has neither a valid activation code nor complete WebSocket credentials")
	}
	return binding, nil
}

func (c *Client) CheckActivation(ctx context.Context, deviceID string) (bool, error) {
	if _, err := net.ParseMAC(deviceID); err != nil {
		return false, fmt.Errorf("invalid Linx Device-Id %q: %w", deviceID, err)
	}
	requestURL, err := url.JoinPath(c.endpoint, "v1/ota/activate")
	if err != nil {
		return false, fmt.Errorf("build Linx activation URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, nil)
	if err != nil {
		return false, fmt.Errorf("create Linx activation request: %w", err)
	}
	req.Header.Set("Device-Id", deviceID)
	c.logger.Info("Linx activation check request", "device_id", deviceID, "url", requestURL, "headers", map[string]string{"Device-Id": deviceID})
	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Error("Linx activation check failed", "device_id", deviceID, "error", err)
		return false, fmt.Errorf("send Linx activation check: %w", err)
	}
	body, readErr := readResponse(resp)
	if readErr != nil {
		c.logger.Error("Linx activation response read failed", "device_id", deviceID, "status", resp.StatusCode, "error", readErr)
		return false, readErr
	}
	c.logger.Info("Linx activation check response", "device_id", deviceID, "status", resp.StatusCode, "body", string(body))
	if resp.StatusCode == http.StatusAccepted {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("Linx activation check returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if strings.TrimSpace(string(body)) == "success" {
		return true, nil
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &result) == nil && result.Code != 0 {
		return false, fmt.Errorf("Linx activation business error %d: %s", result.Code, result.Msg)
	}
	return false, fmt.Errorf("unexpected Linx activation response: %s", strings.TrimSpace(string(body)))
}

func validateDeviceInfo(deviceID, clientID, userAgent string, info DeviceInfo) error {
	if clientID == "" || userAgent == "" {
		return errors.New("Linx Client-Id and User-Agent are required")
	}
	return ValidateDeviceInfo(deviceID, info)
}

func ValidateDeviceInfo(deviceID string, info DeviceInfo) error {
	if _, err := net.ParseMAC(deviceID); err != nil {
		return fmt.Errorf("invalid authenticated Device-Id %q: %w", deviceID, err)
	}
	if info.ActivationVersion != 1 && info.ActivationVersion != 2 {
		return fmt.Errorf("unsupported activation version %d", info.ActivationVersion)
	}
	if (info.ActivationVersion == 1 && info.SerialNumber != "") || (info.ActivationVersion == 2 && strings.TrimSpace(info.SerialNumber) == "") {
		return errors.New("activation version and serial number do not match")
	}
	if info.Report.MACAddress != deviceID {
		return errors.New("device_info MAC does not match authenticated Device-Id")
	}
	app := info.Report.Application
	if strings.TrimSpace(app.Name) == "" || strings.TrimSpace(app.Version) == "" || !validSHA256(app.ELFSHA256) {
		return errors.New("application name, version, and ELF SHA-256 are required")
	}
	if strings.TrimSpace(info.Report.Board.Type) == "" || strings.TrimSpace(info.Report.Board.SSID) == "" {
		return errors.New("board type and Wi-Fi SSID are required")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func validActivationCode(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readResponse(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Linx response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("Linx response exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}
