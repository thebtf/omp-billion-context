package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	engineStartupTimeout = 20 * time.Second
	// A foreign CPA process may still be draining the shared store.
	sessionsLockWait   = 3 * time.Second
	engineFlushTimeout = 12 * time.Second
	failedFlushTimeout = 3 * time.Second
)

var (
	errEngineExited   = errors.New("bundled billion-context engine exited")
	errRuntimeStorage = errors.New("cannot locate persistent CPA plugins storage from the native library path")
	errRuntimeCorrupt = errors.New("installed runtime is incomplete or corrupt")
	errSessionStore   = errors.New("shared session store is unavailable")
)

type runtimeManifest struct {
	Schema        int              `json:"schema"`
	BiliVersion   string           `json:"bili_version"`
	NodeVersion   string           `json:"node_version"`
	Platform      string           `json:"platform"`
	ArchiveSHA256 string           `json:"archive_sha256"`
	Required      map[string]int64 `json:"required"`
}

func (m runtimeManifest) identity() string {
	return m.BiliVersion + "-" + m.NodeVersion + "-" + m.Platform + "-" + m.ArchiveSHA256
}

func (m runtimeManifest) validate() error {
	for _, value := range []string{m.BiliVersion, m.NodeVersion, m.Platform} {
		if value == "" || len(value) > 40 || strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" {
			return errors.New("invalid bundled runtime identity")
		}
	}
	digest, err := hex.DecodeString(m.ArchiveSHA256)
	if m.Schema != 1 || err != nil || len(digest) != sha256.Size || strings.ToLower(m.ArchiveSHA256) != m.ArchiveSHA256 || m.Platform != runtime.GOOS+"-"+runtime.GOARCH {
		return errors.New("invalid bundled runtime manifest")
	}
	node := "node"
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	for _, name := range []string{node, "node_modules/billion-context/dist/index.js"} {
		if m.Required[name] <= 0 {
			return errors.New("bundled runtime manifest omits an executable entry")
		}
	}
	for name, size := range m.Required {
		if !filepath.IsLocal(filepath.FromSlash(name)) || strings.Contains(name, `\`) || name == "version.json" || size < 0 {
			return errors.New("invalid required runtime file")
		}
	}
	return nil
}

func runtimeDataRoot(library string) (string, error) {
	if library == "" {
		return "", errRuntimeStorage
	}
	library, err := filepath.Abs(library)
	if err != nil {
		return "", errors.Join(errRuntimeStorage, err)
	}
	fallback := filepath.Join(os.TempDir(), "cliproxy-pluginhost")
	relative, err := filepath.Rel(fallback, library)
	if err == nil && filepath.IsLocal(relative) {
		return "", errRuntimeStorage
	}
	directory := filepath.Dir(library)
	if filepath.Base(directory) == ".shadow" {
		directory = filepath.Dir(directory)
	}
	if filepath.Base(directory) == runtime.GOARCH && filepath.Base(filepath.Dir(directory)) == runtime.GOOS {
		directory = filepath.Dir(filepath.Dir(directory))
	}
	return filepath.Join(directory, pluginID), nil
}

func acquireRuntimeLock(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		locked, err := tryRuntimeLock(file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if locked {
			return file, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func releaseRuntimeLock(file *os.File) {
	unlockRuntime(file)
	_ = file.Close()
}

// The child inherits this lease so an orphan flushing after a CPA crash still excludes another writer.
type sessionStore struct {
	dir  string
	lock *os.File
	once sync.Once
}

func claimSessionStore(ctx context.Context, dataRoot string, wait time.Duration) (*sessionStore, error) {
	directory := filepath.Join(dataRoot, "sessions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", errSessionStore, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	path := filepath.Join(dataRoot, "sessions.lock")
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := trySessionLease(path)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errSessionStore, err)
		}
		if lock != nil {
			return &sessionStore{dir: directory, lock: lock}, nil
		}
		select {
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", errSessionStore, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// release hands the directory to the next writer and must run only after the
// engine process is gone. The durable directory itself is never removed.
func (s *sessionStore) release() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if lock := s.lock; lock != nil {
			s.lock = nil
			_ = lock.Close()
		}
	})
}

// engineStoreEnv pins bili's storage posture. One plain-JSON envelope per
// session is the only persistent copy: the unified sqlite sidecar would add a
// second durable file set (index.db plus -wal/-shm/-chk) whose bootstrap can
// fetch better-sqlite3 from npm. Retention stays bili's own default — the card
// asks for a session that survives a restart, not for a different eviction
// policy.
func engineStoreEnv(directory string) []string {
	return []string{
		"BILI_SESSIONS_DIR=" + directory,
		"BILI_PERSIST=1",
		"BILI_STORAGE_UNIFIED=legacy",
		"BILI_SQLITE_AUTO_DOWNLOAD=0",
	}
}

func validateRuntimeFiles(ctx context.Context, root string, manifest runtimeManifest) error {
	for name, size := range manifest.Required {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := root
		parts := strings.Split(filepath.FromSlash(name), string(filepath.Separator))
		for index, part := range parts {
			path = filepath.Join(path, part)
			info, err := os.Lstat(path)
			if err != nil {
				return errors.Join(errRuntimeCorrupt, err)
			}
			if index < len(parts)-1 {
				if !info.IsDir() {
					return errRuntimeCorrupt
				}
			} else if !info.Mode().IsRegular() || info.Size() != size || (name == "node" && info.Mode().Perm()&0o100 == 0) {
				return errRuntimeCorrupt
			}
		}
	}
	return ctx.Err()
}

func installedRuntime(ctx context.Context, root string, manifest runtimeManifest) (bool, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, errRuntimeCorrupt
	}
	markerPath := filepath.Join(root, "version.json")
	markerInfo, err := os.Lstat(markerPath)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Size() > 1<<20 {
		return false, errors.Join(errRuntimeCorrupt, err)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return false, errors.Join(errRuntimeCorrupt, err)
	}
	var marker runtimeManifest
	if json.Unmarshal(data, &marker) != nil || marker.Schema != manifest.Schema || marker.BiliVersion != manifest.BiliVersion || marker.NodeVersion != manifest.NodeVersion || marker.Platform != manifest.Platform || marker.ArchiveSHA256 != manifest.ArchiveSHA256 || len(marker.Required) != len(manifest.Required) {
		return false, errRuntimeCorrupt
	}
	for name, size := range manifest.Required {
		if actual, ok := marker.Required[name]; !ok || actual != size {
			return false, errRuntimeCorrupt
		}
	}
	if err := validateRuntimeFiles(ctx, root, manifest); err != nil {
		return false, err
	}
	return true, nil
}

func ensureRuntime(ctx context.Context, dataRoot string, manifest runtimeManifest, readArchive func() ([]byte, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := manifest.validate(); err != nil {
		return "", err
	}
	parent := filepath.Join(dataRoot, "runtime")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	lock, err := acquireRuntimeLock(ctx, filepath.Join(parent, manifest.identity()+".lock"))
	if err != nil {
		return "", err
	}
	defer releaseRuntimeLock(lock)
	root := filepath.Join(parent, manifest.identity())
	if ready, err := installedRuntime(ctx, root, manifest); ready || err != nil {
		return root, err
	}
	stage, err := os.MkdirTemp(parent, "."+manifest.identity()+".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	data, err := readArchive()
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, runtimeReader{ctx: ctx, reader: bytes.NewReader(data)}); err != nil {
		return "", err
	}
	if hex.EncodeToString(digest.Sum(nil)) != manifest.ArchiveSHA256 {
		return "", zip.ErrChecksum
	}
	if err := unpackRuntime(ctx, stage, data); err != nil {
		return "", err
	}
	if err := validateRuntimeFiles(ctx, stage, manifest); err != nil {
		return "", err
	}
	marker, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stage, "version.json"), marker, 0o600); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return "", errors.Join(errRuntimeCorrupt, err)
	}
	if err := os.Rename(stage, root); err != nil {
		return "", err
	}
	_, err = installedRuntime(ctx, root, manifest)
	return root, err
}

type engineStartupError struct {
	stage string
	cause error
}

func (e *engineStartupError) Error() string {
	reason := "runtime unavailable"
	var errno syscall.Errno
	switch {
	case errors.Is(e.cause, errRuntimeStorage):
		reason = errRuntimeStorage.Error()
	case errors.Is(e.cause, errRuntimeCorrupt):
		reason = errRuntimeCorrupt.Error()
	case errors.Is(e.cause, errSessionStore):
		reason = errSessionStore.Error()
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
	library, err := ownLibraryPath()
	if err != nil {
		return engine{}, &engineStartupError{stage: "storage", cause: errors.Join(errRuntimeStorage, err)}
	}
	dataRoot, err := runtimeDataRoot(library)
	if err != nil {
		return engine{}, &engineStartupError{stage: "storage", cause: err}
	}
	return startEngineAt(startupCtx, lifetimeCtx, dataRoot)
}

func startEngineAt(startupCtx, lifetimeCtx context.Context, dataRoot string) (engine, error) {
	data, err := bundledRuntime.ReadFile("bundle/runtime.json")
	if err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	var manifest runtimeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	runtimeRoot, err := ensureRuntime(startupCtx, dataRoot, manifest, func() ([]byte, error) {
		return bundledRuntime.ReadFile("bundle/runtime.zip")
	})
	if err != nil {
		return engine{}, &engineStartupError{stage: "extract", cause: err}
	}
	instances := filepath.Join(dataRoot, "instances")
	if err := os.MkdirAll(instances, 0o700); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	root, err := os.MkdirTemp(instances, "instance-")
	if err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	kept := false
	defer func() {
		if !kept {
			_ = os.RemoveAll(root)
		}
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)
	address := addr.String()
	client := sidecarClient(address)
	stateRoot := root
	// The shared session store outlives the private generation. Failing to claim
	// it fails the start: serving an unpersisted engine would silently lose the
	// history the operator asked to keep.
	store, err := claimSessionStore(startupCtx, dataRoot, sessionsLockWait)
	if err != nil {
		return engine{}, &engineStartupError{stage: "storage", cause: err}
	}
	// The lock is handed over only once the engine process is gone: either the
	// stop path below or a failed start.
	defer func() {
		if !kept {
			store.release()
		}
	}()
	sessionsDir := store.dir
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
	entry := filepath.Join(runtimeRoot, "node_modules", "billion-context", "dist", "index.js")
	preload := runtimeFileURL(watch)
	childCtx, cancelChild := context.WithCancel(context.WithoutCancel(lifetimeCtx))
	defer func() {
		if !kept {
			cancelChild()
		}
	}()
	cmd := exec.CommandContext(childCtx, filepath.Join(runtimeRoot, node), "--import", preload.String(), entry, "start", "--config", configPath, "--no-auto-update", "--no-passthrough")
	cmd.Dir = root
	cmd.Env = append(engineEnv(), "BILI_STRICT_PORT="+stringPort(addr),
		"XDG_CONFIG_HOME="+stateRoot, "XDG_DATA_HOME="+stateRoot, "XDG_STATE_HOME="+stateRoot, "XDG_CACHE_HOME="+stateRoot,
		"BILI_RESUME_INHERITANCE=0", "BILI_FORK_ADOPTION=0", "BILI_ADVISORY_CHECK=0", "BILI_RELEASE_NOTES_CHECK=0", "OMP_ENGINE_TOKEN="+token, "OMP_ENGINE_PROOF="+proof)
	cmd.Env = append(cmd.Env, engineStoreEnv(sessionsDir)...)
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
	if err := startWithSessionLease(cmd, store.lock); err != nil {
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	var stopOnce sync.Once
	// An engine that never became healthy gets a short leash: it has nothing
	// worth flushing and must not hold a cancelled startup for seconds.
	flushBudget := failedFlushTimeout
	stop := func() {
		stopOnce.Do(func() {
			// Closing stdin tells the preload to hand bili its own SIGTERM, so
			// the engine drains and fsyncs its envelopes before exiting. Idle
			// connections go first: bili's drain waits for open sockets.
			client.CloseIdleConnections()
			_ = parent.Close()
			select {
			case <-done:
			case <-time.After(flushBudget):
				cancelChild()
				<-done
			}
			cancelChild()
			store.release()
			_ = os.RemoveAll(root)
		})
	}
	endpoint := url.URL{Scheme: "http", Host: address}
	if err := waitForEngine(startupCtx, client, endpoint.String()+"/__bili/health", proof, done); err != nil {
		cancelChild()
		stop()
		client.CloseIdleConnections()
		return engine{}, &engineStartupError{stage: "health", cause: err}
	}
	flushBudget = engineFlushTimeout
	kept = true
	return engine{url: endpoint, client: client, stop: stop, done: done, token: token}, nil
}

func runtimeFileURL(path string) url.URL {
	path = filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(path, "//?/") {
			path = strings.TrimPrefix(path, "//?/")
			if len(path) >= 4 && strings.EqualFold(path[:4], "UNC/") {
				path = "//" + path[4:]
			}
		}
		if strings.HasPrefix(path, "//") {
			host, rest, _ := strings.Cut(path[2:], "/")
			return url.URL{Scheme: "file", Host: host, Path: "/" + rest}
		}
		path = "/" + path
	}
	return url.URL{Scheme: "file", Path: path}
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
		if !filepath.IsLocal(filepath.FromSlash(file.Name)) || strings.Contains(file.Name, `\`) || file.Name == "version.json" || !file.Mode().IsRegular() {
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
