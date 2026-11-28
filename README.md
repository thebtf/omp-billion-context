# omp-billion-context

Context compression for existing CLIProxyAPI conversations, powered by [acp-kernel](https://github.com/ranxianglei/acp-kernel), the standalone compression library used by [billion-context](https://github.com/ranxianglei/billion-context). CPA still selects the model, provider, and credential. The plugin hosts the standalone **acp-kernel** library. It does not ship or start the billion-context application, create a proxy endpoint, or require clients to change their base URL. Version 0.2.9 used the full application; the architecture described here starts at 0.3.0.

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

## Compression dashboard

Open **Billion Context** in CPA Management Center and enter the CPA **management** key. A model API key does not grant dashboard access. Overview shows compression totals. Sessions lists conversations and lets you inspect their saved blocks. Token savings are estimates, not provider billing measurements.

- **Provider cache:** inspect reported cache reads and writes, coverage, and the before/after-fold comparison without exposing a cache key. The percentage is the token-weighted share of matched input, not a fraction of requests. A missing provider counter is unknown, not a cache miss. An unchanged prefix or cache key alone cannot prove a cache hit.
- **Chat notifications:** change the server's visibility setting in the dashboard. Disabling notifications hides compression markers from the chat without disabling folds or session storage. The setting is shared by clients of this installation and survives a restart.
- **Router priority:** acknowledge the reminder to hide it in this browser. This changes only the notice. To change the actual order, edit plugin priorities in CPA.
- **Lock dashboard:** removes the saved management key from this browser and returns to the login screen. It does not disable the plugin, revoke the key at CPA or delete conversations. Language and notice dismissal are separate browser preferences.

## Runtime and history

The native library contains a pinned Node interpreter, our private worker, the standalone [acp-kernel](https://github.com/ranxianglei/acp-kernel) library and license notices. Go calls the worker over private standard input/output. Only CPA calls providers. There is no model gateway, HTTP listener, second set of provider credentials or runtime download to configure.

The first supported request or authenticated dashboard read installs the immutable bundle under `omp-billion-context/runtime` inside the configured CPA plugins root. A library installed under a platform subdirectory uses that same root. Keep the plugins root on writable persistent storage when running CPA in a container. The plugin refuses to run without a usable session store instead of treating an in-memory conversation as saved.

The worker owns the persisted compression state under `omp-billion-context/sessions`. Treat the directory as private conversation data. New sessions preserve committed folds across process restarts when the client sends its full history again. Files written by **0.2.9** are not rewritten or deleted. They are shown separately as archived data where readable. An archive entry is not evidence that a new conversation inherited an old fold. Sessions already lost by pre-0.2.9 releases cannot be recovered. To roll back, reselect the old library through CPA; the old runtime and session files are left intact.

A model call whose deadline expires returns a timeout to its client. CPA’s synchronous native callback cannot be forcibly cancelled by the plugin. Until that callback actually returns, another request for the same conversation receives `409 session_busy` instead of starting a duplicate provider call. Other conversations can continue.

A reconfigure with only a priority change does not restart an active turn. Before swapping libraries CPA asks the plugin to quiesce. An active turn or output stream can refuse replacement rather than losing state mid-response.

## Supported requests

For streamed Responses with `store: true` (the default), bili applies saved folds and asks the model for a bounded summary before the visible answer when the kernel recommends compression. The visible request keeps the caller's tools and the provider's response ID, so `previous_response_id` can resume it through CPA. If no safe range is available, bili sends the original projection. With `store: false`, the model can also request folds during its stream.

The plugin handles authenticated HTTP Chat Completions, Responses and Anthropic Messages when the request contains a stable conversation ID. It recognizes standard session headers, `session_id` and `prompt_cache_key`. The Anthropic handler does not forward custom session headers, so for `/v1/messages` the plugin also accepts the conversation's opening turn as its identity; that value is stable while the conversation grows. A request without a usable ID or authenticated CPA caller scope follows the native CPA path. For an HTTP Responses full replay that explicitly sets `reasoning.context` to `current_turn`, ends in a new user message, and has a conversation ID (not just a cache key or matching opening text), the plugin can fold history around a complete encrypted reasoning item and its adjacent function call and matching output. Before dispatch, the plugin checks protected groups against the original turn. An unsafe projection cannot replace the protected group sent to the model. Protective fallbacks are counted separately as `protected_fallbacks` in management counters. The default/auto and all-turns reasoning modes, active tool rounds, incomplete or ambiguous groups, WebSocket, background, compact, item references, server-managed conversations and deltas with a previous response ID stay on the native path. Compression state is isolated by CPA caller, model and protocol. A committed fold can be restored after the plugin stops.

Multi-choice Chat Completions (`n > 1`) stay on the native CPA route. Each choice has an independent tool-call stream and there is no single correct fold to commit for all alternatives.

A fold necessarily rewrites part of the prompt. The integration tests observe the provider-side cache key, credential and unchanged prefix across a fold and its next turn. They do not measure a live provider's cache hit rate. CPA's own routing strategy still applies: an actual account switch, expiry or model switch can invalidate a warm cache. Nested model calls may appear as separate traces in LLM Tracing.

For an oversized Anthropic Messages request, bili asks the model to summarize older messages before sending the user's turn. If the normal tier-one summary finishes without visible text, the plugin retries that summary in streaming mode through CPA. It uses only visible text from a completed reply. Hidden reasoning is never substituted for a summary. If neither transport produces a usable summary and bili refuses compression, the plugin reports the failure instead of sending the oversized history to the model.

### Using server-side bili after omp compaction

Use this optional mode for conversations where a text handoff is acceptable. For workloads that depend on encrypted reasoning across tool calls, keep provider-native compaction. With plugin 0.2.8, a tool call can start on the bili route and continue on the native route with a different prefix. The provider can report `input_transformations: [{type: "reasoning", action: "removed", reason: "prefix_mismatch"}]` even on a successful request. A handoff does not prevent this.

To try the text route in an omp conversation that already sends an opaque `compaction` item, run `/handoff` when the agent is idle. For example, `/handoff Preserve decisions, rejected alternatives, open tasks, and the latest tool results.` This asks omp's model to write a text handoff from the conversation. When omp replays that text and the recent turns, CPA can route eligible requests through bili again. A short session may report “Nothing to compact”. A handoff costs a model call and can omit details; check the facts you need on the next turn.

For subsequent automatic compactions, start omp with the optional [handoff preset](examples/omp-handoff.yml). From this repository run `omp --config examples/omp-handoff.yml`; from another directory pass the absolute path to the file. The preset replaces the compaction method order for that process with `handoff`, then `remote`, then `soft`. It keeps your thresholds, model, and tools. Starting without the preset restores your configured method order; a handoff already written to a session remains part of it.

The handoff is an omp operation. A **bili fold** occurs when bili replaces older messages with a summary in the request to the provider. Check **Billion Context → Overview** for folds and token savings. The preset does not force a fold, enable compression of deltas, or make unsupported encrypted reasoning eligible. Native compaction can use fewer tokens on short sessions; use the preset where longer conversations benefit from incremental folding.

## Uncompressed requests

A request that stays on the native CPA path is not an error, but some reasons are fixable by the operator. Those are logged once per ten minutes with the running count: `missing_caller_scope` (CPA did not report an authenticated caller), `missing_session_id`, `ambiguous_session_id`, `unusable_session_id`, and `protected_requires_session_id` (the protected replay lacks an explicit conversation ID). Each names the request that lost compression, so a client that never sends a conversation ID is visible instead of silently uncompressed.

The same counters are available without log access. `/v0/management/plugins/omp-billion-context/webui-data?target=/bypass` returns `{"counts":{...},"uncompressed":N}`, and the read-only overview response carries the same object as `cpaCompression`. `uncompressed` counts only the actionable reasons. Normal bypasses are counted separately: `previous_response_id`, `server_conversation`, `background`, `item_reference` and `compaction_input` distinguish different kinds of provider state. `encrypted_reasoning` means the protected sequence was not supported.

Session rows show the text a conversation starts with, so a wrapper tag such as `<system-reminder>` or `<job>` would otherwise become the title. The dashboard cleans display titles without changing session identity or counters. Open a row to see its full ID.

## Build and test

On the target platform, install Go, a C compiler, Node.js and Python. Run `npm ci --omit=dev --ignore-scripts` and `python scripts/package_runtime.py`. Use `python scripts/release.py library --target <os>-<arch>` to get the canonical versioned library filename, then build with CGO enabled and `go build -buildmode=c-shared -o <plugin-library> .`. Supported targets are `linux-amd64`, `darwin-arm64`, and `windows-amd64`.

Run `go test ./...`, `node --test test/kernel.mjs`, `node test/webui-storage.mjs`, and `node test/smoke.mjs <cpa-executable> <plugin-library> [replacement-library]`. The smoke test uses real CPA, the bundled engine, a synthetic provider, and a temporary server profile. It checks native metadata against `VERSION`, ordinary reconfigure state preservation, bounded busy refusal, and recovery after invalid-library replacement. Successful path-changing replacement requires a distinct native image built from the same source. Build that fixture with `go build -buildmode=c-shared -ldflags=-buildid=bili007-replacement-fixture -o <replacement-library> .`. Pass its path as the third argument or `BILI_TEST_REPLACEMENT_LIBRARY`. CI always supplies this fixture. The fixture is never included in release archives.

## Release protocol

`VERSION` is the single release version. The native plugin embeds it. `scripts/release.py` derives the library filename, release asset filename, and expected Git tag from it. This contract applies to every release and to both manual and Plugin Store installation.

1. Change `VERSION` in a PR. Complete review and checks before merging it.
2. Build each target from that source. Package it with `python scripts/release.py package --target <os>-<arch> --library <plugin-library> --dist dist`. Each ZIP contains exactly one nonempty root library named `omp-billion-context-v<VERSION>.<ext>`. Its asset name is `omp-billion-context_<VERSION>_<os>_<arch>.zip`.
3. Create the tag `v<VERSION>` on the reviewed commit. The tag workflow rejects a tag that differs from `VERSION`.
4. The workflow runs Go tests and the real CPA lifecycle smoke on all three platforms. It verifies the actual asset names and ZIP members together and checks rejection of a bad tag and an unversioned member.
5. Before publication, the workflow checks the tag, all three packages, and their checksums again. It publishes those verified assets through GitHub Releases. It does not install or deploy them to a running CPA instance.

To verify collected packages without publishing, run `python scripts/release.py verify --dist dist --tag v<VERSION>`.
