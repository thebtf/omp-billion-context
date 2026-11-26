# omp-billion-context

Context compression for existing CLIProxyAPI conversations, powered by [billion-context](https://github.com/ranxianglei/billion-context) and [acp-kernel](https://github.com/ranxianglei/acp-kernel). CPA still selects the model, provider, and credential. The plugin packages and runs the compression engine itself.

## Install

Use a plugin-enabled CLIProxyAPI 8.1.4 build. Install from the CPA Plugin Store when listed, or extract the shared library for your server platform from [GitHub Releases](https://github.com/thebtf/omp-billion-context/releases) into CPA's configured plugin directory. Keep its filename unchanged. Enable plugins and this plugin in CPA:

```yaml
plugins:
  enabled: true
  configs:
    omp-billion-context:
      enabled: true
```

No extra model, upstream URL, key, Node.js installation, or background service is required. Keep sending requests to your existing CPA model with the normal CPA client key. Disable the plugin in CPA to stop compressing new requests.

## Runtime storage

The first supported request installs the bundled Node.js 24.22.3 and billion-context 0.1.216 beside the native plugin library. CPA restarts reuse that installation without reading or extracting the embedded ZIP. No runtime download or separate setup is needed.

The plugin discovers its own loaded library path. CPA 8.1.4 loads local copies from `<artifact-dir>/.shadow`, so storage is `<artifact-dir>/omp-billion-context`. A library in `/CLIProxyAPI/plugins` uses `/CLIProxyAPI/plugins/omp-billion-context`. A library in `/CLIProxyAPI/plugins/linux/amd64` uses `/CLIProxyAPI/plugins/linux/amd64/omp-billion-context`. Custom plugin directories follow the same rule. Relative library paths resolve against CPA's working directory. The plugin never strips platform directories, assumes a default plugins directory, or derives storage from CPA's executable, config, or home.

The storage directory contains:

- `runtime/<bili-version>-<node-version>-<os>-<arch>-<zip-sha256>/` with the packaged executable, modules, licenses, and a `version.json` completion marker.
- `runtime/<identity>.lock` with a stable OS file lock shared by installers and readers. These files remain after shutdown.
- `instances/<unique-id>/` with each running engine's private config, preload, sessions directory, and XDG directories. Node runs with this directory as its working directory.

The package builder emits `bundle/runtime.json` with the versions, platform, ZIP SHA-256, and required file sizes. Sorted ZIP entries have fixed timestamps and permissions. A new bundle identity installs in a separate directory. The installer verifies the ZIP digest, extracts into its own sibling staging directory, checks required files, and writes `version.json` last before publication. Every ready check and installation holds the identity's OS lock, including on Windows. The startup deadline includes lock waiting and installation.

Old completed versions stay available to native plugin generations that CPA retains. Shutdown removes only the current private instance. Session history is not persistent. `BILI_PERSIST=0` and `logFile=off` remain in effect, with explicit sessions and XDG paths inside the instance. Requests do not write into the installed runtime.

The artifact directory must be writable and backed by persistent storage for installations to survive container replacement. CPA's temporary fallback shadows do not identify the original artifact directory and produce a storage error. A missing or mismatched completion marker, missing required file, or wrong required file size is refused without deleting the installation. Warm checks do not hash every installed file. Interrupted stages and unknown files are left untouched. There is no automatic runtime cleanup policy. A forced process termination can leave its private instance behind.

## Billion Context WebUI

In CPA Management Center, select **Billion Context** in the sidebar. Enter your existing CPA management key to open the bundled bili WebUI. The WebUI remembers an accepted key in the current browser tab, including across reloads. Select **Lock WebUI** to clear the key. The WebUI also clears it when CPA rejects it.

The WebUI shows read-only **Overview** and **Sessions**, including session details. **Got it** dismisses only the selected notice in the current tab.

The first request starts one bundled engine per plugin instance. Concurrent requests share that startup. Its 20-second deadline includes unpacking, and WebUI reads wait at most 10 seconds. A failed startup or an exited engine stays unavailable until the plugin instance is reinitialized. Canceling a request does not cancel startup. Plugin shutdown cancels startup and stops its engine. CPA can retain old native plugin instances until process shutdown, so this does not change CPA's hot-unload behavior.

## Supported requests

The plugin handles authenticated HTTP Chat Completions and Responses when the request contains a stable conversation ID. It recognizes standard session headers, `session_id` and `prompt_cache_key`. A request without a usable ID or authenticated CPA caller scope follows the native CPA path. Responses WebSocket, compact, and continuations with opaque provider state, including encrypted reasoning items, stay on the native CPA path. Compression state is isolated by CPA client, model, and protocol, and resets when the plugin stops.

A fold necessarily rewrites part of the prompt. The integration tests observe the provider-side cache key, credential and unchanged prefix across a fold and its next turn. They do not measure a live provider's cache hit rate. CPA's own routing strategy still applies: an actual account switch, expiry or model switch can invalidate a warm cache. Nested model calls may appear as separate traces in LLM Tracing.

## Build and test

On the target platform, install Go, a C compiler, Node.js and Python. Run `npm ci --omit=dev --ignore-scripts` and `python scripts/package_runtime.py`, then build with CGO enabled and `go build -buildmode=c-shared -o <plugin-library> .`. Name the file `omp-billion-context.so` on Linux, `omp-billion-context.dylib` on macOS, or `omp-billion-context.dll` on Windows.

Run `go test ./...` and `node test/smoke.mjs <cpa-executable> <plugin-library>`. The smoke test uses real CPA and the bundled engine with a synthetic provider and temporary server profile.
