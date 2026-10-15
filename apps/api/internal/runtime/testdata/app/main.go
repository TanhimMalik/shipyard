package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "exit" {
		os.Exit(7)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/limits", func(w http.ResponseWriter, _ *http.Request) {
		values := map[string]string{}
		for key, file := range map[string]string{
			"memory_max": "memory.max", "memory_swap_max": "memory.swap.max",
			"pids_max": "pids.max", "cpu_max": "cpu.max",
		} {
			data, err := os.ReadFile("/sys/fs/cgroup/" + file)
			if err != nil {
				http.Error(w, "cgroup v2 limit unavailable", http.StatusInternalServerError)
				return
			}
			values[key] = strings.TrimSpace(string(data))
		}
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			http.Error(w, "process status unavailable", http.StatusInternalServerError)
			return
		}
		for _, line := range strings.Split(string(status), "\n") {
			key, value, _ := strings.Cut(line, ":")
			if key == "Uid" || key == "CapEff" || key == "NoNewPrivs" {
				values[key] = strings.TrimSpace(value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(values)
	})
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		os.Exit(1)
	}
}
