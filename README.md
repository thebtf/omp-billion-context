# omp-billion-context

Context compression for existing CLIProxyAPI conversations, powered by [billion-context](https://github.com/ranxianglei/billion-context) and [acp-kernel](https://github.com/ranxianglei/acp-kernel). CPA still selects the model, provider, and credential. The plugin packages and runs the compression engine itself.

## Install

Use a plugin-enabled CLIProxyAPI 8.1.4 build. Install from the CPA Plugin Store when listed, or extract the shared library for your server platform from [GitHub Releases](https://github.com/thebtf/omp-billion-context/releases) into CPA's configured plugin directory. Both installation paths use `omp-billion-context-v<version>.so` on Linux, `.dylib` on macOS, or `.dll` on Windows. Keep the versioned filename unchanged. Enable plugins and this plugin in CPA:

```yaml
plugins:
  enabled: true
  configs:
    omp-billion-context:
      enabled: true
```

No extra model, upstream URL, key, Node.js installation, or background service is required. Keep sending requests to your existing CPA model with the normal CPA client key. Disable the plugin in CPA to stop compressing new requests.

With a configured `plugins.configs.omp-billion-context.store.version` or `store.release-tag` pin, CPA requires that exact filename version. If the pinned version is missing, CPA does not load a matching plugin. Only without a pin does CPA prefer the highest versioned filename. The Plugin Store records its installed release in this configuration. For a manual update of a Store-managed installation, update or remove the old version pin as well. Reload CPA's configuration after installing the new file. Native replacement requires a changed selected path, so overwriting an unversioned library can leave the old native image running. The release packages use versioned filenames for manual and automated installation.

When upgrading from a release without `plugin.quiesce`, including 0.2.2, restart CPA once after installation. The new library cannot stop engines already retained by an older native image. This plugin does not scan for or kill legacy processes.

## Runtime storage

The first supported request installs the bundled Node.js 24.22.3 and billion-context 0.1.216 in CPA's plugins root. CPA restarts reuse that installation without reading or extracting the embedded ZIP. No runtime download or separate setup is needed.

The plugin discovers its own loaded library path. It removes CPA 8.1.4's local `.shadow` component and the current platform's `<os>/<arch>` suffix when present. A library in either `/CLIProxyAPI/plugins` or `/CLIProxyAPI/plugins/linux/amd64` uses `/CLIProxyAPI/plugins/omp-billion-context`. Custom plugin roots follow the same layout. Relative library paths resolve against CPA's working directory. The plugin does not hardcode the Docker path or derive storage from CPA's executable, config, or home.

The storage directory contains:

- `runtime/<bili-version>-<node-version>-<os>-<arch>-<zip-sha256>/` with the packaged executable, modules, licenses, and a `version.json` completion marker.
- `runtime/<identity>.lock` with a stable OS file lock shared by installers and readers. These files remain after shutdown.
- `instances/<unique-id>/` with each running engine's private config, preload, sessions directory, and XDG directories. Node runs with this directory as its working directory.

The package builder emits `bundle/runtime.json` with the versions, platform, ZIP SHA-256, and required file sizes. Sorted ZIP entries have fixed timestamps and permissions. A new bundle identity installs in a separate directory. The installer verifies the ZIP digest, extracts into its own sibling staging directory, checks required files, and writes `version.json` last before publication. Every ready check and installation holds the identity's OS lock, including on Windows. The startup deadline includes lock waiting and installation.

Old completed versions stay available to native plugin generations that CPA retains. Shutdown removes only the current private instance. Session history is not persistent. `BILI_PERSIST=0` and `logFile=off` remain in effect, with explicit sessions and XDG paths inside the instance. Requests do not write into the installed runtime.

The plugins root must be writable and backed by persistent storage for installations to survive container replacement. CPA's temporary fallback shadows do not identify the original artifact directory and produce a storage error. A missing or mismatched completion marker, missing required file, or wrong required file size is refused without deleting the installation. Warm checks do not hash every installed file. Interrupted stages and unknown files are left untouched. There is no automatic runtime cleanup policy. A forced process termination can leave its private instance behind.

## Billion Context WebUI

In CPA Management Center, select **Billion Context** in the sidebar. Enter your existing CPA management key to open the bundled bili WebUI. The WebUI remembers an accepted key in the current browser tab, including across reloads. Select **Lock WebUI** to clear the key. The WebUI also clears it when CPA rejects it.

The WebUI shows read-only **Overview** and **Sessions**, including session details. **Got it** dismisses only the selected notice in the current tab.

The first request starts one bundled engine per generation. Concurrent requests share that startup. Its 20-second deadline includes unpacking, and WebUI reads wait at most 10 seconds. A failed startup or an exited engine stays unavailable for that generation. Canceling a request does not cancel startup. Ordinary reconfigure keeps the generation, including its engine, cached startup failure, compression summaries, and provider prefix.

Before native replacement, `plugin.quiesce` closes admission and waits up to two seconds for admitted RPCs, detached output streams, startup, and relay callbacks. Busy work returns `plugin_busy` and resumes the same generation without cancellation or cache reset. CPA aborts replacement and calls `plugin.reconfigure`. An idle quiesce stops the engine and relay and removes only their private instance. Requests during quiesce return `plugin_quiesced` with HTTP 503.

If loading the replacement fails after quiesce, CPA's rollback reconfigure creates a fresh lazy generation with the same host callback. The shared runtime stays installed. Engine restart is a cold-state boundary, so clients must continue sending their complete conversation history. In-memory summaries do not survive replacement or rollback after completed quiesce. Permanent shutdown cancels startup and stops the generation. That lifecycle owner cannot resume through reconfigure. A new native host initialization can create a distinct owner after completed quiesce or shutdown cleanup. It binds the new host callback and permanently retires the old owner. This permits disabling and re-enabling the same native image. CPA still retains old native library images until it exits. Its native-library preflight has no host deadline; the plugin's bounded drain does not bound that host operation.

## Supported requests

The plugin handles authenticated HTTP Chat Completions and Responses when the request contains a stable conversation ID. It recognizes standard session headers, `session_id` and `prompt_cache_key`. A request without a usable ID or authenticated CPA caller scope follows the native CPA path. Responses WebSocket, compact, and continuations with opaque provider state, including encrypted reasoning items, stay on the native CPA path. Compression state is isolated by CPA client, model, and protocol, and resets when the plugin stops.

A fold necessarily rewrites part of the prompt. The integration tests observe the provider-side cache key, credential and unchanged prefix across a fold and its next turn. They do not measure a live provider's cache hit rate. CPA's own routing strategy still applies: an actual account switch, expiry or model switch can invalidate a warm cache. Nested model calls may appear as separate traces in LLM Tracing.

## Build and test

On the target platform, install Go, a C compiler, Node.js and Python. Run `npm ci --omit=dev --ignore-scripts` and `python scripts/package_runtime.py`. Use `python scripts/release.py library --target <os>-<arch>` to get the canonical versioned library filename, then build with CGO enabled and `go build -buildmode=c-shared -o <plugin-library> .`. Supported targets are `linux-amd64`, `darwin-arm64`, and `windows-amd64`.

Run `go test ./...` and `node test/smoke.mjs <cpa-executable> <plugin-library> [replacement-library]`. The smoke test uses real CPA, the bundled engine, a synthetic provider, and a temporary server profile. It checks native metadata against `VERSION`, ordinary reconfigure state preservation, bounded busy refusal, and recovery after invalid-library replacement. Successful path-changing replacement requires a distinct native image built from the same source. Build that fixture with `go build -buildmode=c-shared -ldflags=-buildid=bili007-replacement-fixture -o <replacement-library> .`. Pass its path as the third argument or `BILI_TEST_REPLACEMENT_LIBRARY`. CI always supplies this fixture. The fixture is never included in release archives.

## Release protocol

`VERSION` is the single release version. The native plugin embeds it. `scripts/release.py` derives the library filename, release asset filename, and expected Git tag from it. This contract applies to every release and to both manual and Plugin Store installation.

1. Change `VERSION` in a PR. Complete review and checks before merging it.
2. Build each target from that source. Package it with `python scripts/release.py package --target <os>-<arch> --library <plugin-library> --dist dist`. Each ZIP contains exactly one nonempty root library named `omp-billion-context-v<VERSION>.<ext>`. Its asset name is `omp-billion-context_<VERSION>_<os>_<arch>.zip`.
3. Create the tag `v<VERSION>` on the reviewed commit. The tag workflow rejects a tag that differs from `VERSION`.
4. The workflow runs Go tests and the real CPA lifecycle smoke on all three platforms. It verifies the actual asset names and ZIP members together and checks rejection of a bad tag and an unversioned member.
5. Before publication, the workflow checks the tag, all three packages, and their checksums again. It publishes those verified assets through GitHub Releases. It does not install or deploy them to a running CPA instance.

To verify collected packages without publishing, run `python scripts/release.py verify --dist dist --tag v<VERSION>`.
