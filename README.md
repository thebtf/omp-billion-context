# omp-billion-context

A native CLIProxyAPI plugin that sends an opt-in model alias through [billion-context](https://github.com/ranxianglei/billion-context). Other models keep their usual CLIProxyAPI routes. The plugin supports OpenAI Chat Completions and Responses, including streaming. The upstream billion-context process owns the compression state and uses [acp-kernel](https://github.com/ranxianglei/acp-kernel). Both links provide upstream attribution.

## Set up the route

1. Build the library with Go, CGO, and a C compiler. For Windows, run `go build -buildmode=c-shared -o omp-billion-context.dll .`. Use `omp-billion-context.so` on Linux or `omp-billion-context.dylib` on macOS.
2. Copy the library into the `plugins` directory of a CGO-enabled CLIProxyAPI v8.1.4 or a fork with an equivalent cancellation-safe stream bridge. Older CPA forks can race when a client disconnects during a streamed reply. Set `plugins.enabled: true` in its config.
3. Install `billion-context@0.1.216` with Node.js 20 or later. Start `bili start` on the same machine. Keep bili on a numeric loopback address and give it a dedicated state directory if you run other bili instances. This plugin does not start bili or change your OMP profile.
4. Set these environment variables for the CLIProxyAPI process, then restart it:

| Variable | Value |
| --- | --- |
| `OMP_BILI_SIDECAR_URL` | The numeric IPv4 or IPv6 loopback origin of bili, such as `http://127.0.0.1:8787`. |
| `OMP_BILI_UPSTREAM_URL` | The next backend's base URL before `/v1`. Do not point back to a route using this same alias. |
| `OMP_BILI_MODEL_ALIAS` | The exact model name clients use to opt in. |
| `OMP_BILI_TARGET_MODEL` | The actual model name sent to the backend. It must differ from the alias. |
| `OMP_BILI_CLIENT_TOKEN` | A dedicated CLIProxyAPI client API key. Add the same key to CPA's `api-keys`. Do not share it across tenants. |
| `OMP_BILI_UPSTREAM_TOKEN` | Optional bearer token for the next backend. Use a separate key. |

Send requests to the alias with the dedicated bearer key and an opaque, stable `x-acp-session` header. Use a different session ID for each conversation. The plugin also accepts `x-session-id`, `session-id`, and `session_id` as aliases for the header. Configure the alias explicitly in clients that require model discovery. To use an existing CPA model and its configured OAuth or provider credentials, set `OMP_BILI_UPSTREAM_URL` to CPA's own base URL and `OMP_BILI_TARGET_MODEL` to that model's ordinary name. Set `OMP_BILI_UPSTREAM_TOKEN` to a second CPA API key. The ordinary model bypasses this plugin, so there is no routing loop. Do not enable the native billion-context extension for the same client route.

billion-context stores conversation state. Keep the loopback listener and its state directory private. If the sidecar stops, requests for the alias fail. Switch the client back to the ordinary model to bypass it.

## Check the route locally

Run `go test ./...`. To exercise the actual plugin, build the shared library and run `node test/smoke.mjs <cpa-executable> <bili-dist/index.js> <plugin-library>`. The smoke test uses synthetic requests and a local fake model backend. It does not use provider accounts or change installed profiles.