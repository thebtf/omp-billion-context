package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeFixture(t *testing.T, nodeContent string) (runtimeManifest, []byte) {
	t.Helper()
	node := "node"
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	files := map[string]string{node: nodeContent, "node_modules/billion-context/dist/index.js": "fixture entry"}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	required := make(map[string]int64)
	for _, name := range []string{node, "node_modules/billion-context/dist/index.js"} {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o700)
		file, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
		required[name] = int64(len(files[name]))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	digest := sha256.Sum256(data)
	return runtimeManifest{
		Schema: 1, BiliVersion: "0.1.216", NodeVersion: "24.22.3", Platform: runtime.GOOS + "-" + runtime.GOARCH,
		ArchiveSHA256: hex.EncodeToString(digest[:]), Required: required,
	}, data
}

func TestRuntimeDataRootUsesArtifactDirectory(t *testing.T) {
	plugins := filepath.Join(t.TempDir(), "custom")
	for _, artifact := range []string{plugins, filepath.Join(plugins, runtime.GOOS, runtime.GOARCH)} {
		for _, library := range []string{filepath.Join(artifact, "plugin.so"), filepath.Join(artifact, ".shadow", "cliproxy-plugin-digest.so")} {
			root, err := runtimeDataRoot(library)
			if err != nil || root != filepath.Join(plugins, pluginID) {
				t.Fatalf("root for %q = %q, %v", library, root, err)
			}
		}
	}
	for _, artifact := range []string{filepath.Join(plugins, "not-the-os", runtime.GOARCH), filepath.Join(plugins, runtime.GOOS, "not-the-arch")} {
		root, err := runtimeDataRoot(filepath.Join(artifact, ".shadow", "plugin.so"))
		if err != nil || root != filepath.Join(artifact, pluginID) {
			t.Fatalf("non-platform root for %q = %q, %v", artifact, root, err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{"plugins", filepath.Join("plugins", runtime.GOOS, runtime.GOARCH)} {
		root, err := runtimeDataRoot(filepath.Join(artifact, ".shadow", "plugin.so"))
		if err != nil || root != filepath.Join(cwd, "plugins", pluginID) {
			t.Fatalf("relative plugin directory = %q, %v", root, err)
		}
	}
	for _, library := range []string{"", filepath.Join(os.TempDir(), "cliproxy-pluginhost", "cache", "digest", "plugin.so")} {
		if _, err := runtimeDataRoot(library); !errors.Is(err, errRuntimeStorage) {
			t.Fatalf("unsupported library path %q = %v", library, err)
		}
	}
}

func TestRuntimeFileURL(t *testing.T) {
	type fileURLCase struct {
		name string
		path string
		want string
	}
	cases := []fileURLCase{
		{"unix", "/opt/bili data/#instance/private-runtime.mjs", "file:///opt/bili%20data/%23instance/private-runtime.mjs"},
	}
	if runtime.GOOS == "windows" {
		cases = []fileURLCase{
			{"drive", `C:\bili data\#instance\private-runtime.mjs`, "file:///C:/bili%20data/%23instance/private-runtime.mjs"},
			{"unc", `\\nas\share\bili data\#instance\private-runtime.mjs`, "file://nas/share/bili%20data/%23instance/private-runtime.mjs"},
			{"extended drive", `\\?\C:\bili data\#instance\private-runtime.mjs`, "file:///C:/bili%20data/%23instance/private-runtime.mjs"},
			{"extended unc", `\\?\UNC\nas\share\bili data\#instance\private-runtime.mjs`, "file://nas/share/bili%20data/%23instance/private-runtime.mjs"},
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runtimeFileURL(tc.path)
			if got.String() != tc.want {
				t.Fatalf("preload URL for %q = %q, want %q", tc.path, got.String(), tc.want)
			}
		})
	}
}

func TestRuntimeColdInstallAndWarmReuse(t *testing.T) {
	root := t.TempDir()
	manifest, data := runtimeFixture(t, "first node")
	reads := 0
	readArchive := func() ([]byte, error) { reads++; return data, nil }
	installed, err := ensureRuntime(context.Background(), root, manifest, readArchive)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(installed, "version.json")
	markerFile, err := os.Open(marker)
	if err != nil {
		t.Fatal(err)
	}
	before, err := markerFile.Stat()
	closeErr := markerFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var completed runtimeManifest
	if err := json.Unmarshal(markerData, &completed); err != nil || completed.ArchiveSHA256 != manifest.ArchiveSHA256 {
		t.Fatalf("completion marker = %s, %v", markerData, err)
	}
	for range 2 {
		warm, err := ensureRuntime(context.Background(), root, manifest, readArchive)
		if err != nil || warm != installed {
			t.Fatalf("warm runtime = %q, %v", warm, err)
		}
	}
	after, err := os.Stat(marker)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || reads != 1 {
		t.Fatalf("warm reuse changed marker or read archive again: reads=%d, error=%v", reads, err)
	}
	for name, size := range manifest.Required {
		content, err := os.ReadFile(filepath.Join(installed, filepath.FromSlash(name)))
		if err != nil || int64(len(content)) != size {
			t.Fatalf("installed file %s = %q, %v", name, content, err)
		}
	}
}

func TestRuntimeUpgradePreservesOldGeneration(t *testing.T) {
	root := t.TempDir()
	first, data := runtimeFixture(t, "old executable")
	old, err := ensureRuntime(context.Background(), root, first, func() ([]byte, error) { return data, nil })
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(old, "version.json")
	markerFile, err := os.Open(marker)
	if err != nil {
		t.Fatal(err)
	}
	before, err := markerFile.Stat()
	closeErr := markerFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	upgraded, nextData := runtimeFixture(t, "new executable")
	versionOnly := upgraded
	versionOnly.BiliVersion = "0.1.217"
	for _, next := range []runtimeManifest{upgraded, versionOnly} {
		installed, err := ensureRuntime(context.Background(), root, next, func() ([]byte, error) { return nextData, nil })
		if err != nil || installed == old {
			t.Fatalf("upgrade reused old generation: %q, %v", installed, err)
		}
	}
	after, err := os.Stat(marker)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("upgrade changed old completion marker: %v", err)
	}
	for name := range first.Required {
		if filepath.Base(name) == "index.js" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(old, name))
		if err != nil || string(content) != "old executable" {
			t.Fatalf("old executable no longer usable: %q, %v", content, err)
		}
	}
}

