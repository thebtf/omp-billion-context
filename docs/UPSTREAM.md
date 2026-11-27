# Upstream bili integration contract

We do not carry patches against the bundled billion-context source. Every line we
own lives in this repository as an adapter around bili's public HTTP surface. This
file names each dependency, so an upstream upgrade is a checklist instead of an
archaeology exercise.

Pinned by `package.json` and `package-lock.json`: billion-context 0.1.216,
Node.js 24.22.3. `scripts/package_runtime.py` fails if the installed tree does not
match the lock file.

## What we depend on

| Dependency | Where we use it | If upstream changes it |
| --- | --- | --- |
| `bili start --config <file> --no-auto-update --no-passthrough`, `host`/`port`/`upstream`/`logFile`/`mitm` keys | `runtime.go` engine launch | Engine does not start; the smoke test fails at `/__bili/health` |
| One listening port, `BILI_STRICT_PORT` | `runtime.go` | Port race; smoke fails before any model call |
| `GET /__bili/health` returning `{"ok":true}` | `waitForEngine` | Startup never reports ready; bounded failure inside 20 s |
| `POST /bili/<origin><path>` for `chat` and `responses` | `forward.go` `execute` | Compression silently stops; `skip_report.go` counters stay at zero while requests succeed |
| SSE event shape per protocol, `[DONE]` for chat | `stream.go` `forwardStream` | Streaming closes early with a reported error |
| Documented reads: `overview`, `sessions`, `sessions/{id}/{detail,tree,block-content,message}`, `stats`, `status`, `diagnostics`, `cache-report`, `sessions/{id}/report` | `management.go` `webUIReads`, `sessionReads` | The bridge rejects the target with HTTP 400 instead of proxying |
| `POST /__bili/notices/dismiss`, `POST /__bili/host-plugins/dismiss` | `webui.html` `dismissPaths` | Notices stop dismissing; the request is answered locally |
| Session rows: `id`, `title`, `hostTitle`, `firstBlockHint`, `live`, `restored` | `management.go` `withReadableTitles`, `webui.html` | Titles fall back to bili's raw text; nothing else breaks |
| Overview JSON, which we extend with `cpaCompression` | `management.go` `withBypassSummary` | The counters move to `?target=/bypass` only |
| Embedded WebUI markup: `#st-sessions`, `data-nav`, `#cpa-webui-notice` insertion point, `.embed`, `table.data`, `.row-title` | `webui.html` | Cosmetic: hidden tabs or title widths regress, data still loads |
| `prompt_cache_key` semantics: bili no longer stamps it by default and may drop it when an upstream rejects the key | nothing we call; we rely on the request passing through | Cache affinity changes with no plugin-side signal |

## What we deliberately do not touch

- We never rewrite `prompt_cache_key`, `messages`, `input` or model fields. CPA
  and bili own the prompt; the plugin moves bytes.
- We never call bili's config, logs, pricing, resign, upstream, packs or update
  endpoints. The bridge allowlist rejects them.
- We do not import bili's internals. `private-runtime.mjs` is loaded with
  `--import` and touches only `node:http` and `node:crypto`.

## Upgrade procedure

1. Bump `dependencies.billion-context` in `package.json`, run
   `npm ci --omit=dev --ignore-scripts`, and commit the refreshed lock file.
2. Run `go test ./...`, `node test/webui-storage.mjs`, and
   `node test/smoke.mjs <cpa> <library> <replacement-library>`.
3. Confirm the smoke output still reports folded history, a preserved prefix,
   preserved cache key, injected tools, and the unchanged overview reads. A pass
   here is the compatibility verdict; there is no separate matrix.
4. Check the table above against the new bili release notes and update any row
   that moved. A row that changed without a test failing is a coverage gap and
   needs a test before the version bump lands.
5. Only then bump `VERSION` and follow the release protocol in `README.md`.

The smoke test is the compatibility judge because it drives the real CPA binary
with the real bundled engine. It does not measure a live provider's cache hit
rate; that needs access and an A/B that this repository cannot run.
