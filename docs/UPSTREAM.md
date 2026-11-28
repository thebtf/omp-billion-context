# Compression kernel integration contract

The native CPA plugin uses [acp-kernel](https://github.com/ranxianglei/acp-kernel) for model-driven folding. Version 0.3.0 replaces the private full billion-context server used in 0.2.9. The model writes summaries. The kernel validates and applies them. The dashboard and the continuation loop belong to this plugin.

| Owner | Contract |
| --- | --- |
| CPA | Chooses the model, provider and credentials for every call, including summary and replay calls. Native management authentication protects all session reads and server-setting writes. |
| Native Go library | Checks caller identity and route eligibility, keeps opaque Responses groups intact, dispatches bounded continuations, streams client-visible events and manages worker lifetime. Never sends management/provider credentials to the kernel. |
| Private worker | Uses the pinned kernel's public wire codecs and state operations, stores folds and exposes typed read projections. The interpreter is packaged with the plugin and has no service endpoint. |
| Dashboard | Uses the CPA management API on the same origin. The key stays in the browser. Dismissing a routing reminder does not change server priorities. Saving chat-notification visibility changes presentation, not compression. |

Stored Responses streams (omitted or `store: true`) keep one visible provider response and its native ID. Bili may request non-stored summaries before that stream. The in-stream private-tool continuation is reserved for `store: false`; server-owned continuations return directly to CPA. A rate limit or gateway failure from either the summary call or the visible model call ends the attempt. The plugin does not retry it as a context overflow. CPA and the client retain their own retry policies.

The ABI boundary is exercised against the released CPA binary (`test/smoke.mjs`). A test provider records the actual prompt on first dispatch, replay and next turn. Verify a folded range disappears, its model-written summary persists across restart, authentication and cache identity remain scoped, and an ordinary streaming answer is visible before the provider finishes. A successful answer alone does not prove compression. There is no claim about provider-side cache hit rate without a controlled live-provider experiment.

For a kernel upgrade, pin the npm version and integrity lock, rebuild the platform archive, and inspect its members. It must contain the kernel, interpreter, adapter and licenses, never the full `billion-context` npm application. Run the kernel, persistence and browser checks as well as the real-host smoke before releasing.