func TestRuntimeConcurrentInstallReadsArchiveOnce(t *testing.T) {
	root := t.TempDir()
	manifest, data := runtimeFixture(t, "shared executable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	readArchive := func() ([]byte, error) {
		if reads.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return data, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	type result struct {
		path string
		err  error
	}
	results := make(chan result, 2)
	install := func() {
		path, err := ensureRuntime(ctx, root, manifest, readArchive)
		results <- result{path, err}
	}
	go install()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go install()
	close(release)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.path != second.path || reads.Load() != 1 {
		t.Fatalf("shared installation = %+v, %+v, archive reads=%d", first, second, reads.Load())
	}
}

func TestRuntimeCancellationCleansOnlyOwnedStage(t *testing.T) {
	root := t.TempDir()
	manifest, data := runtimeFixture(t, "canceled executable")
	parent := filepath.Join(root, "runtime")
	interrupted := filepath.Join(parent, "."+manifest.identity()+".stage-interrupted")
	if err := os.MkdirAll(interrupted, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(interrupted, "unknown")
	if err := os.WriteFile(unknown, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := ensureRuntime(ctx, root, manifest, func() ([]byte, error) {
		cancel()
		return data, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled installation = %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 2 {
		t.Fatalf("cancellation left owned stage or deleted unknown stage: %v, %v", entries, err)
	}
	installed, err := ensureRuntime(context.Background(), root, manifest, func() ([]byte, error) { return data, nil })
	if err != nil || installed == interrupted {
		t.Fatalf("restart after interrupted stage = %q, %v", installed, err)
	}
	if content, err := os.ReadFile(unknown); err != nil || string(content) != "keep" {
		t.Fatalf("interrupted stage was modified: %q, %v", content, err)
	}
}

func TestRuntimeRefusesCorruptPublishedDirectory(t *testing.T) {
	for _, corruption := range []string{"missing-marker", "invalid-marker", "wrong-version", "truncated-node", "missing-entry"} {
		t.Run(corruption, func(t *testing.T) {
			root := t.TempDir()
			manifest, data := runtimeFixture(t, "original executable")
			installed, err := ensureRuntime(context.Background(), root, manifest, func() ([]byte, error) { return data, nil })
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(installed, "version.json")
			switch corruption {
			case "missing-marker":
				err = os.Remove(marker)
			case "invalid-marker":
				err = os.WriteFile(marker, []byte("not json"), 0o600)
			case "wrong-version":
				wrong := manifest
				wrong.NodeVersion = "1.0.0"
				content, marshalErr := json.Marshal(wrong)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(marker, content, 0o600)
			case "truncated-node":
				for name := range manifest.Required {
					if filepath.Base(name) != "index.js" {
						err = os.WriteFile(filepath.Join(installed, name), []byte("short"), 0o700)
					}
				}
			case "missing-entry":
				err = os.Remove(filepath.Join(installed, "node_modules", "billion-context", "dist", "index.js"))
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = ensureRuntime(context.Background(), root, manifest, func() ([]byte, error) {
				t.Error("corrupt published runtime triggered archive read or repair")
				return data, nil
			})
			if !errors.Is(err, errRuntimeCorrupt) {
				t.Fatalf("corrupt runtime accepted: %v", err)
			}
			if _, err := os.Stat(installed); err != nil {
				t.Fatalf("corrupt directory was deleted: %v", err)
			}
		})
	}
}

func TestRuntimeRejectsArchiveDigestMismatch(t *testing.T) {
	root := t.TempDir()
	manifest, _ := runtimeFixture(t, "expected executable")
	_, wrong := runtimeFixture(t, "changed executable")
	if _, err := ensureRuntime(context.Background(), root, manifest, func() ([]byte, error) { return wrong, nil }); !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("archive digest mismatch = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "runtime"))
	if err != nil || len(entries) != 1 || entries[0].Name() != manifest.identity()+".lock" {
		t.Fatalf("digest mismatch left a stage or published files: %v, %v", entries, err)
	}
}

func TestRuntimeHeldLockDeadlineIncludesWarmValidation(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			root := t.TempDir()
			manifest, data := runtimeFixture(t, "locked executable")
			parent := filepath.Join(root, "runtime")
			if warm {
				if _, err := ensureRuntime(context.Background(), root, manifest, func() ([]byte, error) { return data, nil }); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			lock, err := acquireRuntimeLock(context.Background(), filepath.Join(parent, manifest.identity()+".lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer releaseRuntimeLock(lock)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err = ensureRuntime(ctx, root, manifest, func() ([]byte, error) {
				t.Error("held lock allowed archive access")
				return data, nil
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("held lock = %v", err)
			}
		})
	}
}

func TestRuntimeLockChildProcess(t *testing.T) {
	path := os.Getenv("OMP_RUNTIME_LOCK_TEST")
	if path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	lock, err := acquireRuntimeLock(ctx, path)
	if os.Getenv("OMP_RUNTIME_LOCK_BUSY") == "1" {
		if lock != nil {
			releaseRuntimeLock(lock)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("child bypassed held lock: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	releaseRuntimeLock(lock)
}

func TestRuntimeLockExcludesAnotherProcessAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireRuntimeLock(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if lock != nil {
			releaseRuntimeLock(lock)
		}
	}()
	before, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runChild := func(busy string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRuntimeLockChildProcess$")
		cmd.Env = append(os.Environ(), "OMP_RUNTIME_LOCK_TEST="+path, "OMP_RUNTIME_LOCK_BUSY="+busy)
		configureProcess(cmd)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lock child failed: %v\n%s", err, output)
		}
	}
	runChild("1")
	releaseRuntimeLock(lock)
	lock = nil
	runChild("0")
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("release replaced or removed stable lock: %v", err)
	}
}

func TestSessionStorePersistsAcrossGenerations(t *testing.T) {
	dataRoot := t.TempDir()
	first, err := claimSessionStore(context.Background(), dataRoot, time.Second)
	if err != nil {
		t.Fatalf("free store was not claimed: %v", err)
	}
	want := filepath.Join(dataRoot, "sessions")
	if first.dir != want {
		t.Fatalf("store directory = %q, want %q", first.dir, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("durable directory missing: %v", err)
	}
	envelope := filepath.Join(want, "openai", "127.0.0.1_abc.json")
	if err := os.MkdirAll(filepath.Dir(envelope), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envelope, []byte(`{"id":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first.release()
	first.release()

	second, err := claimSessionStore(context.Background(), dataRoot, time.Second)
	if err != nil {
		t.Fatalf("released store was not claimable again: %v", err)
	}
	defer second.release()
	if content, err := os.ReadFile(envelope); err != nil || string(content) != `{"id":"abc"}` {
		t.Fatalf("durable envelope did not survive the generation change: %q, %v", content, err)
	}
}

func TestSessionStoreRefusesSecondWriter(t *testing.T) {
	dataRoot := t.TempDir()
	first, err := claimSessionStore(context.Background(), dataRoot, time.Second)
	if err != nil {
		t.Fatalf("first writer did not claim the store: %v", err)
	}
	defer first.release()
	started := time.Now()
	second, err := claimSessionStore(context.Background(), dataRoot, 150*time.Millisecond)
	if second != nil {
		second.release()
		t.Fatal("second writer shared the store with the first")
	}
	// The refusal must identify itself as storage and keep the cause readable,
	// while staying a storage failure: an engineFailure must not report a held
	// store as a startup timeout.
	if !errors.Is(err, errSessionStore) {
		t.Fatalf("contended claim error = %v, want a session-store refusal", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended claim would be reported as a timeout: %v", err)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("contended claim hid the underlying lock error: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("contended claim returned before its bounded wait: %v", elapsed)
	}
}

func TestSessionStoreRefusesUnusableDirectory(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataRoot, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, claimErr := claimSessionStore(context.Background(), dataRoot, time.Second)
	if store != nil || claimErr == nil {
		store.release()
		t.Fatalf("unusable root was accepted: %+v", store)
	}
	startup := (&engineStartupError{stage: "storage", cause: claimErr}).Error()
	if !strings.Contains(startup, "session store") || strings.Contains(startup, dataRoot) {
		t.Fatalf("startup error does not name the session store: %q", startup)
	}
}

func startPreloadFixture(t *testing.T, script string) (stdin io.WriteCloser, exited <-chan error, dir string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the preload shutdown path needs a real node")
	}
	dir = t.TempDir()
	watch := filepath.Join(dir, "private-runtime.mjs")
	if err := os.WriteFile(watch, privateRuntime, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "fixture.cjs")
	if err := os.WriteFile(fixture, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	preload := runtimeFileURL(watch)
	cmd := exec.CommandContext(ctx, node, "--import", preload.String(), fixture)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OMP_ENGINE_TOKEN="+strings.Repeat("ab", 16), "OMP_ENGINE_PROOF=proof", "OMP_TEST_DIR="+dir)
	configureProcess(cmd)
	stdin, err = cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ready := filepath.Join(dir, "ready")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("preload fixture exited before becoming ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("preload fixture did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return stdin, done, dir
}

func TestPreloadDeliversOrderlyShutdownOnParentClose(t *testing.T) {
	const script = `const fs = require('node:fs');
process.on('SIGTERM', () => {
  fs.writeFileSync(require('node:path').join(process.env.OMP_TEST_DIR, 'flushed'), 'orderly');
  process.exit(0);
});
fs.writeFileSync(require('node:path').join(process.env.OMP_TEST_DIR, 'ready'), 'ready');
setInterval(() => {}, 1000);
`
	stdin, done, dir := startPreloadFixture(t, script)
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("preload fixture failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("preload did not finish the orderly shutdown")
	}
	if content, err := os.ReadFile(filepath.Join(dir, "flushed")); err != nil || string(content) != "orderly" {
		t.Fatalf("parent close skipped the engine's own shutdown path: %q, %v", content, err)
	}
}

func TestPreloadExitsWhenEngineHasNoShutdownHook(t *testing.T) {
	const script = `const fs = require('node:fs');
fs.writeFileSync(require('node:path').join(process.env.OMP_TEST_DIR, 'ready'), 'ready');
setInterval(() => {}, 1000);
`
	stdin, done, dir := startPreloadFixture(t, script)
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("preload fixture failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preload hung instead of exiting for an engine without a shutdown hook")
	}
	if _, err := os.Stat(filepath.Join(dir, "flushed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected shutdown marker: %v", err)
	}
}
