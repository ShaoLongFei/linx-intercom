package main

import "testing"

func TestParseDeviceTokens(t *testing.T) {
	t.Run("empty configuration is allowed for health-only development", func(t *testing.T) {
		tokens, err := parseDeviceTokens("")
		if err != nil || len(tokens) != 0 {
			t.Fatalf("parseDeviceTokens() = %v, %v", tokens, err)
		}
	})
	t.Run("valid device credentials", func(t *testing.T) {
		tokens, err := parseDeviceTokens(`{"device-a":"secret-a"}`)
		if err != nil || tokens["device-a"] != "secret-a" {
			t.Fatalf("parseDeviceTokens() = %v, %v", tokens, err)
		}
	})
	t.Run("rejects blank credentials", func(t *testing.T) {
		if _, err := parseDeviceTokens(`{"device-a":" "}`); err == nil {
			t.Fatal("blank token accepted")
		}
	})
}
