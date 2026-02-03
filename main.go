package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type Config struct {
	Addr                  string     `json:"addr"`
	DefaultTimeoutSeconds int        `json:"default_timeout_seconds"`
	Endpoints             []Endpoint `json:"endpoints"`
}

type Endpoint struct {
	Path           string   `json:"path"`
	Command        []string `json:"command"`
	WorkDir        string   `json:"work_dir"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
	PTY            bool     `json:"pty"`
}

type CommandResult struct {
	Path     string `json:"path"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration string `json:"duration"`
	TimedOut bool   `json:"timed_out"`
	Error    string `json:"error,omitempty"`
}

func main() {
	configPath := flag.String("config", "config.json", "Path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	mux := http.NewServeMux()
	for _, ep := range cfg.Endpoints {
		ep := ep
		mux.HandleFunc(ep.Path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				w.Header().Set("Allow", "GET, POST")
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
				return
			}
			result := runEndpoint(r.Context(), cfg.DefaultTimeoutSeconds, ep)
			status := http.StatusOK
			if result.Error != "" {
				status = http.StatusInternalServerError
			}
			if result.TimedOut {
				status = http.StatusGatewayTimeout
			}
			writeJSON(w, status, result)
		})
	}

	addr := cfg.Addr
	if addr == "" {
		addr = ":8080"
	}

	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("config must include at least one endpoint")
	}

	seen := make(map[string]struct{}, len(cfg.Endpoints))
	for i := range cfg.Endpoints {
		ep := &cfg.Endpoints[i]
		if !strings.HasPrefix(ep.Path, "/") {
			return nil, fmt.Errorf("endpoint path must start with '/': %q", ep.Path)
		}
		if len(ep.Command) == 0 {
			return nil, fmt.Errorf("endpoint %q must include a command", ep.Path)
		}
		if ep.WorkDir == "" {
			return nil, fmt.Errorf("endpoint %q must include a work_dir", ep.Path)
		}
		abs, err := filepath.Abs(ep.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("endpoint %q work_dir error: %w", ep.Path, err)
		}
		if info, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("endpoint %q work_dir error: %w", ep.Path, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("endpoint %q work_dir is not a directory: %s", ep.Path, abs)
		}
		ep.WorkDir = abs
		if _, ok := seen[ep.Path]; ok {
			return nil, fmt.Errorf("duplicate endpoint path: %q", ep.Path)
		}
		seen[ep.Path] = struct{}{}
	}

	return &cfg, nil
}

func runEndpoint(parent context.Context, defaultTimeout int, ep Endpoint) CommandResult {
	start := time.Now()

	timeout := defaultTimeout
	if ep.TimeoutSeconds != nil {
		timeout = *ep.TimeoutSeconds
	}

	ctx := parent
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, ep.Command[0], ep.Command[1:]...)
	cmd.Dir = ep.WorkDir

	if ep.PTY {
		return runWithPTY(cmd, ep.Path, start, ctx)
	}

	return runWithPipes(cmd, ep.Path, start, ctx)
}

func runWithPipes(cmd *exec.Cmd, path string, start time.Time, ctx context.Context) CommandResult {
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	return finalizeResult(path, stdoutBuf.String(), stderrBuf.String(), err, timedOut, start)
}

func runWithPTY(cmd *exec.Cmd, path string, start time.Time, ctx context.Context) CommandResult {
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		return CommandResult{
			Path:     path,
			ExitCode: -1,
			Duration: time.Since(start).String(),
			TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
			Error:    err.Error(),
		}
	}
	defer ptyFile.Close()

	output, readErr := io.ReadAll(ptyFile)
	waitErr := cmd.Wait()
	if readErr != nil && !errors.Is(readErr, syscall.EIO) && waitErr == nil {
		waitErr = readErr
	}

	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	return finalizeResult(path, string(output), "", waitErr, timedOut, start)
}

func finalizeResult(path, stdout, stderr string, err error, timedOut bool, start time.Time) CommandResult {
	exitCode := 0
	errorMessage := ""
	if err != nil {
		var exitErr *exec.ExitError
		if timedOut {
			exitCode = -1
			errorMessage = "command timed out"
		} else if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			errorMessage = err.Error()
		}
	}

	return CommandResult{
		Path:     path,
		ExitCode: exitCode,
		Stdout:   strings.TrimSpace(stdout),
		Stderr:   strings.TrimSpace(stderr),
		Duration: time.Since(start).String(),
		TimedOut: timedOut,
		Error:    errorMessage,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
