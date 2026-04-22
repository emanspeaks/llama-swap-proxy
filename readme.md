# llama-swap-proxy

A lightweight reverse proxy that sits in front of [llama-swap](https://github.com/mostlygeek/llama-swap) and adds additional endpoints and features to serve as an adapter for other services.

1. **Transparent pass-through** — every request not handled below is forwarded to llama-swap unchanged.
2. **Dynamic `/sdcpp/*` routing** — requests under `/sdcpp/` are routed at request time to whichever stable-diffusion-cpp model is currently loaded in llama-swap, without needing to know its address in advance.
3. **Enhanced `/v1/models`** — filters out `sd` models and enriches the response with `context_length`, `max_context_length`, and `capabilities` so OpenAI-compatible clients get useful metadata.
4. **`/opencode` config endpoint** — generates a ready-to-use [opencode](https://opencode.ai) provider config JSON, with per-model context limits and capability flags (tool calling, reasoning, vision) derived from GGUF metadata and live llama-server state.

---

## CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--listen` | `:5900` | Address and port to listen on |
| `--upstream` | `http://127.0.0.1:9290` | Base URL of the llama-swap instance |
| `--config` | `/ai/llama-swap/config.yaml` | Path to llama-swap's `config.yaml` (used by `/opencode`) |

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

### `GET /opencode` — opencode provider config

Returns a JSON document conforming to the [opencode config schema](https://opencode.ai/config.json). The provider name is derived from the `Host` header of the request so it works correctly whether accessed by hostname or IP.

For each non-SD model defined in `config.yaml` the endpoint:

1. Parses the model's `cmd` to extract the `-c` (context) and `-m` (model file path) flags.
2. If the model is currently running, queries its llama-server `/props` endpoint for the live `n_ctx` value (takes priority over the `-c` flag).
3. If a model file path is found, reads the GGUF header directly to obtain the native `context_length` (fallback when `-c` is not set) and the `tokenizer.chat_template` field.
4. Derives capabilities from the chat template:
   - **`tool_call`** — template contains `tools`, `tool_calls`, `[TOOL_CALLS]`, or `<tool_call>`.
   - **`reasoning`** — template contains `<think>`, `<|think|>`, `enable_thinking`, or `<|thinking|>`.
   - **`vision` / `attachment`** — the `--mmproj` flag is present in the launch command.
5. Any of the above can be overridden per-model in `config.yaml` under `metadata.capabilities`:

```yaml
models:
  my-model:
    cmd: llama-server -m /models/my-model.gguf -c 8192
    metadata:
      capabilities:
        tool_call: true
        reasoning: false
        vision: false
```

The generated config can be dropped directly into an opencode `config.json` or fetched dynamically with an `extends` entry.

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

  # Optional — extra args passed verbatim to the binary
  # extraArgs = [ "--config" "/path/to/config.yaml" ];
};
```

The `upstream` option automatically tracks `services.llama-swap.port` so a port change in your llama-swap config propagates here without any extra edits.

### NixOS module options

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `enable` | bool | `false` | Enable the service |
| `port` | port | `5900` | TCP port to listen on |
| `upstream` | string | `http://localhost:${services.llama-swap.port}` | Upstream llama-swap URL |
| `extraArgs` | list of string | `[]` | Extra CLI arguments |
| `package` | package | flake default | Override the package |

---

## Building manually

Requires Go 1.22+ and [gomod2nix](https://github.com/nix-community/gomod2nix) if updating dependencies.

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
