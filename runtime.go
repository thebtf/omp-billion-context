package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed all:bundle
var bundledRuntime embed.FS

//go:embed private-runtime.mjs
var privateRuntime []byte

const engineStartupTimeout = 20 * time.Second

var errEngineExited = errors.New("bundled billion-context engine exited")

type engineStartupError struct {
	stage string
	cause error
}

func (e *engineStartupError) Error() string {
	reason := "runtime unavailable"
	var errno syscall.Errno
	switch {
	case errors.Is(e.cause, context.DeadlineExceeded):
		reason = "startup timed out"
	case errors.Is(e.cause, context.Canceled):
		reason = "startup canceled"
	case errors.Is(e.cause, os.ErrPermission):
		reason = "permission denied"
	case errors.Is(e.cause, os.ErrNotExist):
		reason = "runtime file not found"
	case errors.As(e.cause, &errno):
		reason = errno.Error()
	case errors.Is(e.cause, errEngineExited):
		reason = "engine exited before becoming ready"
	case errors.Is(e.cause, zip.ErrFormat), errors.Is(e.cause, zip.ErrAlgorithm), errors.Is(e.cause, zip.ErrChecksum):
		reason = "invalid runtime archive"
	}
	return "bundled billion-context " + e.stage + " failed: " + reason
}

func (e *engineStartupError) Unwrap() error {
	return e.cause
}

type engine struct {
	url    url.URL
	client *http.Client
	stop   func()
	done   <-chan struct{}
	token  string
}

func (e engine) close() {
	if e.client != nil {
		e.client.CloseIdleConnections()
	}
	if e.stop != nil {
		e.stop()
	}
}

func startEngine(startupCtx, lifetimeCtx context.Context) (engine, error) {
	if err := startupCtx.Err(); err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	data, err := bundledRuntime.ReadFile("bundle/runtime.zip")
	if err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	if err := startupCtx.Err(); err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	root, err := os.MkdirTemp("", "omp-billion-context-")
	if err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	kept := false
	defer func() {
		if !kept {
			_ = os.RemoveAll(root)
		}
	}()
	if err := unpackRuntime(startupCtx, root, data); err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)
	stateRoot := root
	sessionsDir := filepath.Join(stateRoot, "sessions")
	// The bili WebUI scans its explicit sessions directory even when persistence is disabled.
	if err := os.Mkdir(sessionsDir, 0o700); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	configPath := filepath.Join(root, "bili.json")
	cfg, err := json.Marshal(map[string]any{
		"host": "127.0.0.1", "port": addr.Port, "upstream": "http://127.0.0.1:1", "logFile": "off",
		"mitm": map[string]bool{"enabled": false},
	})
	if err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	if err := os.WriteFile(configPath, cfg, 0o600); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	token, proof := hex.EncodeToString(secret[:16]), hex.EncodeToString(secret[16:])
	watch := filepath.Join(root, "private-runtime.mjs")
	if err := os.WriteFile(watch, privateRuntime, 0o600); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	node := "node"
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	entry := filepath.Join(root, "node_modules", "billion-context", "dist", "index.js")
	watchPath := filepath.ToSlash(watch)
	if runtime.GOOS == "windows" {
		watchPath = "/" + watchPath
	}
	preload := url.URL{Scheme: "file", Path: watchPath}
	childCtx, cancelChild := context.WithCancel(lifetimeCtx)
	defer func() {
		if !kept {
			cancelChild()
		}
	}()
	cmd := exec.CommandContext(childCtx, filepath.Join(root, node), "--import", preload.String(), entry, "start", "--config", configPath, "--no-auto-update", "--no-passthrough")
	cmd.Dir = root
	cmd.Env = append(engineEnv(), "BILI_STRICT_PORT="+stringPort(addr), "BILI_SESSIONS_DIR="+sessionsDir,
		"XDG_CONFIG_HOME="+stateRoot, "XDG_DATA_HOME="+stateRoot, "XDG_STATE_HOME="+stateRoot, "XDG_CACHE_HOME="+stateRoot,
		"BILI_PERSIST=0", "BILI_RESUME_INHERITANCE=0", "BILI_FORK_ADOPTION=0", "BILI_ADVISORY_CHECK=0", "BILI_RELEASE_NOTES_CHECK=0", "OMP_ENGINE_TOKEN="+token, "OMP_ENGINE_PROOF="+proof)
	configureProcess(cmd)
	parent, err := cmd.StdinPipe()
	if err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	defer func() {
		if !kept {
			_ = parent.Close()
		}
	}()
	if err := listener.Close(); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	if err := startupCtx.Err(); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	if err := cmd.Start(); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = parent.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cancelChild()
				<-done
			}
			cancelChild()
			_ = os.RemoveAll(root)
		})
	}
	address := addr.String()
	client := sidecarClient(address)
	endpoint := url.URL{Scheme: "http", Host: address}
	if err := waitForEngine(startupCtx, client, endpoint.String()+"/__bili/health", proof, done); err != nil {
		cancelChild()
		stop()
		client.CloseIdleConnections()
		return engine{}, &engineStartupError{stage: "health", cause: err}
	}
	kept = true
	return engine{url: endpoint, client: client, stop: stop, done: done, token: token}, nil
}

func stringPort(addr *net.TCPAddr) string {
	_, port, _ := net.SplitHostPort(addr.String())
	return port
}

func engineEnv() []string {
	var env []string
	for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "TMPDIR", "HOME", "USERPROFILE", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func waitForEngine(ctx context.Context, client *http.Client, health, proof string, done <-chan struct{}) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errEngineExited
		case <-ticker.C:
			probe, cancel := context.WithTimeout(ctx, time.Second)
			req, _ := http.NewRequestWithContext(probe, http.MethodGet, health, nil)
			resp, err := client.Do(req)
			if err == nil {
				var payload struct{ OK bool }
				_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK && payload.OK && resp.Header.Get("X-Omp-Bili-Proof") == proof {
					cancel()
					return ctx.Err()
				}
			}
			cancel()
		}
	}
}

func unpackRuntime(ctx context.Context, root string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, file := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !filepath.IsLocal(filepath.FromSlash(file.Name)) || strings.Contains(file.Name, `\`) || !file.Mode().IsRegular() {
			return errors.New("invalid bundled runtime archive entry")
		}
		destination := filepath.Join(root, filepath.FromSlash(file.Name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := extractRuntimeFile(ctx, destination, file); err != nil {
			return err
		}
	}
	return ctx.Err()
}

type runtimeReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r runtimeReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func extractRuntimeFile(ctx context.Context, destination string, file *zip.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	mode := os.FileMode(0o600)
	if file.Name == "node" || file.Name == "node.exe" {
		mode = 0o700
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, runtimeReader{ctx: ctx, reader: reader})
	err = errors.Join(err, ctx.Err(), out.Close())
	if err != nil {
		_ = os.Remove(destination)
	}
	return err
}
