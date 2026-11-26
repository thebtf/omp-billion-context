package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	artifact := filepath.Join(t.TempDir(), "custom", "linux", "amd64")
	for _, library := range []string{filepath.Join(artifact, "plugin.so"), filepath.Join(artifact, ".shadow", "cliproxy-plugin-digest.so")} {
		root, err := runtimeDataRoot(library)
		if err != nil || root != filepath.Join(artifact, pluginID) {
			t.Fatalf("root for %q = %q, %v", library, root, err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := runtimeDataRoot(filepath.Join("plugins", ".shadow", "plugin.so"))
	if err != nil || root != filepath.Join(cwd, "plugins", pluginID) {
		t.Fatalf("relative plugin directory = %q, %v", root, err)
	}
	for _, library := range []string{"", filepath.Join(os.TempDir(), "cliproxy-pluginhost", "cache", "digest", "plugin.so")} {
		if _, err := runtimeDataRoot(library); !errors.Is(err, errRuntimeStorage) {
			t.Fatalf("unsupported library path %q = %v", library, err)
		}
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
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
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
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
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
	before, err := os.Stat(path)
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
