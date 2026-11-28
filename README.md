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

> [!IMPORTANT]
> If you use other model routers, set `omp-billion-context` to a higher `priority` number than all of them, including combo-router. Equal or unset priorities can let another router handle a request before compression.

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

The plugin handles authenticated HTTP Chat Completions, Responses and Anthropic Messages when the request contains a stable conversation ID. It recognizes standard session headers, `session_id` and `prompt_cache_key`. The Anthropic handler does not forward custom session headers, so for `/v1/messages` the plugin also accepts the conversation's opening turn as its identity; that value is stable while the conversation grows. A request without a usable ID or authenticated CPA caller scope follows the native CPA path. For an HTTP Responses full replay that explicitly sets `reasoning.context` to `current_turn`, ends in a new user message, and has a conversation ID (not just a cache key or matching opening text), the plugin can fold history around a complete encrypted reasoning item and its adjacent function call and matching output. Before each callback to CPA, the relay checks the protected groups against the original turn. If bili removed or changed a group, the relay sends the original full request at most once, and only if the callback uses the same streaming mode. A damaged callback on another transport, or a callback after this fallback, is rejected with HTTP 409 before model dispatch. These callback substitutions are counted separately as `protected_fallbacks` in the management counters. This can forgo compression; it does not retry a failed model call or decrypt reasoning. The default/auto and all-turns reasoning modes, active tool rounds, incomplete or ambiguous groups, WebSocket, background, compact, item references, server-managed conversations and deltas with a previous response ID stay on the native path. Compression state is isolated by CPA client, model, and protocol, and resets when the plugin stops.

A fold necessarily rewrites part of the prompt. The integration tests observe the provider-side cache key, credential and unchanged prefix across a fold and its next turn. They do not measure a live provider's cache hit rate. CPA's own routing strategy still applies: an actual account switch, expiry or model switch can invalidate a warm cache. Nested model calls may appear as separate traces in LLM Tracing.

### Using server-side bili after omp compaction

Use this optional mode for conversations where a text handoff is acceptable. For workloads that depend on encrypted reasoning across tool calls, keep provider-native compaction. With plugin 0.2.8, a tool call can start on the bili route and continue on the native route with a different prefix. The provider can report `input_transformations: [{type: "reasoning", action: "removed", reason: "prefix_mismatch"}]` even on a successful request. A handoff does not prevent this.

To try the text route in an omp conversation that already sends an opaque `compaction` item, run `/handoff` when the agent is idle. For example, `/handoff Preserve decisions, rejected alternatives, open tasks, and the latest tool results.` This asks omp's model to write a text handoff from the conversation. When omp replays that text and the recent turns, CPA can route eligible requests through bili again. A short session may report “Nothing to compact”. A handoff costs a model call and can omit details; check the facts you need on the next turn.

For subsequent automatic compactions, start omp with the optional [handoff preset](examples/omp-handoff.yml). From this repository run `omp --config examples/omp-handoff.yml`; from another directory pass the absolute path to the file. The preset replaces the compaction method order for that process with `handoff`, then `remote`, then `soft`. It keeps your thresholds, model, and tools. Starting without the preset restores your configured method order; a handoff already written to a session remains part of it.

The handoff is an omp operation. A **bili fold** occurs when bili replaces older messages with a summary in the request to the provider. Check **Billion Context → Overview** for folds and token savings. The preset does not force a fold, enable compression of deltas, or make unsupported encrypted reasoning eligible. Native compaction can use fewer tokens on short sessions; use the preset where longer conversations benefit from incremental folding.

## Uncompressed requests

A request that stays on the native CPA path is not an error, but some reasons are fixable by the operator. Those are logged once per ten minutes with the running count: `missing_caller_scope` (CPA did not report an authenticated caller), `missing_session_id`, `ambiguous_session_id`, `unusable_session_id`, and `protected_requires_session_id` (the protected replay lacks an explicit conversation ID). Each names the request that lost compression, so a client that never sends a conversation ID is visible instead of silently uncompressed.

The same counters are available without log access. `/v0/management/plugins/omp-billion-context/webui-data?target=/bypass` returns `{"counts":{...},"uncompressed":N}`, and the read-only overview response carries the same object as `cpaCompression`. `uncompressed` counts only the actionable reasons. Normal bypasses are counted separately: `previous_response_id`, `server_conversation`, `background`, `item_reference` and `compaction_input` distinguish different kinds of provider state. `encrypted_reasoning` means the protected sequence was not supported.

Session rows show the text a conversation starts with, so a wrapper tag such as `<system-reminder>` or `<job>` would otherwise become the title. The read-only bridge rewrites only those display strings and leaves identifiers, counters and unknown fields byte-identical.

Two limits come from bili itself and stay there. The message list inside a block renders `messagePreview`, which deliberately flattens whitespace to fit a 110-character row; the expanded text behind the one/full buttons is served as escaped `<pre>` and keeps its line breaks. Provider continuation snapshots are capped at 2 MiB by `BLOCK_CONTENT_MAX_BYTES`. We do not patch either, because a second renderer in the plugin would diverge from bili's own UI.

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
