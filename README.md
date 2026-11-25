# omp-billion-context

A native CLIProxyAPI plugin that sends an opt-in model alias through [billion-context](https://github.com/ranxianglei/billion-context). Other models keep their usual CLIProxyAPI routes. The plugin supports OpenAI Chat Completions and Responses, including streaming. The upstream billion-context process owns the compression state and uses [acp-kernel](https://github.com/ranxianglei/acp-kernel). Both links provide upstream attribution.

## Install the plugin

Use a CGO-enabled CLIProxyAPI v8.1.4 or a fork with an equivalent cancellation-safe stream bridge. Older CPA forks can race when a client disconnects during a streamed reply.

Install `omp-billion-context` from the CLIProxyAPI Plugin Store when available. For manual installation, choose the [release archive](https://github.com/thebtf/omp-billion-context/releases) for the CPA server's operating system and architecture. Extract the library into CPA's `plugins` directory without renaming it. Set `plugins.enabled: true` in CPA's YAML configuration.

The library includes Node.js and billion-context. The plugin starts its own engine on the first matching request. You do not need to install Node.js, run npm, or start bili on the CPA server.

## Configure the model route

Set these environment variables for the CLIProxyAPI process, then restart it. Until a model alias is configured, the plugin loads without handling requests:

| Variable | Value |
| --- | --- |
| `OMP_BILI_UPSTREAM_URL` | The next backend's base URL before `/v1`. Do not point back to a route using this same alias. |
| `OMP_BILI_MODEL_ALIAS` | The exact model name clients use to opt in. |
| `OMP_BILI_TARGET_MODEL` | The actual model name sent to the backend. It must differ from the alias. |
| `OMP_BILI_CLIENT_TOKEN` | A dedicated CLIProxyAPI client API key. Add the same key to CPA's `api-keys`. Do not share it across tenants. |
| `OMP_BILI_UPSTREAM_TOKEN` | Optional bearer token for the next backend. Use a separate key. |

Send requests to the alias with the dedicated bearer key and an opaque, stable `x-acp-session` header. Use a different session ID for each conversation. The plugin also accepts `x-session-id`, `session-id`, and `session_id` as aliases for the header. Configure the alias explicitly in clients that require model discovery. To use an existing CPA model and its configured OAuth or provider credentials, set `OMP_BILI_UPSTREAM_URL` to CPA's own base URL and `OMP_BILI_TARGET_MODEL` to that model's ordinary name. Set `OMP_BILI_UPSTREAM_TOKEN` to a second CPA API key. The ordinary model bypasses this plugin, so there is no routing loop. Do not enable the native billion-context extension for the same client route.

billion-context stores conversation state. Keep the loopback listener and its state directory private. If the sidecar stops, requests for the alias fail. Switch the client back to the ordinary model to bypass it.

## Build and test

Build on the target operating system with Go, a C compiler, Node.js, npm, and Python. Run `npm ci --omit=dev --ignore-scripts`, then `python scripts/package_runtime.py`. The runtime archive must exist before you build the library with CGO enabled. For Windows, run `go build -buildmode=c-shared -o omp-billion-context.dll .`. Use `omp-billion-context.so` on Linux or `omp-billion-context.dylib` on macOS.

Run `go test ./...`. To exercise the actual plugin, build the shared library and run `node test/smoke.mjs <cpa-executable> <plugin-library>`. The smoke test uses synthetic requests and a local fake model backend. It does not use provider accounts or change installed profiles.