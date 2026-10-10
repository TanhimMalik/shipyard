package server

import (
	"errors"
	"net/netip"
)

type Config struct {
	ListenAddr string
}

func LoadConfig(lookupEnv func(string) (string, bool)) (Config, error) {
	address := "127.0.0.1:8080"
	if value, exists := lookupEnv("SHIPYARD_API_ADDR"); exists {
		address = value
	}

	parsed, err := netip.ParseAddrPort(address)
	if err != nil || parsed.Port() == 0 {
		return Config{}, errors.New("SHIPYARD_API_ADDR must be a loopback IP and port from 1 to 65535, for example 127.0.0.1:8080")
	}
	if !parsed.Addr().IsLoopback() {
		return Config{}, errors.New("SHIPYARD_API_ADDR must use a loopback IP (127.0.0.1 or ::1)")
	}

	return Config{ListenAddr: address}, nil
}
