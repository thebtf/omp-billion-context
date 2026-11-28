# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Operators of a public CPA installation use this dashboard to check whether conversation compression helped and to investigate a session without reading its on-disk state. Model clients keep using their existing CPA endpoint.

## Product purpose

Use the model's own summaries and the standalone ACP kernel to fold conversation history. CPA stays responsible for model selection, providers, credentials and model transport. There is no additional gateway for users to install.

## Operating context

The dashboard is embedded in CPA Management Center, where a floating control strip can cover the upper right. Sessions contain private conversation data. The shell can be opened without exposing data; reads and writes require a CPA management key. An archive is distinguishable from a current session. Estimates of saved tokens are not billing or cache-hit measurements.

## Operator tasks

- Inspect reported provider cache reads and writes. Compare real usage before a fold with the replay and distinguish an unreported measurement from a cache miss.

- See actual folds and their estimated benefit. Open a session to inspect summaries or diagnose a rejected compression request.
- Control whether compression markers appear in model conversations without turning off compression.
- Use any supported language on narrow or wide screens, dismiss a priority reminder and forget the saved dashboard key without stopping the service.

## Evidence

`test/smoke.mjs` drives a local CPA and a synthetic provider. A protected prefix, restored fold, and scoped identity are observed in the model input, not inferred from a green status badge. A missing metric must not be presented as measured savings.
