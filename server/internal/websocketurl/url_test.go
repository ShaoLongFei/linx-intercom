package websocketurl

import "testing"

func TestValidateWebSocketURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "secure remote", url: "wss://linx.example/ws"},
		{name: "loopback IPv4 test server", url: "ws://127.0.0.1:1234/ws"},
		{name: "loopback IPv6 test server", url: "ws://[::1]:1234/ws"},
		{name: "localhost", url: "ws://localhost:1234/ws"},
		{name: "remote plaintext", url: "ws://linx.example/ws", wantErr: true},
		{name: "non websocket scheme", url: "https://linx.example/ws", wantErr: true},
		{name: "missing host", url: "wss:///ws", wantErr: true},
		{name: "userinfo", url: "wss://user:pass@linx.example/ws", wantErr: true},
		{name: "fragment", url: "wss://linx.example/ws#token", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
