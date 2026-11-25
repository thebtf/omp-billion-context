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

## Supported requests

The plugin handles authenticated HTTP Chat Completions and Responses when the request contains a stable conversation ID. It recognizes standard session headers, `session_id` and `prompt_cache_key`. A request without a usable ID or authenticated CPA caller scope follows the native CPA path. Responses WebSocket, compact and opaque continuations also stay native. Compression state is isolated by CPA client, model, and protocol, and resets when the plugin stops.

A fold necessarily rewrites part of the prompt. The integration tests observe the provider-side cache key, credential and unchanged prefix across a fold and its next turn. They do not measure a live provider's cache hit rate. CPA's own routing strategy still applies: an actual account switch, expiry or model switch can invalidate a warm cache. Nested model calls may appear as separate traces in LLM Tracing.

## Build and test

On the target platform, install Go, a C compiler, Node.js and Python. Run `npm ci --omit=dev --ignore-scripts` and `python scripts/package_runtime.py`, then build with CGO enabled and `go build -buildmode=c-shared -o <plugin-library> .`. Name the file `omp-billion-context.so` on Linux, `omp-billion-context.dylib` on macOS, or `omp-billion-context.dll` on Windows.

Run `go test ./...` and `node test/smoke.mjs <cpa-executable> <plugin-library>`. The smoke test uses real CPA and the bundled engine with a synthetic provider and temporary server profile.
