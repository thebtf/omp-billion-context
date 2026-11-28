package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	engineStartupTimeout = 20 * time.Second
	// A foreign CPA process may still be draining the shared store.
	sessionsLockWait   = 3 * time.Second
	engineFlushTimeout = 12 * time.Second
)

var (
	errEngineExited   = errors.New("bundled billion-context engine exited")
	errRuntimeStorage = errors.New("cannot locate persistent CPA plugins storage from the native library path")
	errRuntimeCorrupt = errors.New("installed runtime is incomplete or corrupt")
	errSessionStore   = errors.New("shared session store is unavailable")
)

type runtimeManifest struct {
	Schema        int              `json:"schema"`
	KernelVersion string           `json:"kernel_version"`
	NodeVersion   string           `json:"node_version"`
	Platform      string           `json:"platform"`
	ArchiveSHA256 string           `json:"archive_sha256"`
	Required      map[string]int64 `json:"required"`
}

func (m runtimeManifest) identity() string {
	return m.KernelVersion + "-" + m.NodeVersion + "-" + m.Platform + "-" + m.ArchiveSHA256
}

func (m runtimeManifest) validate() error {
	for _, value := range []string{m.KernelVersion, m.NodeVersion, m.Platform} {
		if value == "" || len(value) > 40 || strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" {
			return errors.New("invalid bundled runtime identity")
		}
	}
	digest, err := hex.DecodeString(m.ArchiveSHA256)
	if m.Schema != 2 || err != nil || len(digest) != sha256.Size || strings.ToLower(m.ArchiveSHA256) != m.ArchiveSHA256 || m.Platform != runtime.GOOS+"-"+runtime.GOARCH {
		return errors.New("invalid bundled runtime manifest")
	}
	node := "node"
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	for _, name := range []string{node, "kernel/worker.mjs", "kernel/host.mjs", "kernel/store.mjs", "kernel/cache.mjs", "node_modules/acp-kernel/dist/index.js", "node_modules/acp-kernel/package.json"} {
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
			probe, err := os.CreateTemp(directory, ".write-check-*")
			if err == nil {
				_, writeErr := probe.Write([]byte{0})
				err = errors.Join(writeErr, probe.Close(), os.Remove(probe.Name()))
			}
			if err != nil {
				_ = lock.Close()
				return nil, fmt.Errorf("%w: %v", errSessionStore, err)
			}
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
	if json.Unmarshal(data, &marker) != nil || marker.Schema != manifest.Schema || marker.KernelVersion != manifest.KernelVersion || marker.NodeVersion != manifest.NodeVersion || marker.Platform != manifest.Platform || marker.ArchiveSHA256 != manifest.ArchiveSHA256 || len(marker.Required) != len(manifest.Required) {
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
	worker *kernelProcess
	stop   func()
	done   <-chan struct{}
}

func (e engine) close() {
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
	store, err := claimSessionStore(startupCtx, dataRoot, sessionsLockWait)
	if err != nil {
		return engine{}, &engineStartupError{stage: "storage", cause: err}
	}
	keep := false
	defer func() {
		if !keep {
			store.release()
		}
	}()
	node := "node"
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	cmd := exec.Command(filepath.Join(runtimeRoot, node), filepath.Join(runtimeRoot, "kernel", "worker.mjs"), store.dir)
	cmd.Dir = runtimeRoot
	cmd.Env = engineEnv()
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return engine{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return engine{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return engine{}, err
	}
	if err = startupCtx.Err(); err == nil {
		err = startWithSessionLease(cmd, store.lock)
	}
	if err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return engine{}, &engineStartupError{stage: "start", cause: err}
	}
	worker := newKernelProcess(stdin, stdout, stderr)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			worker.close()
			select {
			case <-done:
			case <-time.After(engineFlushTimeout):
				_ = cmd.Process.Kill()
				<-done
			}
			store.release()
		})
	}
	var reply struct{ Ready bool }
	if err = worker.call(startupCtx, "ping", nil, &reply); err == nil && !reply.Ready {
		err = errKernelProtocol
	}
	if err != nil {
		_ = cmd.Process.Kill()
		stop()
		return engine{}, &engineStartupError{stage: "startup", cause: err}
	}
	if err = lifetimeCtx.Err(); err != nil {
		stop()
		return engine{}, err
	}
	keep = true
	return engine{worker: worker, done: done, stop: stop}, nil
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
