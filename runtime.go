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
	"time"
)

//go:embed all:bundle
var bundledRuntime embed.FS

//go:embed private-runtime.mjs
var privateRuntime []byte

type engine struct {
	url    url.URL
	client *http.Client
	stop   func()
	token  string
}

func startEngine(ctx context.Context, upstream string) (engine, error) {
	data, err := bundledRuntime.ReadFile("bundle/runtime.zip")
	if err != nil {
		return engine{}, errors.New("library was built without the billion-context runtime")
	}
	root, err := os.MkdirTemp("", "omp-billion-context-")
	if err != nil {
		return engine{}, err
	}
	kept := false
	defer func() {
		if !kept {
			_ = os.RemoveAll(root)
		}
	}()
	if err := unpackRuntime(root, data); err != nil {
		return engine{}, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return engine{}, err
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)
	stateRoot, err := os.UserConfigDir()
	if err != nil {
		return engine{}, err
	}
	stateRoot = filepath.Join(stateRoot, pluginID)
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return engine{}, err
	}
	configPath := filepath.Join(root, "bili.json")
	cfg, err := json.Marshal(map[string]any{
		"host": "127.0.0.1", "port": addr.Port, "upstream": upstream, "logFile": "off",
		"mitm": map[string]bool{"enabled": false},
	})
	if err != nil {
		return engine{}, err
	}
	if err := os.WriteFile(configPath, cfg, 0o600); err != nil {
		return engine{}, err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return engine{}, err
	}
	token, proof := hex.EncodeToString(secret[:16]), hex.EncodeToString(secret[16:])
	watch := filepath.Join(root, "private-runtime.mjs")
	if err := os.WriteFile(watch, privateRuntime, 0o600); err != nil {
		return engine{}, err
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
	cmd := exec.CommandContext(ctx, filepath.Join(root, node), "--import", preload.String(), entry, "start", "--config", configPath, "--no-auto-update", "--no-passthrough")
	cmd.Dir = root
	cmd.Env = append(engineEnv(), "BILI_STRICT_PORT="+stringPort(addr), "BILI_SESSIONS_DIR="+filepath.Join(stateRoot, "sessions"),
		"XDG_CONFIG_HOME="+stateRoot, "XDG_DATA_HOME="+stateRoot, "XDG_STATE_HOME="+stateRoot, "XDG_CACHE_HOME="+stateRoot,
		"BILI_ADVISORY_CHECK=0", "BILI_RELEASE_NOTES_CHECK=0", "OMP_ENGINE_TOKEN="+token, "OMP_ENGINE_PROOF="+proof)
	configureProcess(cmd)
	parent, err := cmd.StdinPipe()
	if err != nil {
		return engine{}, err
	}
	defer func() {
		if !kept {
			_ = parent.Close()
		}
	}()
	if err := listener.Close(); err != nil {
		return engine{}, err
	}
	if err := cmd.Start(); err != nil {
		return engine{}, err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	stop := func() {
		_ = parent.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.RemoveAll(root)
	}
	address := addr.String()
	client := sidecarClient(address)
	endpoint := url.URL{Scheme: "http", Host: address}
	if err := waitForEngine(ctx, client, endpoint.String()+"/__bili/health", proof, done); err != nil {
		stop()
		client.CloseIdleConnections()
		return engine{}, err
	}
	kept = true
	return engine{url: endpoint, client: client, stop: stop, token: token}, nil
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
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("bundled billion-context startup timed out")
		case <-done:
			return errors.New("bundled billion-context exited before becoming ready")
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
					return nil
				}
			}
			cancel()
		}
	}
}

func unpackRuntime(root string, data []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, file := range archive.File {
		if !filepath.IsLocal(filepath.FromSlash(file.Name)) || strings.Contains(file.Name, `\`) || !file.Mode().IsRegular() {
			return errors.New("invalid bundled runtime archive entry")
		}
		destination := filepath.Join(root, filepath.FromSlash(file.Name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := extractRuntimeFile(destination, file); err != nil {
			return err
		}
	}
	return nil
}

func extractRuntimeFile(destination string, file *zip.File) error {
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
	_, err = io.Copy(out, reader)
	return errors.Join(err, out.Close())
}
