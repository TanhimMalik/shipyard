package server

import (
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		set     bool
		want    string
		wantErr bool
	}{
		{name: "default", want: "127.0.0.1:8080"},
		{name: "IPv4 loopback", value: "127.0.0.1:9090", set: true, want: "127.0.0.1:9090"},
		{name: "IPv6 loopback", value: "[::1]:9090", set: true, want: "[::1]:9090"},
		{name: "highest port", value: "127.0.0.1:65535", set: true, want: "127.0.0.1:65535"},
		{name: "explicitly empty", set: true, wantErr: true},
		{name: "IPv4 wildcard", value: "0.0.0.0:8080", set: true, wantErr: true},
		{name: "IPv6 wildcard", value: "[::]:8080", set: true, wantErr: true},
		{name: "LAN address", value: "192.168.1.10:8080", set: true, wantErr: true},
		{name: "public address", value: "8.8.8.8:8080", set: true, wantErr: true},
		{name: "hostname", value: "localhost:8080", set: true, wantErr: true},
		{name: "missing host", value: ":8080", set: true, wantErr: true},
		{name: "missing port", value: "127.0.0.1", set: true, wantErr: true},
		{name: "service port", value: "127.0.0.1:http", set: true, wantErr: true},
		{name: "zero port", value: "127.0.0.1:0", set: true, wantErr: true},
		{name: "negative port", value: "127.0.0.1:-1", set: true, wantErr: true},
		{name: "port overflow", value: "127.0.0.1:65536", set: true, wantErr: true},
		{name: "surrounding whitespace", value: " 127.0.0.1:8080 ", set: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := LoadConfig(func(key string) (string, bool) {
				if key != "SHIPYARD_API_ADDR" {
					t.Fatalf("unexpected environment variable: %s", key)
				}
				return tt.value, tt.set
			})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "SHIPYARD_API_ADDR") {
					t.Fatalf("expected configuration error naming SHIPYARD_API_ADDR, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.ListenAddr != tt.want {
				t.Errorf("ListenAddr = %q, want %q", config.ListenAddr, tt.want)
			}
		})
	}
}
