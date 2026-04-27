# llama-swap-proxy

A lightweight reverse proxy that sits in front of [llama-swap](https://github.com/mostlygeek/llama-swap) and adds additional endpoints and features to serve as an adapter for other services.

1. **Transparent pass-through** — every request not handled below is forwarded to llama-swap unchanged.
2. **Dynamic `/sdcpp/*` routing** — requests under `/sdcpp/` are routed at request time to whichever stable-diffusion-cpp model is currently loaded in llama-swap, without needing to know its address in advance.
3. **Enhanced `/v1/models`** — filters out `sd` models and enriches the response with `context_length`, `max_context_length`, and `capabilities` so OpenAI-compatible clients get useful metadata.
4. **`/opencode` config endpoint** — generates a ready-to-use [opencode](https://opencode.ai) provider config JSON, with per-model context limits and capability flags (tool calling, reasoning, vision) derived from GGUF metadata and live llama-server state.
5. **Cross-device WebUI sync middleware** — injects sync bootstrap code into llama.cpp WebUI pages under `/upstream/<model>/` and centralizes both `localStorage` and IndexedDB state in a SQLite store.

---

## CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--listen` | `:5900` | Address and port to listen on |
| `--upstream` | `http://127.0.0.1:9290` | Base URL of the llama-swap instance |
| `--config` | `/ai/llama-swap/config.yaml` | Path to llama-swap's `config.yaml` (used by `/opencode`) |
| `--sessions-dir` | `/ai/sessions` | Directory for synchronized session state (`sessions.db` is created here) |
| `--default-user` | `user` | Default username used by sync endpoints when auth is not configured |
| `--isolate-model-user-states` | `false` | Isolate sync state per `/upstream/<model>/` namespace instead of global shared state |
| `--opencode-hostname` | _(request Host)_ | Custom host (and optional port) for `/opencode` response URLs, e.g. `myserver.local:5900`. Overrides the `Host` header derived from the incoming request. Useful when the proxy is accessed via a different address than clients should use. |
| `--opencode-include-model-type` | `""` | Comma-separated `metadata.model_type` values to include in `/opencode`. If set by itself, only matching model types are returned. |
| `--opencode-exclude-model-type` | `""` | Comma-separated `metadata.model_type` values to exclude from `/opencode`. If set by itself, all non-matching model types are returned. |

---

## Endpoints

### `GET /sdcpp/*` — dynamic stable-diffusion-cpp proxy

At request time the proxy:

1. Calls `GET <upstream>/running` to find all currently-loaded models.
2. Calls `GET <upstream>/v1/models` and finds the first entry whose `meta.llamaswap.model_type` is `"sd"` that is also in the running set.
3. Forwards the request (stripping the `/sdcpp` prefix is **not** done — the full path is passed through) to that model's direct proxy address reported by llama-swap.

Returns `503 Service Unavailable` if no SD model is currently loaded.

### `GET /v1/models` — enriched model list

Fetches the upstream `/v1/models` response and transforms it:

- `sd` model entries are removed entirely.
- Each remaining entry is reshaped into an OpenAI-compatible object:
  - `context_length` — the loaded context size (`meta.llamaswap.context`, i.e. the `-c` value used at launch).
  - `max_context_length` — architectural maximum (`meta.llamaswap.max_context`); falls back to `context_length` when not set.
  - `capabilities` — passed through from `meta.llamaswap.capabilities` verbatim.

### `GET /opencode` and `GET /v1/opencode` — opencode provider config

Returns a JSON document conforming to the [opencode config schema](https://opencode.ai/config.json). The hostname used in response URLs is taken from `--opencode-hostname` if set, otherwise from the `Host` header of the incoming request. Use `--opencode-hostname` when the proxy is reached via a different address than what opencode clients should connect to.

For each model defined in `config.yaml` that passes the configured `metadata.model_type` filters, the endpoint:

1. Applies `--opencode-include-model-type` and `--opencode-exclude-model-type` against `metadata.model_type`. If neither is set, all model types are eligible. If only include is set, only listed model types are eligible. If only exclude is set, listed model types are omitted. If both are set, exclude wins and everything else remains eligible.
2. Skips launch commands containing `--embedding`, because those are treated as embedding-only servers with no chat completions endpoint.
3. Expands macros in the model `cmd` before parsing using global `macros`, model-level `models.<id>.macros` (model-level wins), automatic `${MODEL_ID}`, and environment macros like `${env.NAME}`; `${PORT}` is intentionally not expanded for `/opencode` derivation because it is not needed for model selection or capability metadata.
4. Parses the expanded model `cmd` to extract the configured context (`-c` or `--ctx-size`) and model file path (`-m` or `--model`).
5. If a model file path is found, reads the GGUF header directly to obtain native `context_length` and the `tokenizer.chat_template` field.
6. Sets model limits with `limit.context` from the configured command context and `limit.input` from GGUF `context_length` when available.
7. Derives capabilities from the GGUF chat template unless the command explicitly overrides chat templating (`--chat-template` / `--chat-template-file`); command flags like `--skip-chat-parsing`, `--no-jinja`, `--reasoning off`, or `--reasoning-format none` can force capabilities off.
8. Any of the above can be overridden per-model in `config.yaml` under `metadata.capabilities`:

```yaml
models:
  my-model:
    aliases:
      - my-model-fast
      - my-model-latest
    cmd: llama-server -m /models/my-model.gguf -c 8192
    metadata:
      capabilities:
        tool_call: true
        reasoning: false
        vision: false
```

Any `aliases` entries are also emitted in the `/opencode` model map, reusing the same generated settings as the parent model but with the alias name.

Both paths return the same payload. The generated config can be dropped directly into an opencode `config.json` or fetched dynamically with an `extends` entry.

### Sync endpoints (`/api/sessions/<user>/...`)

These endpoints back the injected WebUI sync middleware and can also be used directly:

- `GET /api/sessions/<user>/snapshot?scope=<scope>` — returns merged centralized state for the given user/scope.
- `POST /api/sessions/<user>/sync?scope=<scope>` — uploads current client state (`localStorage` + IndexedDB), performs merge/upsert (last-write-wins for scalar key-values, union/upsert behavior for record-like structures), and returns merged snapshot.
- `GET /api/sessions/<user>/ws?scope=<scope>` — WebSocket notification channel for near-real-time cross-client refresh.

Notes:

- `scope` is `global` by default, or `model:<model-id>` when `--isolate-model-user-states` is enabled.
- Attachments discovered in IndexedDB payloads (base64 blobs/data URLs) are uploaded to server-side storage and replaced with attachment references in centralized JSON state. This keeps centralized truth while avoiding immediate full blob fan-out to all clients.

### `/upstream/<model>/` llama.cpp WebUI injection

For HTML responses detected as llama.cpp WebUI pages, the proxy injects a bootstrap script that:

1. Pulls centralized snapshot before app scripts run.
2. Applies synchronized `localStorage` and IndexedDB state.
3. Hooks browser writes to trigger sync uploads.
4. Opens WebSocket notifications for near-real-time refresh from other clients.
5. Runs periodic background sync as a safety net.

### `/*` — transparent pass-through

All other paths are forwarded verbatim to the upstream llama-swap instance via a standard reverse proxy.

---

## NixOS integration

This repo is a self-contained Nix flake. Add it as a flake input in your system `flake.nix`:

```nix
inputs = {
  # ...
  llama-swap-proxy.url = "github:emanspeaks/llama-swap-proxy";
};
```

Pass the module through to your NixOS configuration:

```nix
outputs = inputs@{ self, nixpkgs, llama-swap-proxy, ... }:
{
  nixosConfigurations.myhostname = nixpkgs.lib.nixosSystem {
    modules = [
      llama-swap-proxy.nixosModules.default
      # ...
    ];
  };
};
```

Then enable and configure the service:

```nix
services.llama-swap-proxy = {
  enable = true;

  # Optional — defaults to :5900
  port = 5900;

  # Optional — defaults to http://localhost:${config.services.llama-swap.port}
  # upstream = "http://localhost:9290";

  # Optional — defaults to /ai/llama-swap/config.yaml
  # config = "/path/to/llama-swap/config.yaml";

  # Optional — defaults to /ai/sessions (SQLite at /ai/sessions/sessions.db)
  # sessionsDir = "/ai/sessions";

  # Optional — defaults to "user"
  # defaultUser = "user";

  # Optional — defaults to false
  # isolateModelUserStates = false;

  # Optional — override hostname used in /opencode response URLs
  # opencodeHostname = "myserver.local:5900";

  # Optional — whitelist metadata.model_type values for /opencode
  # opencodeIncludeModelType = [ "llm" "vlm" ];

  # Optional — blacklist metadata.model_type values for /opencode
  # opencodeExcludeModelType = [ "embedding" "sd" ];

  # Optional — extra args passed verbatim to the binary
  # extraArgs = [ ];
};
```

The `upstream` option automatically tracks `services.llama-swap.port` so a port change in your llama-swap config propagates here without any extra edits.

### NixOS module options

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `enable` | bool | `false` | Enable the service |
| `port` | port | `5900` | TCP port to listen on |
| `upstream` | string | `http://localhost:${services.llama-swap.port}` | Upstream llama-swap URL |
| `config` | string | `"/ai/llama-swap/config.yaml"` | Path to llama-swap `config.yaml` used by `/opencode` |
| `sessionsDir` | string | `"/ai/sessions"` | Directory for centralized synchronized state; SQLite file is `sessions.db` inside this directory |
| `defaultUser` | string | `"user"` | Default session username used when auth is not configured |
| `isolateModelUserStates` | bool | `false` | Isolate synchronized state by `/upstream/<model>/` when enabled |
| `opencodeHostname` | string | `""` | Custom host (and optional port) for `/opencode` response URLs, e.g. `"myserver.local:5900"`. Empty string means use the request `Host` header. |
| `opencodeIncludeModelType` | list of string | `[]` | Whitelist of `metadata.model_type` values eligible for `/opencode` |
| `opencodeExcludeModelType` | list of string | `[]` | Blacklist of `metadata.model_type` values omitted from `/opencode`; takes priority over include |
| `extraArgs` | list of string | `[]` | Extra CLI arguments |
| `package` | package | flake default | Override the package |

---

## Building manually

Requires Go 1.25+ and [gomod2nix](https://github.com/nix-community/gomod2nix) if updating dependencies.

```sh
go build ./...
```

To build with Nix:

```sh
nix build
```

To update `gomod2nix.toml` after a `go.mod` change:

```sh
gomod2nix
```
