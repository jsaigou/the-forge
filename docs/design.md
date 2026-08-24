# The Forge — Design Document

**V3 last updated:** 2026-05-17 (fully shipped, Phases 0–8 complete)  
**V4 last updated:** 2026-05-18 (design complete — ready for implementation)  
**Status:** V4 design finalised. Implementation follows §16 Open Work priority order. V3 source lives on ForgeHost at `/opt/forge/`; read it before implementing each component.

---

## 1. Purpose

The Forge is a self-hosted AI inference orchestration system. Its job is to:

1. Run large language models on a single high-memory APU node (ForgeHost) via llama.cpp
2. Present a fixed, stable API surface to clients regardless of which model is loaded
3. Let an operator switch between inference modes (model + context + args) without reconfiguring any downstream tool
4. Provide a live ops dashboard with metrics, mode switching, model management, and configuration
5. **V4 adds:** Enforce authenticated access with role-based permissions, expose all administrative configuration through the UI (no SSH required for routine ops), and manage NFS model storage mounts from the dashboard

---

## 2. Hardware Platform

### ForgeHost — Ryzen AI Max+ 395 (Strix Halo)

This is the only inference node. Understanding its memory architecture is critical for all model and mode decisions.

| Layer | What it is | Size |
|---|---|---|
| Physical DRAM | LPDDR5x-8000, 256-bit bus | 128 GB |
| GTT | GPU-addressable system RAM (dynamic) | ~120 GB effective ceiling |
| GART | Fixed BIOS GPU aperture | 512 MB (kept minimal) |

**GTT is GPU memory.** The GPU reads model weights at full 256 GB/s via GTT. CPU access to the same memory runs at ~128 GB/s. There is no PCIe bus and no discrete VRAM pool — `rocm-smi` reports 2 GB "VRAM" (on-die only); the real model memory pool is `GTT Total`.

**GPU specs (Radeon 8060S, RDNA3.5 / gfx1151):**
- 40 Compute Units, 59.4 TFLOPS FP16/BF16
- Token generation is memory-bandwidth-limited at 60–80% GPU utilization — this is normal

### Backend selection

Two llama.cpp builds coexist on ForgeHost:

| Backend | Path | Ceiling |
|---|---|---|
| Vulkan (RADV STRIX_HALO) | `/opt/forge/llama.cpp/build-vulkan/bin/llama-server` | ~63 GB GTT |
| ROCm (HIP) | `/opt/forge/llama.cpp/build/bin/llama-server` | ~120 GB GTT (requires `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON`) |

Vulkan is 8–12% faster at token generation for models under the 63 GB ceiling. ROCm is required for any model above that ceiling. Backend is set per-mode in `config.toml`.

### Supporting nodes

| Node | Role | Hardware |
|---|---|---|
| PairNode | Image generation (ComfyUI — separate install from ForgeHost's, started/stopped via the Forge Node Agent rather than ForgeHost's local service-mode path; requires PairNode's `forge-agent.service` to be up) | RTX 5080, 16GB GDDR7, Ubuntu |
| Core | Ancillary services (LibreChat, Open Notebook, etc.) | Ryzen 7 8845HS, 90GB RAM, Docker |

---

## 3. Network Topology

All nodes are connected via Tailscale mesh (`example-tailnet.ts.net`). The Forge exposes named Tailscale services rather than IP addresses:

| Service | URL | Backing port |
|---|---|---|
| `svc:ops` | `ops.example.ts.net` | 5000 (dashboard) |
| `svc:a1` | `a1.example.ts.net` | 8080 (primary inference) |
| `svc:a2` | `a2.example.ts.net` | 8081 (secondary inference) |
| `svc:comfy-forgehost` | `comfy-forge.example.ts.net` | 3001 (ComfyUI on ForgeHost, `forge-comfyui` service mode) |
| `librechat` | `librechat.example.ts.net` | 3000 — **not** a ForgeHost `svc:`; LibreChat runs on Core in Docker with its own `librechat-tailscale` sidecar container, registering as a full peer node |
| `svc:workspace` | `workspace.example.ts.net` | — (Hermes Workspace; offline as of 2026-07, last seen 5d ago) |

**Ports 8080 and 8081 are fixed and never change.** Any client (LibreChat, VS Code Continue, agent frameworks) can hardcode `a1.example.ts.net` and always reach whatever model is loaded, regardless of mode switches.

Nodes cannot resolve their own Tailscale service names. Inter-process communication on ForgeHost uses `localhost`.

---

## 4. Core Design Decisions

### 4.1 Single config file as source of truth

`/etc/forge/config.toml` remains the primary config. All modes, ports, UI settings, users, and service parameters live here. `config_writer.py` mutates it losslessly via `tomlkit`. No regex/sed edits.

**V4 change:** Credentials (HF token, password hashes) are split to a separate secrets file (`/etc/forge/secrets.toml`, mode 0600 root:forge). `config.toml` references it by path; the application merges at load time. This prevents credentials appearing in git-exported configs or log output.

### 4.2 Fixed ports, mode-switched content

Ports 8080 and 8081 are permanent Tailscale endpoints. The mode switch changes what process binds to them. Clients never need reconfiguring.

### 4.3 Systemd as the service layer

Inference processes are systemd units (`forge-primary.service`, `forge-secondary.service`). The engine writes model-specific parameters into sysconfig env files before starting units.

**V4 change:** Service unit parameters (unit names, ports, timeouts) are configurable via the Settings → Services tab without SSH. Changes are written to `config.toml` and validated before save.

### 4.4 Slot-based independent loading

Primary and secondary slots can be loaded/unloaded independently. `load_to_slot()` only touches one unit. Supports planner/worker dual-model patterns.

### 4.5 Async mode switches via SSE

Mode switches run in a background thread. The frontend receives progress via a Server-Sent Events stream (`/api/v1/events`). The same SSE bus carries metrics heartbeats, download progress, and config change notifications.

**V4 deployment change:** Flask's built-in WSGI server (Werkzeug) blocks one OS thread per open connection. A single SSE client holds a thread indefinitely; a mode switch adds another. Under normal use (dashboard open + switch in progress + metrics poller) this exhausts a small thread pool quickly. V4 runs under `gunicorn -k gevent` which multiplexes all connections on a single-threaded event loop, eliminating this ceiling without requiring an async framework rewrite.

### 4.6 Authentication and authorization (V4 — wired)

V3 deferred auth entirely. V4 wires `auth.py` completely. The dashboard requires login; the inference ports (8080/8081) remain unauthenticated (they speak the OpenAI API protocol; auth there is delegated to the calling client). Programmatic access to the ops API uses API keys rather than session cookies.

### 4.7 Input validation at the boundary (V4 new)

All API request bodies are validated with Pydantic models before any business logic runs. This prevents path traversal in file references, invalid TOML injection via mode names, and malformed extra_args reaching systemd env files.

### 4.8 NFS mounts and configurable Notes page (V4 new)

Model storage remains local (`/opt/forge/models/`). NFS mounts provide access to output from other nodes (ComfyUI on PairNode, etc.). Mount definitions live in `config.toml` under `[storage.mounts]` and are managed via Settings → Storage.

A dedicated **Notes** page in the dashboard is a general-purpose configurable content area. Admins can write markdown, embed links, and add widget slots. It ships preconfigured with an NFS gallery widget pointed at the PairNode ComfyUI output mount, but the underlying feature is not specific to NFS or image output. Any content an operator wants to surface — service links, runbooks, model notes — can live there.

### 4.9 Tailscale-aware operation (V4 new)

The Forge treats Tailscale as a first-class infrastructure component rather than an assumed prerequisite. V4 discovers the tailnet at runtime, verifies that the required named services exist, and can create missing ones from the dashboard UI without requiring SSH.

**Discovery:** `tailscale status --json` provides the tailnet name (`MagicDNSSuffix`), the local node identity, and peer list. The tailnet name is never hardcoded — all service URLs are constructed from discovered values.

**Service verification:** `tailscale serve status --json` lists active serve configurations. The dashboard compares the configured expected services (`svc:ops`, `svc:a1`, `svc:a2`) against what's actually registered. Missing or misconfigured services surface as warnings on the dashboard and as action items in the Settings → Tailscale tab.

**Service creation:** When services are missing, the dashboard runs the `tailscale serve` commands directly via `subprocess` after operator confirmation:
```
tailscale serve --bg --service=svc:ops --https=443 localhost:5000
tailscale serve --bg --service=svc:a1  --https=443 localhost:8080
tailscale serve --bg --service=svc:a2  --https=443 localhost:8081
```
This requires the process running the dashboard to have access to the Tailscale socket (the `forge` user must be in the group that owns `/run/tailscale/tailscaled.sock`, typically `tailscale`).

**Service names are configurable.** The expected service names live in `config.toml [tailscale]`. The defaults match the standard Forge deployment but can be changed if the tailnet already uses those names for something else.

### 4.10 Trained context detection (V4 new)

GGUF files embed model metadata in their header, including the architecture's native context length (e.g., `llama.context_length`, `qwen2.context_length`). The engine reads this at model-scan time using the `gguf` package (metadata-only read, no tensor loading — fast even for 90 GB files). The trained context is compared against the mode's configured `context` value at two points: (1) in the mode builder as a pre-save validation hint, and (2) after each load via `/props` to produce a three-way comparison: trained → configured → actual loaded. This makes context mismatches visible and diagnosable rather than silent.

---

## 5. Security Model

### 5.1 Authentication

- **Browser sessions:** Flask-Login with Argon2id password hashing. Session cookies are `HttpOnly`, `Secure`, `SameSite=Strict`.
- **Passkeys (WebAuthn/FIDO2):** Phishing-resistant public-key credentials. Platform authenticators (Touch ID, Face ID, Windows Hello) and roaming authenticators (YubiKey, FIDO2 hardware keys) both work via the same code path — they are the same protocol. Passkeys are additive; password login remains as a fallback. Credentials are stored as public keys (never secret material) in `[[webauthn_credentials]]` in `secrets.toml`.
- **API keys:** Static tokens (stored as Argon2id hashes in `secrets.toml`) for programmatic clients. Passed via `Authorization: Bearer <token>` header. Each key has an associated role.
- **Tailscale as the network boundary:** The dashboard is only reachable from within the Tailscale mesh. Auth adds a second factor — Tailscale membership is not a substitute for login.

### 5.2 Authorization (RBAC)

| Role | Capabilities |
|---|---|
| `admin` | Full access: mode CRUD, config edits, user management, storage mounts, service config |
| `operator` | Mode switching, slot control, model downloads, restart, output browser — no config/user edits |
| `viewer` | Read-only: dashboard, metrics, output browser — no state-changing operations |

UI elements for actions outside the current user's role are **hidden**, not disabled.

All state-changing API endpoints check the caller's role. The mode-switch, load, and unload endpoints require `operator` or `admin`.

### 5.3 CSRF protection

All `POST`, `PUT`, `DELETE` endpoints require a CSRF token (double-submit cookie pattern). Tokens are embedded in all dashboard forms and the SSE-connected frontend JS. API key requests are exempt (stateless header auth is CSRF-immune).

### 5.4 Secrets management

```
/etc/forge/
├── config.toml     (640 root:forge)   — modes, ports, UI, service params
└── secrets.toml    (600 root:forge)   — hf_token, [[users]] with password_hash, [[api_keys]], [[webauthn_credentials]]
```

`config.toml` no longer contains any credentials. The engine loads `secrets.toml` separately at startup. `config_writer.py` routes mutations to the correct file.

**Secrets in memory — scrubbing rules:**
- HF tokens, password hashes, and API key material are never passed as positional arguments to functions (only as keyword args, reducing accidental logging).
- Exception handlers in `auth.py` and `config_writer.py` catch and re-raise with sanitised messages — raw exception args that may contain token strings are replaced with `"<redacted>"` before the exception propagates to Flask's error handler or the log.
- Python's default `logging` configuration is augmented with a `SensitiveFilter` that scrubs strings matching `sk-forge-[A-Za-z0-9]+` and `$argon2id$` patterns from all log records before they are written.
- `secrets.toml` is never read into a variable named anything that would trigger automatic serialisation (e.g., not returned from a route handler, not included in `repr()` of any model class).

### 5.5 Input validation

All API request bodies use Pydantic v2 models. Validation errors are returned with **field-level detail** — the exact field name, the value that failed (truncated if sensitive), and a human-readable reason:

```json
{
  "error": "validation_failed",
  "fields": {
    "remote_path": "must be an absolute path starting with '/' — got 'exports/models'",
    "options":     "unrecognised mount option 'rw' — NFS output mounts must be read-only"
  }
}
```

Pydantic v2's `ValidationError` is caught in a `before_request`-adjacent decorator; its `errors()` list is mapped to this format before returning `422`. Generic "invalid input" responses are not acceptable.

Field-specific constraints:
- Mode names: `^[a-z0-9][a-z0-9-_]{0,63}$` — prevents TOML key injection
- File paths: resolved against `models_dir`; must not escape via `..`
- `extra_args`: each element a non-empty string; no shell metacharacters in interpolated positions
- NFS remote path: must start with `/` (enforced with a message quoting this rule)
- NFS options: allowlist of known-safe NFS options; unknown options rejected with name

### 5.6 File upload validation

Icon uploads (`/api/v1/upload/icon`) and the auto-fetch path (`/api/v1/icons/from-hf`) require content validation, not just extension checking. SVG is the primary risk: it is XML that browsers execute — a malicious SVG served from the same origin as the dashboard can steal session cookies, forge API requests, and exfiltrate audit logs.

**SVG handling — rasterize at upload:**  
The preferred approach is to convert uploaded SVGs to PNG at upload time using Pillow or `cairosvg`, then discard the original. This eliminates the SVG attack surface entirely. If preserving SVG fidelity matters (e.g., crisp icons at any scale), every uploaded SVG must be sanitized before storage:
- Parse with `defusedxml` to prevent XXE
- Strip `<script>` elements
- Strip all event-handler attributes (`onload`, `onclick`, `onerror`, `onmouseover`, etc.)
- Strip `<use>`, `<image>`, `<feImage>`, and any element with `href` or `xlink:href` pointing outside the document (external URLs, `data:` URIs)
- Strip `<foreignObject>` entirely (embeds arbitrary HTML)
- Strip `<style>` tags — these can contain `url("data:...")`, `expression()` (IE), and `@import` directives that survive naive script-stripping. Either remove all `<style>` blocks or pass their content through a CSS allowlist (`cssutils` with `CSSParser(raiseExceptions=True)` and a safe-property allowlist). A missing `<style>` strip is the most commonly overlooked SVG XSS vector.

Never serve sanitized SVGs with `Content-Type: image/svg+xml` inline — add `Content-Disposition: attachment` or serve from a path covered by a restrictive `Content-Security-Policy: default-src 'none'` header.

**Raster images (PNG, WebP, JPEG):**
- Verify magic bytes match the claimed type — do not trust `Content-Type` or file extension alone
- Open and re-encode via Pillow: `Image.open(buf).verify()` then save as the target format. This strips metadata (EXIF), normalizes encoding, and rejects malformed files that exploit parser bugs
- Enforce maximum dimensions (e.g., 512×512 for icons) and maximum file size (1 MB) before decoding to prevent memory exhaustion from decompression bombs
- Generate a new UUID filename on save — never use the client-supplied filename

**mmproj files:**  
When a mode definition references an mmproj path, validate that the file starts with the GGUF magic bytes (`GGUF` at offset 0) and has a plausible minimum size (> 100 MB). This prevents a misconfigured path (e.g., pointing at a text file) from causing a silent llama-server crash.

**HuggingFace downloads:**  
HuggingFace repo metadata includes per-file SHA256 checksums. After each download completes, `hf_manager.py` should verify the file hash before marking the job `done`. Mismatch → mark `error`, delete the partial file, surface in the UI.

### 5.7 Security headers

All dashboard responses include:
```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Forge-Version: <version>
```
`X-Forge-Version` carries the running app version, useful for confirming deploys took effect and for client compatibility checks.

**`unsafe-inline` for styles** is the V4 initial state. It is required for the theme system (CSS custom properties applied via `<style>` tags per theme). The Phase 2 target is nonce-based CSP: Flask generates a cryptographic nonce per response (`secrets.token_hex(16)`), injects it into all `<style>` blocks in templates, and emits `style-src 'nonce-{nonce}'`. This eliminates `unsafe-inline` without changing the theme architecture. It is deferred from V4 initial release because it requires auditing every template for inline styles, but the path is clear and non-breaking.

### 5.7 Audit logging

All state-changing operations are written to `/var/log/forge/audit.log` as JSON lines:
```json
{"ts": "2026-05-17T14:23:01Z", "user": "testuser", "role": "admin", "action": "switch_mode", "target": "gemma4-31b", "result": "ok"}
```

Rate-limiting on login: 10 attempts / minute, keyed on the real client IP taken from `X-Forwarded-For` (`auth._client_ip()`), falling back to `remote_addr` only if XFF is absent. **Not** keyed on Tailscale node name as originally planned here — `tailscale serve` always presents `remote_addr` as `127.0.0.1` to the app, so node-name lookup would need an extra `tailscale status` round trip per request; XFF is already trustworthy since all ingress is forced through `tailscale serve`, so IP-keying was simpler and just as safe. (2026-07: the original `remote_addr`-keyed implementation was a live bug — every client shared the same `127.0.0.1` bucket, so 10 failed logins from *anyone* locked out *everyone* for 60s. Fixed in the multi-user QA remediation pass, see `multiuser-qa-remediation-plan.md`.) The same `_client_ip()` + `cachetools.TTLCache` pattern now also rate-limits failed Bearer-token attempts (`verify_api_key`/`verify_router_key`) — see §6.6. In-process counter; no Redis needed.

---

## 6. Component Architecture

```
/etc/forge/
├── config.toml                   ← Modes, ports, UI, service params (640 root:forge)
└── secrets.toml                  ← HF token, users, API keys (600 root:forge)

/opt/forge/
├── engine.py                     ← Mode engine core
├── app.py                        ← Flask dashboard + REST API + SSE bus
├── monitor.py                    ← Background metrics + hang detection
├── config_writer.py              ← Lossless TOML mutations (config + secrets routing)
├── hf_manager.py                 ← HuggingFace Hub integration
├── auth.py                       ← Flask-Login + Argon2 + CSRF + API key validation
├── validators.py                 ← Pydantic request models + file upload validation (magic bytes, Pillow re-encode, SVG sanitization)
├── audit.py                      ← Structured audit log writer
├── storage.py                    ← NFS mount management (systemd .mount unit generation + systemctl)
├── notes.py                      ← Notes page: widget registry, NFS gallery, markdown rendering
├── history.py                    ← Per-mode runtime history (context verification records, ring buffer)
├── tailscale.py                  ← Tailscale discovery, service verification, serve command execution
└── templates/
    ├── index.html                ← Main dashboard
    ├── login.html                ← Login page
    ├── setup.html                ← First-run wizard
    ├── notes.html                ← Configurable Notes page
    ├── help.html                 ← Help panel + doc renderer (no auth)
    └── settings.html             ← Settings page (6-tab layout: General, Services, Storage, Users, Tailscale, Appearance)

/etc/sysconfig/
├── forge-primary-env
├── forge-primary-args
├── forge-secondary-env
└── forge-secondary-args

/var/lib/forge/
├── current                       ← Active mode name + timestamp
└── slots.json                    ← Per-slot mode tracking

/var/log/forge/
└── audit.log                     ← JSON-lines audit trail
```

### 6.1 `engine.py` — V4 additions

All V3 responsibilities unchanged. V4 adds:

- **`read_model_metadata(model_path)`** — abstract interface for reading model file metadata. Currently backed by a GGUF implementation (`_read_gguf_metadata`) that reads the file header without loading tensors. The public function signature is format-agnostic: it returns a `ModelMetadata` dataclass with `trained_ctx`, `architecture`, `parameter_count`, and `quantization` fields regardless of the underlying format. If the GGUF package API changes (it has done so across llama.cpp versions), only the private implementation needs updating. Safetensors and other formats are explicitly out of scope for V4 — GGUF is the only inference format used — but the interface makes future extension non-breaking. The implementation reads `general.architecture` then resolves `{arch}.context_length`; logs a warning and sets `trained_ctx=None` if the key is absent.

- **`get_local_models()`** — annotates each discovered GGUF with `trained_ctx` from `read_gguf_metadata()`. Sharded models read metadata from the first shard. Builds a reverse map of `filename → [mode_names]`; both `model` and `mmproj` fields are indexed so mmproj files appear in the `in_use_by` list as `"mode_name (mmproj)"`.

- **Model tags** are stored in `config.toml` under `[model_tags]` (key = filename, value = list of strings). They are per-model, not per-mode. `get_local_models()` includes `"tags": [...]` per entry. `GET /api/v1/modes` enriches each mode response with `own_tags` (the mode's config-level `tags`, what the editor form writes) and a merged `tags` (model's `model_tags` first, then mode-only tags not already present). Dashboard buttons display the merged set; the mode editor only edits `own_tags`.

- **Three-way context comparison** — after `_wait_service_running()` succeeds, the engine compares:
  - `trained_ctx` — from GGUF metadata (what the model architecture supports)
  - `requested_ctx` — from the mode's `context` field in `config.toml` (what was asked for)
  - `actual_ctx` — from `/props` (what llama-server actually loaded)
  
  Logs a distinct warning for each mismatch type:
  - `requested > trained`: "configured context exceeds model's trained context — server will clamp"
  - `actual < requested`: "context silently reduced (GTT contiguous allocation likely)"
  - All three match: no warning
  
  All three values are passed to `history.append()` for the load record.

- **GTT retry with checkpoints** — if `actual_ctx < requested_ctx` (GTT contiguous allocation failure detected), the engine pushes a `ctx_reduced` SSE event that includes a suggested remediation: reload the slot with `--ctx-checkpoints 1` appended to `extra_args`. The dashboard surfaces this as a one-click "Retry with checkpoints" action on the affected slot, which calls `load_to_slot()` with the modified args without permanently changing the mode definition.

- **`check_system_params()` — runs once at startup** (called from `app.py` before the first SSE heartbeat). Reads live kernel and hardware state and returns a list of `SystemWarning` objects that are stored in `_system_warnings` and included in every `/api/v1/status` response. Warnings persist across page reloads; dismissed state is tracked in `/var/lib/forge/dismissed_warnings.json` keyed by warning ID. Warnings re-evaluate on each service restart so resolved issues clear automatically.

  Checks performed:

  | Check | Source | Threshold | Severity |
  |---|---|---|---|
  | GTT pool size | `/sys/class/drm/card0/device/mem_info_gtt_total` | < 100 GB | **warning** |
  | `amdgpu.gttsize` in kernel cmdline | `/proc/cmdline` | absent | **warning** |
  | `iommu=pt` in kernel cmdline | `/proc/cmdline` | absent | info |
  | ROCm available | `rocm-smi` exit code | non-zero | **warning** |
  | Vulkan STRIX_HALO driver | `vulkaninfo --summary` | absent | **error** |
  | `GGML_CUDA_ENABLE_UNIFIED_MEMORY` in primary sysconfig | `/etc/sysconfig/forge-primary-env` | absent | warning (only if ROCm present) |

  Each `SystemWarning` carries: `id`, `severity` (`error`/`warning`/`info`), `title`, `detail`, `fix_commands` (list of shell commands), and `docs_link`. The GTT and ROCm warnings include a specific note about Nemotron being the only mode that requires ROCm and the ~63 GB Vulkan ceiling.

### 6.2 `app.py` — V4 changes

- All routes prefixed `/api/v1/` (old `/api/` paths redirect with 301 during transition)
- Flask-Login `@login_required` on all routes except `/login`, `/api/v1/events` (SSE is session-authenticated at stream open), `/api/v1/status` (viewer-accessible), and `/help` (no auth)
- API key authentication via `before_request` hook for `Authorization: Bearer` requests
- CSRF token injection into all templates; CSRF validation on all mutating endpoints
- Role check decorator (`@require_role("operator")`) on switching/loading endpoints
- Audit log calls on every state-changing handler
- **Dashboard status bar** — rendered at the top of every page; shows: current mode, slot states, Tailscale service health (green dot per service), GTT usage, and a notification bell. The bell badge shows a count of unread `SystemWarning` items from `_system_warnings`. Tailscale service status is read from `tailscale.check_services()` (30s cached). If any expected service is missing, a yellow warning badge appears with a link to Settings → Tailscale.

### 6.3 `monitor.py` — unchanged from V3

Metrics loop (4s), hang detection, switch cooldown. Alert flow into SSE `status_update`.

### 6.4 `config_writer.py` — V4 changes

Gains awareness of which config keys belong in `secrets.toml`. `set_scalar("hf_token", ...)` routes to secrets; `set_scalar("default_mode", ...)` routes to config. Both files use the same lossless tomlkit round-trip pattern.

**Concurrency:** `tomlkit` is not thread-safe. All reads and writes go through a module-level `threading.Lock` already present in `engine.py` for the config cache. V4 extends this with a `filelock.FileLock` on the TOML path itself, so that the dashboard process and any concurrent CLI invocation of `engine.py` cannot race on the file. Lock order is always: acquire `FileLock` → acquire in-process lock → read/mutate/write → release both. This prevents config corruption during concurrent mode switches and settings saves.

### 6.5 `hf_manager.py` — unchanged from V3

### 6.6 `auth.py` — wired in V4

Previously written but not connected. V4 wires it fully:
- `login_manager.user_loader` reads from `secrets.toml` `[[users]]`
- `/login` POST validates Argon2id hash, sets session cookie
- `/logout` clears session
- `verify_api_key(token)` / `verify_router_key(token)` — tokens are `sk-forge-<keyid>-<secret>` / `sk-router-<keyid>-<secret>`; the keyid routes to exactly one `[[api_keys]]`/`[[router_keys]]` entry so a single Argon2 verify runs per request, instead of the original design's O(n) scan (Argon2 against every stored key in order until a match). Both are also rate-limited per client IP (10 failed attempts/60s, successes never count against the limit) — closes an unauthenticated CPU-exhaustion vector on the `request_loader` path. **No back-compat for pre-keyid tokens** — existing keys must be re-minted (2026-07 fix, see `multiuser-qa-remediation-plan.md`).
- `generate_csrf_token()` / `validate_csrf_token()` using `secrets.token_urlsafe`
- **Passkey CRUD** (`list_passkeys`, `get_passkey_for_credential`, `store_passkey`, `update_passkey_sign_count`, `delete_passkey`) — read/write `[[webauthn_credentials]]` in `secrets.toml` under filelock + in-process lock. Each entry stores `username`, `credential_id` (base64url), `public_key` (base64url COSE key), `sign_count`, `name`, `aaguid`, `created_at`. No private key material is ever stored — verification is public-key only.

### 6.7 `validators.py` — new in V4

Pydantic v2 models for every API request body, plus file upload validation logic (see §5.6). Examples:
```python
class ModeSwitchRequest(BaseModel):
    mode: str = Field(pattern=r'^[a-z0-9][a-z0-9\-_]{0,63}$')

class ModeCreateRequest(BaseModel):
    name: str = Field(pattern=r'^[a-z0-9][a-z0-9\-_]{0,63}$')
    label: str = Field(max_length=64)
    services: list[ServiceDefinition]
    # ...

class NfsMountRequest(BaseModel):
    name: str = Field(pattern=r'^[a-z0-9\-_]+$')
    server: str  # validated as hostname
    remote_path: str = Field(pattern=r'^/.*')
    local_path: str  # validated against allowed mount base
    options: str = Field(default="ro,hard,timeo=600")
```

### 6.8 `storage.py` — new in V4

Manages NFS mounts declared in `config.toml [storage.mounts]`. Mounts are for ComfyUI output access only — not model storage.

**Mount mechanism — systemd `.mount` units:** For each configured share, `storage.py` generates a systemd `.mount` unit file (e.g., `mnt-forge-pairnode.mount`) in `/etc/systemd/system/`. All mount and unmount operations go through `systemctl start/stop` rather than `subprocess mount`. This eliminates the `CAP_SYS_ADMIN` requirement from the dashboard process and avoids the subprocess-hang problem on stale NFS servers — systemd's `TimeoutSec=30` in the unit bounds the operation safely.

Exposes:
- `list_mounts()` — mount definitions + active state from `systemctl is-active`
- `mount(name)` — `systemctl start <unit>`; polls active state with timeout
- `umount(name)` — `systemctl stop <unit>`
- `generate_unit(name)` — writes/overwrites the `.mount` unit file, calls `systemctl daemon-reload`
- `remove_unit(name)` — disables, stops, and deletes the unit file
- `is_mounted(name)` — checks unit active state

Auto-mount on boot is a two-step flow: the mount unit must reach `active` state at least once before "Enable auto-mount on boot" appears. Enabling it adds `[Install] WantedBy=multi-user.target` and calls `systemctl enable`. The UI displays a boot-delay warning at that point.

### 6.9 `notes.py` — new in V4

Serves the configurable Notes page. The page is a list of sections defined in `config.toml [notes]`. Each section has a type and type-specific parameters. Admins edit sections via the Settings → Notes tab (or directly via the API). The page renders server-side; no client-side framework required.

**Section types:**

| Type | Description |
|---|---|
| `markdown` | Freeform text rendered as HTML (using `mistune`; sanitized with `bleach`) |
| `links` | A titled list of hyperlinks (label + URL pairs) |
| `nfs_gallery` | Paginated image/video gallery from a configured NFS mount |

**`nfs_gallery` implementation:**
- `list_files(mount_name, subdir, page)` — paginated directory listing filtered to `.png`, `.jpg`, `.webp`, `.mp4`, `.gif`
- `get_thumbnail(mount_name, rel_path)` — 256×256 JPEG thumbnail via Pillow (EXIF stripped); cached in `/var/cache/forge/thumbs/`. Cache key is `sha256(mount + rel_path)`. A systemd timer (`forge-thumb-prune.timer`, daily) removes thumbnails older than 7 days or prunes LRU entries when the cache directory exceeds 500 MB.
- `stream_file(mount_name, rel_path)` — serves original file as `Content-Disposition: attachment`
- Path safety: `rel_path` resolved against mount's `local` path; `..` and symlinks outside the mount root rejected

**Default configuration (preconfigured):**
```toml
[[notes.sections]]
type  = "markdown"
title = "ComfyUI Output"
body  = "Images and video generated by PairNode are browsable below."

[[notes.sections]]
type  = "nfs_gallery"
mount = "pairnode-output"
```

### 6.10 `history.py` — new in V4

Records the result of every inference service load attempt. Persists to `/var/lib/forge/mode_history.json`. Structure: one ring buffer (last 10 records) per mode key.

```python
# Written by engine.py after _wait_service_running() completes
record = {
    "ts": "2026-05-17T14:23:01Z",
    "trained_ctx": 131072,      # from GGUF metadata — what the model natively supports
    "requested_ctx": 524288,    # from config.toml mode definition
    "actual_ctx": 262144,       # from /props after load — None if service failed to start
    "load_time_s": 47.3,
    "result": "ctx_reduced"     # "ok" | "ctx_exceeds_trained" | "ctx_reduced" | "failed"
}
history.append(mode_name, record)
```

Result values:
- `ok` — actual_ctx matches requested_ctx (within 5%), and requested_ctx ≤ trained_ctx
- `ctx_exceeds_trained` — requested_ctx > trained_ctx (config asks for more than the model supports; server clamped it)
- `ctx_reduced` — requested_ctx ≤ trained_ctx, but actual_ctx < requested_ctx (GTT contiguous allocation failure)
- `failed` — service did not reach running state

`get_summary(mode_name)` returns:
- `last_result`: most recent record's result
- `ctx_reduction_rate`: fraction of recent loads where `actual_ctx < requested_ctx`
- `avg_load_time_s`: mean over recent successful loads
- `last_actual_ctx`: most recent verified context value
- `trained_ctx`: most recently observed trained context from GGUF metadata

This data drives the mode builder's context warning (replacing the speculative estimator) and the health indicator on each mode button on the dashboard.

### 6.11 `tailscale.py` — new in V4

Tailscale integration layer. All calls run `tailscale` CLI via subprocess with a 10s timeout (30s for serve commands). Results are cached with a 30s TTL to avoid hammering the daemon on every status poll.

```python
get_status()        → dict   # tailscale status --json; tailnet name, node identity, peers
get_tailnet()       → str    # MagicDNSSuffix, e.g. "example-tailnet.ts.net"
get_node_name()     → str    # Self.HostName, e.g. "forgehost"
get_serve_status()  → dict   # tailscale serve status --json; active serve configs
check_services()    → list   # each expected service with "active"|"missing"|"misconfigured" status + URL
create_service(name, port)   # runs tailscale serve --bg --service=svc:{name} --https=443 localhost:{port}
remove_service(name)         # runs tailscale serve --remove --service=svc:{name} --https=443
get_peer_names()    → list   # HostName list of known peers; used for NFS server validation
```

**`check_services()`** compares the expected service list from `config.toml [tailscale]` against parsed `serve status` output. Returns per-service:
- `status`: `active` / `missing` / `misconfigured` (service exists but points to wrong port)
- `url`: constructed HTTPS URL (e.g., `https://ops.example.ts.net`)
- `name`: service name (e.g., `ops`)
- `port`: expected local port

**Error handling:** if `tailscale status` fails (daemon not running, not logged in), all functions return a sentinel `{"tailscale": "unavailable", "reason": "..."}` rather than raising. The dashboard surfaces this as a "Tailscale unavailable" banner rather than a crash.

**Permissions note:** the `forge` user must be in the group that owns `/run/tailscale/tailscaled.sock`. On Fedora this is typically the `tailscale` group: `usermod -aG tailscale forge`. The `tailscale` group grants access to the local daemon socket only — it allows running Tailscale CLI commands as the local node. It does not grant the ability to alter other nodes' ACLs, routes, or configurations; those are controlled by the Tailscale control plane and the tailnet's ACL policy, which are independent of socket access.

### 6.12 `audit.py` — new in V4

Thin wrapper that appends JSON lines to `/var/log/forge/audit.log`. Called from `app.py` after every state-changing operation. Format: `{ts, user, role, action, target, result, detail}`.

---

## 7. Admin & Settings UX (V4 Redesign)

### 7.1 Admin page goals

V3's settings page required direct TOML editing for service configuration and had no guided setup flow. V4 redesigns the admin experience around three principles:

1. **No SSH required for routine operations** — all parameters configurable via UI
2. **Validation before commit** — errors caught before they reach systemd or tomlkit
3. **Progressive disclosure** — simple cases look simple; advanced options are collapsible

### 7.2 Settings page — 5-tab layout

#### Tab 1: General

Current V3 general settings plus:
- `default_mode` selector (dropdown populated from defined modes)
- `models_dir` path display (read-only in UI; local only — not an NFS path)
- Dashboard theme, accent color, sparkline window

#### Tab 2: Services

**Dashboard service bookmark manager.** All entries in the main dashboard Services panel are configured here — there are no hardcoded services. Entries are stored as `[[ui.bookmarks]]` in `config.toml` and fully editable (add, inline edit, remove).

**Bookmark fields:**

| Field | Required | Description |
|---|---|---|
| `label` | yes | Display name |
| `url` | yes | Link target |
| `icon` | no | Filename in `/static/icons/` |
| `sub` | no | Subtitle line (hostname) |
| `primary` | no | Large card (top) vs compact row (below divider) |
| `always_online` | no | Skip health check; always show as online |
| `health` | no | `"systemd_unit"` or `"tailscale_node"` — server-side health method |
| `health_arg` | no | Unit name or Tailscale node name for the health check |

Server-side health (when `health` is set) is computed in `_build_status()` and included as `online: bool` in the status response. Bookmarks without a `health` field are probed client-side via `fetch(url, {mode: 'no-cors'})` with a 2-failure threshold before going offline.

The inline edit form pre-fills all fields and saves on confirm without a page reload.

#### Tab 3: Storage

**New in V4.** Manages NFS mounts for accessing ComfyUI output from PairNode. Each configured mount is shown as a card:

```
┌──────────────────────────────────────────────────────┐
│ pairnode-output                        [Mounted ✓]   │
│ Server:  pairnode.example.ts.net            │
│ Remote:  /home/user/ComfyUI/output     [Unmount]     │
│ Local:   /mnt/forge/pairnode         [Edit] [Del]  │
│ Options: ro,hard,timeo=600                           │
│                              [Enable auto-mount ↓]   │
└──────────────────────────────────────────────────────┘
```

"Add NFS Share" opens a form. Every field has an inline tooltip (`?` icon) explaining the constraint in plain language:

- **Name** — identifier used in the output browser (alphanumeric, hyphens). *Tooltip: "Used as the label in the gallery and in config.toml. Cannot be changed after creation."*
- **Server** — hostname or Tailscale name. *Tooltip: "Use the Tailscale hostname (e.g. pairnode.example.ts.net) for nodes in your mesh. Check the Tailscale tab for available peers."*
- **Remote path** — *Tooltip: "The path on the server being shared. Must start with / — e.g. /home/user/ComfyUI/output"*
- **Local mount point** — *Tooltip: "Where the share will appear on this machine. Must be under /mnt/forge/ for security."*
- **Mount options** — *Tooltip: "NFS options string. Default ro,hard,timeo=600 means: read-only, retry indefinitely on server failure, 60s timeout per request. Use the ComfyUI preset unless you have specific requirements."* A "ComfyUI output" preset button fills `ro,hard,timeo=600,rsize=131072,wsize=131072`.

Saving the form creates the local directory if needed and writes the definition to `config.toml`. It does **not** mount immediately — the operator clicks Mount on the card to test it.

**Auto-mount on boot** is a secondary action, only available on a card that has been successfully mounted at least once. Enabling it generates a systemd `.mount` unit and enables it. The UI shows a warning: "A failing auto-mount unit can delay boot. Only enable after confirming the mount works reliably."

**Delete** unmounts first (if mounted), then removes the definition and the generated `.mount` unit if one exists.

#### Tab 4: Users & API Keys

- User list with role badges; Edit (change password, change role) and Deactivate buttons
- "Add User" form: username, password, role selector. Password is hashed server-side (Argon2id); never transmitted or stored in plaintext.
- API Keys section: list of named keys showing name, role, creation date, expiry date, and last-used timestamp (no secret ever shown after issuance)
- "Issue API Key" generates a `sk-forge-*` token, stores its Argon2id hash in `secrets.toml`, and shows the plaintext once in a modal with a copy button and a mandatory "I've copied this key" checkbox before dismiss
- Key creation form includes an optional **expiry** field: 30 / 60 / 90 / 365 days or "Never". Default is 90 days. Expired keys are rejected at auth time with a `401` and a `WWW-Authenticate: Bearer error="expired_token"` header.
- **Rotate** button on each key: issues a new secret, invalidates the old hash, shows the new secret once. Rotation does not interrupt active requests that started before the rotation.
- Expired keys shown in a separate "Expired" section with a Renew button (re-issues with a new expiry, same name and role)
- Auth settings: session TTL, remember-me duration, login rate limit

#### Tab 5: Tailscale

**New in V4.** Displays the live Tailscale network state and manages named services.

**Network identity panel** (read-only):
- Tailnet: `example-tailnet.ts.net` (discovered)
- This node: `forgehost` (from `tailscale status`)
- Status: Connected / Offline

**Services table:**

| Service | Port | Expected URL | Status |
|---|---|---|---|
| `svc:ops` | 5000 | `https://ops.example.ts.net` | ✓ Active |
| `svc:a1` | 8080 | `https://a1.example.ts.net` | ✗ Missing |
| `svc:a2` | 8081 | `https://a2.example.ts.net` | ✗ Missing |

Each row has a **Create** button (when missing) or **Remove** button (when active). "Create" runs the `tailscale serve --bg --service=svc:{name} --https=443 localhost:{port}` command and polls for the service to appear in `serve status`. The command and its output are shown inline before and after execution — no hidden magic.

A **"Create all missing"** button appears at the top when any service is absent. It runs all missing commands in sequence, showing progress.

**Customise service names** (collapsible advanced section): edits the `[tailscale]` keys in `config.toml` if the default names conflict with existing tailnet services.

**Peers panel**: lists known Tailscale peers by hostname — used as a convenience reference when configuring NFS mounts (PairNode's hostname is shown here).

#### Tab 6: Audit Log

**New in V4. Admin-only.** Provides in-dashboard visibility into the audit log without requiring SSH, directly supporting the "no SSH for routine ops" goal.

- **Log view** — paginated table of recent audit entries (newest first): timestamp, user, role, action, target, result. Default: last 100 entries.
- **Filter bar** — filter by user, action type, result (ok / failed), or date range. All filtering is client-side on the loaded page; larger ranges trigger a new API request.
- **Export** — "Download as JSON" exports the filtered view. Useful for incident investigation.
- **Live tail** — toggle to auto-refresh every 10s (polls `/api/v1/audit`). Useful during active sessions.

This is read-only. Audit entries cannot be edited or deleted from the UI.

**API:**

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/audit` | admin | Paginated audit log (`?page=&per_page=&user=&action=&result=&since=&until=`) |

#### Tab 7: Appearance

- **Theme** — moved to the main dashboard topbar (compact `<select>`, autosaves via `POST /api/v1/theme`). Not present on the Settings Appearance tab.
- **Accent color** — hex override; applied within the selected theme's lightness constraints (prevents unreadable combinations). Live preview via `previewAccent()`.
- **Sparkline window** — history retention in seconds (default 120s).
- **Service label** — the `// ops` part of the browser tab title and nav bar. Defaults to `ops`.

#### Implemented Settings tabs (as-shipped, may diverge from plan above)

| Tab | Contents |
|---|---|
| Monitoring | Poll intervals, KFD hang detection thresholds, GTT warning % |
| Services | Dashboard bookmark manager (`[[ui.bookmarks]]`) |
| Help Button | `[ui.help_button]` label/title/body; NFS fallback entries (`[[nfs.shares]]`) |
| Models | HF token, HF search + download, local model library + tags |
| Modes | Mode grid + 3-step mode builder (identity → type → services) |

The **Help Button** tab configures the `↗ NFS Shares` button on the dashboard. The popup body is Markdown, rendered server-side via mistune + bleach. If left blank, mount instructions are auto-generated from the NFS fallback entries.

### 7.3 Mode builder — human-error reduction

The existing mode builder (Phase 6) gains validation enhancements and presets in V4.

**Start from a preset** — a "Start from template" option appears before the blank form. Templates pre-fill backend, context, extra_args, and suggested tag/color for common configurations:

| Template | Pre-filled values |
|---|---|
| Dense text model (Vulkan) | backend=vulkan, context=131072, --parallel 2, --no-mmap |
| Large MoE / thinking model | backend=vulkan, context=262144, --parallel 2, --ctx-checkpoints 0 |
| Vision model with mmproj | backend=vulkan, context=131072, mmproj field enabled, --parallel 2 |
| ROCm large model (>63 GB) | backend=rocm, context=131072, --parallel 1, warning about GTT |
| Dual-slot coding pair | port_role guidance for primary+secondary, --parallel 2 each |
| Unloaded / standby | no services, model=none |

Selecting a template populates all fields; the operator then selects the model file and adjusts as needed. The template name is shown at the top of the form as a breadcrumb ("Starting from: Vision model with mmproj") with a "Clear and start blank" link.

**Field-level help:** every non-obvious field has an inline `?` tooltip matching the NFS form style. Examples: extra_args table explains `--parallel` (concurrent request slots, not threads), `--no-mmap` (required for ROCm; ignored by Vulkan), `--ctx-checkpoints 0` (required for Qwen/Gemma to prevent GTT leaks).

**Remaining validation enhancements:**

- **Model file selector** replaces freetext path — shows all `.gguf` files under `models_dir` (local only). Sharded models shown as a single entry. File sizes annotated.
- **Backend auto-suggestion** — if selected model size > 55 GB, suggests `rocm`; displays a warning if Vulkan is chosen for a model likely to exceed the ceiling.
- **Context panel** — three rows, populated as data becomes available:

  | Row | Source | Example |
  |---|---|---|
  | Trained context | GGUF metadata (read at model scan) | `131 072` |
  | Configured context | `config.toml` mode definition | `524 288  ⚠ exceeds trained` |
  | Last loaded context | Runtime history (`/props` after load) | `262 144  ⚠ reduced from configured` |

  "Exceeds trained" fires when configured > trained (server will silently clamp). "Reduced from configured" fires when actual < configured (GTT contiguous allocation failure). For a new mode with no load history, the third row shows "—  (no history yet)". This gives the operator the full picture before and after switching.
- **Port role conflict check** — warns if two services in the same mode claim the same port role.
- **`extra_args` structured editor** — key-value pair table for known flags (`--parallel`, `--no-mmap`, `--ctx-checkpoints`); raw append field for unknown args. Flags with known interactions (e.g., `--ctx-checkpoints 0` required for Qwen/Gemma) auto-suggested based on model alias pattern.
- **Pre-save validation** — before writing to TOML: model file exists, mmproj file exists if specified, no duplicate mode names.

### 7.4 First-run wizard

Triggered when `secrets.toml` does not exist or contains no active users. All routes (except `/static/`) redirect to `/setup`. Once an admin account is created, `/setup` redirects to the dashboard for all subsequent visits.

**Step 1 — Create admin account**

Single form: username (default suggestion: `admin`), password, confirm password. Password strength indicator shows entropy estimate. Submit is disabled until passwords match and minimum strength is met. On success, writes `[[users]]` to `secrets.toml` (creating the file with mode 0600) and auto-logs in.

**Step 2 — HuggingFace token** *(optional)*

Text field for HF token with a "Skip" link. Explains that the token is needed to download gated models (Gemma, Llama). On submit, writes `hf_token` to `secrets.toml`. Skip leaves it empty.

**Step 3 — Tailscale services**

Runs `tailscale status --json` and `tailscale serve status --json` to determine the current state. Three outcomes:

*Tailscale unavailable* — shows a warning ("Tailscale daemon not reachable — is tailscale running?") with a Skip link. The app will start without Tailscale awareness; the Settings → Tailscale tab can be used later.

*All services present* — shows a green summary:
```
Tailnet:   example-tailnet.ts.net
Node:      forgehost
svc:ops  → https://ops.example.ts.net  ✓
svc:a1   → https://a1.example.ts.net   ✓
svc:a2   → https://a2.example.ts.net   ✓
```
"Continue" proceeds to Step 4.

*Some services missing* — shows the table with missing rows highlighted and a "Create missing services" button. Clicking it shows each command that will be run, then executes them in sequence. Output (stdout/stderr) is streamed inline. After completion, re-checks status. If creation succeeds, "Continue" activates. If it fails (e.g., permission denied), an error with diagnostic hints is shown alongside a "Skip for now" link.

**Step 4 — Verify**

Read-only summary, no user input:
- Config file found at `/etc/forge/config.toml`: ✓ / ✗
- Models directory `<models_dir>`: ✓ exists / ✗ not found
- GGUF files found: N (list of filenames)
- Default mode: `<default_mode>` — defined / not defined

If `config.toml` is missing entirely, the wizard generates a minimal valid one from V4 defaults (empty modes, local models dir `/opt/forge/models`, default ports, Vulkan backend) before proceeding to Step 4. It presents this as "Generated a starter config — you'll add modes via the mode builder." If `models_dir` is absent or empty, a non-blocking warning is shown; inference won't be available until models are downloaded and a mode is defined, but the dashboard will start.

**Step 4 — Complete**

"Launch Dashboard" button. Redirects to the main dashboard, already authenticated.

The wizard is purely additive — it never modifies `config.toml`. All it does is create `secrets.toml` and set the HF token. Infrastructure configuration (modes, ports, services) is handled in the Settings page post-setup.

**Security guard:** `/setup/admin` returns `403 Forbidden` immediately if `secrets.toml` already contains at least one user. This prevents the route from being used to inject additional admin accounts after initial setup, since the route is CSRF-exempt and unauthenticated by design (no session exists at that point).

**Resetting to setup state:** the V4 `ai-mode reset_setup` CLI is retired. In v0.5,
wipe users/keys/passkeys from the dashboard's Settings → Danger panel (admin), which
operates on the store-backed auth tables; the next browser visit re-enters first-run
setup. Restart the daemon afterwards if prompted (`sudo systemctl restart forge-daemon.service`).

### 7.5 System notifications

System warnings from `engine.check_system_params()` are displayed as dismissible banners at the top of the dashboard, below the status bar. They render on page load from the `/api/v1/status` response; no extra API call needed.

**Banner anatomy:**

```
┌─────────────────────────────────────────────────────────────┐
│ ⚠  GTT pool is 63 GB — large models may not load  [How to fix ▾]  [✕] │
└─────────────────────────────────────────────────────────────┘
```

Expanding "How to fix" reveals:
```
GTT is GPU memory on Strix Halo. Without amdgpu.gttsize set in kernel
parameters, the pool is limited to ~63 GB, preventing models larger
than that from loading (Nemotron at 91 GB requires ~120 GB GTT).

Recommended fix — run as root, then reboot:
  sudo grubby --update-kernel=ALL --args="amdgpu.gttsize=122880"

After reboot, GTT should show ~120 GB in rocm-smi.
```

**ROCm unavailable banner** — shown when `rocm-smi` is not reachable or reports no device:

```
┌─────────────────────────────────────────────────────────────┐
│ ℹ  ROCm not available — models >63 GB cannot be loaded  [How to fix ▾]  [✕] │
└─────────────────────────────────────────────────────────────┘
```

Expanded:
```
ROCm (HIP) is required for models that exceed the Vulkan GTT ceiling
of ~63 GB. On this system, Nemotron Super (91 GB, 3 shards) is the
only configured mode that requires ROCm.

All other modes run on Vulkan and are unaffected.

To enable ROCm: ensure the ROCm therock build is present at
/opt/rocm-therock-7.13/ and that GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON
is set in /etc/sysconfig/forge-primary-env.

See: docs/pitfalls.md — ROCm tuning, GTT limits
```

**Vulkan error banner** — `error` severity; cannot be dismissed (Vulkan is required for all default modes):

```
┌─────────────────────────────────────────────────────────────┐
│ ✗  Vulkan (RADV STRIX_HALO) not detected — inference unavailable  [How to fix ▾] │
└─────────────────────────────────────────────────────────────┘
```

**Severity styling:**

| Severity | Color | Dismissible |
|---|---|---|
| `error` | Red border, `--danger` background tint | No — persists until resolved |
| `warning` | Amber border, `--warning` background tint | Yes — dismissed state saved |
| `info` | Blue border, `--accent` background tint | Yes |

Dismissed warnings are stored in `/var/lib/forge/dismissed_warnings.json`. On each service restart, all checks re-run; a previously dismissed warning that is now resolved simply does not reappear. One that is still present reappears (dismiss is per-boot, not permanent).

**API:**

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/warnings` | viewer | Current active system warnings |
| `POST` | `/api/v1/warnings/<id>/dismiss` | operator | Dismiss a warning for this session |

### 7.7 Per-mode runtime history on the dashboard

Each mode button on the main dashboard shows a small health indicator derived from `history.get_summary()`:

- **No history yet** — no indicator (first time the mode has ever been loaded)
- **Last load: OK** — subtle green dot; tooltip shows "Last loaded YYYY-MM-DD, ctx 131072"
- **Last load: context reduced** — amber dot; tooltip shows "Context reduced: loaded 262144 of 524288"
- **Last load: failed** — red dot; tooltip shows "Last load failed YYYY-MM-DD"

The mode detail panel (expanded view) shows a mini history log — the last 5 records as a small table with timestamp, result, and actual vs requested context. This gives the operator immediate visibility into whether a mode is reliable before switching to it.

### 7.8 Color themes

Four named themes ship with V4, defined in `forge/themes.py`. The active theme is stored as
`[ui].theme` in `config.toml` (also cached client-side in `localStorage` under
`forgehost.ops.theme`) and selected from a hamburger menu in the dashboard topbar — it applies
live and autosaves without a page reload.

| Key | Display name | Background | Accent | Character |
|---|---|---|---|---|
| `aurora` | Aurora | `#070418` | `#5BFCC4` | Deep purple-black, mint-green accent; **default**, dark |
| `fire` | Fire | `#0C0604` | `#FF6A1A` | Near-black with a warm radial glow, orange accent; dark |
| `graphite` | Graphite | `#18181b` | `#f59e0b` | Warm charcoal, amber accent; terminal / forge feel; dark |
| `spring` | Spring Bloom | `#C0AEB6` | `#2F9D4F` | Light gradient theme, botanical greens and blossoms |

Each theme defines 24 CSS custom properties (`--bg`, `--bg-grad`, `--grid`, `--panel`,
`--panel-strong`, `--border`, `--border-strong`, `--text`, `--text-dim`, `--text-mute`,
`--label`, `--accent` through `--accent-5`, `--accent-warm`, `--accent-cool`, `--ok`,
`--danger`, `--ring`, `--glow`, `--scan`, `--btn-fill`). No hardcoded hex values in templates.
(This V4 theme system and its `forge/themes.py` implementation were retired along with the
rest of `forge/*.py`, TOML decommission Phase 8, 2026-07-28 — v0.5's PWA has its own theming.)

**Gradient body background:** every theme sets `--bg-grad` (a `linear-gradient` or
`radial-gradient`; `graphite` uses `"none"`), applied via `background-image: var(--bg-grad)` on
`<body>` with `background-attachment: fixed`. Spring's light panels additionally require
`backdrop-filter: blur(14px) saturate(120%)` (via `[data-surface="light"]`) so the gradient
bleeds through the transparent panel fill.

**Theme switching:** selecting a theme from the hamburger menu calls `autoSaveTheme(name)`,
which applies the CSS vars from a client-side palette table and POSTs `{"theme": name}` to
`POST /api/v1/theme` (operator role). No page reload required.

### 7.9 Help panel

A `?` button in the nav bar (always visible, all roles, no authentication required to open) opens a slide-over help panel. The panel is rendered server-side from the existing `docs/*.md` files in the repository and served at `/help/<doc>`.

**Panel contents:**

- **Quick reference** — one-page cheat sheet: keyboard shortcuts, mode switch flow summary, slot controls, what the status indicators mean
- **Keyboard shortcuts** — full list (also shown inline as tooltips on interactive elements throughout the UI)
- **Documentation** — rendered versions of `docs/modes.md`, `docs/infrastructure.md`, `docs/pitfalls.md`, `docs/adding-a-model.md`; rendered server-side with `mistune` + `bleach`
- **Context-sensitive help** — the `?` button also appears inline on complex form fields (mode builder extra_args editor, backend selector, context field, NFS mount options). These open a focused popover with field-specific guidance rather than the full panel.

**Keyboard shortcut:** `?` key (when no input is focused) toggles the help panel.

**Implementation:** a single `/help` route in `app.py` (no auth required) serves the panel HTML. Documentation pages served at `/help/doc/<name>` map `name` to files in `docs/`. Allowlist: `quick-reference`, `modes`, `infrastructure`, `pitfalls`, `adding-a-model`, `bios-setup`. Path traversal guarded — only allowlisted names are served.

---

## 8. API Reference (V4)

All endpoints are under `/api/v1/`. The legacy `/api/` prefix 301-redirects to `/api/v1/` during the transition window.

Authentication: session cookie (browser) or `Authorization: Bearer <api-key>` (programmatic). Unauthenticated requests to protected endpoints return `401`. Role-insufficient requests return `403`.

### Auth

| Method | Path | Auth required | Description |
|---|---|---|---|
| `POST` | `/login` | — | Session login (`{username, password, csrf_token}`) |
| `POST` | `/logout` | session | Clear session |
| `GET` | `/api/v1/me` | any | Current user info and role |

### Help (no auth)

| Method | Path | Description |
|---|---|---|
| `GET` | `/help` | Help panel HTML (slide-over, embedded in page) |
| `GET` | `/help/doc/<name>` | Rendered markdown doc page (`modes`, `infrastructure`, `pitfalls`, `adding-a-model`) |

### Mode control

| Method | Path | Min role | Description |
|---|---|---|---|
| `POST` | `/api/v1/switch/<mode>` | operator | Async mode switch |
| `POST` | `/api/v1/restart` | operator | Restart current mode |
| `POST` | `/api/v1/load` | operator | Load mode into slot (`{mode, slot}`) |
| `POST` | `/api/v1/unload` | operator | Unload slot (`{slot}`) |

### Status and metrics

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/health` | — | Unauthenticated liveness check; returns `{"ok": true, "version": "..."}`. Used by systemd `ExecStartPost=` readiness probe and external monitors. |
| `GET` | `/api/v1/warnings` | viewer | Active system warnings (GRUB params, Vulkan, ROCm, etc.) |
| `POST` | `/api/v1/warnings/<id>/dismiss` | operator | Dismiss a warning for this boot session |
| `GET` | `/api/v1/status` | viewer | Full system state |
| `GET` | `/api/v1/metrics` | viewer | Current metrics snapshot |
| `GET` | `/api/v1/history` | viewer | Ring buffer history for sparklines |
| `GET` | `/api/v1/events` | viewer | SSE stream |

### Model management

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/models` | viewer | Local GGUF inventory |
| `GET` | `/api/v1/hf/search` | operator | HuggingFace search |
| `GET` | `/api/v1/hf/info` | operator | Repo file listing |
| `POST` | `/api/v1/hf/download` | operator | Start download |
| `GET` | `/api/v1/hf/jobs` | operator | Download job states |
| `POST` | `/api/v1/hf/cancel/<job_id>` | operator | Cancel download |

### Mode configuration

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/modes` | viewer | All mode definitions |
| `POST` | `/api/v1/modes` | admin | Create mode |
| `PUT` | `/api/v1/modes/<name>` | admin | Update mode |
| `DELETE` | `/api/v1/modes/<name>` | admin | Delete mode (409 if active/default) |

### Service configuration (V4 new)

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/services` | viewer | Current service config (units, ports, timeouts, env vars) |
| `PUT` | `/api/v1/services` | admin | Update service config; validates before writing |
| `GET` | `/api/v1/services/status` | viewer | Live systemd unit states for both slots |

### Storage / NFS (V4 new)

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/storage/mounts` | viewer | All configured mounts + current kernel state |
| `POST` | `/api/v1/storage/mounts` | admin | Add NFS mount definition |
| `PUT` | `/api/v1/storage/mounts/<name>` | admin | Update mount definition (must be unmounted first) |
| `DELETE` | `/api/v1/storage/mounts/<name>` | admin | Unmount (if mounted) and remove definition |
| `POST` | `/api/v1/storage/mounts/<name>/mount` | admin | Mount now |
| `POST` | `/api/v1/storage/mounts/<name>/umount` | admin | Unmount |
| `POST` | `/api/v1/storage/mounts/<name>/automount` | admin | Enable/disable auto-mount on boot (`{enabled: bool}`); blocked if mount has never succeeded |

### Tailscale (V4 new)

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/tailscale/status` | viewer | Tailnet name, node identity, peer list |
| `GET` | `/api/v1/tailscale/services` | viewer | Expected services with current status + URLs |
| `POST` | `/api/v1/tailscale/services/<name>/create` | admin | Run `tailscale serve --bg --service=svc:{name} ...`; streams output via SSE |
| `DELETE` | `/api/v1/tailscale/services/<name>` | admin | Run `tailscale serve --remove --service=svc:{name} ...` |

### Notes page (V4 new)

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/notes/sections` | viewer | All configured notes sections (type, title, params) |
| `POST` | `/api/v1/notes/sections` | admin | Add a section (`{type, title, ...params}`) |
| `PUT` | `/api/v1/notes/sections/<index>` | admin | Update a section |
| `DELETE` | `/api/v1/notes/sections/<index>` | admin | Remove a section |
| `POST` | `/api/v1/notes/sections/reorder` | admin | Reorder sections (`{order: [0,2,1]}`) |
| `GET` | `/api/v1/notes/gallery/<mount>/files` | viewer | Paginated file listing for nfs_gallery widget |
| `GET` | `/api/v1/notes/gallery/<mount>/thumb/<path:rel_path>` | viewer | 256×256 JPEG thumbnail (cached) |
| `GET` | `/api/v1/notes/gallery/<mount>/download/<path:rel_path>` | viewer | Original file download |

### Models — trained context (V4 new)

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/models` | viewer | Local GGUF inventory — now includes `trained_ctx` field per model |

### Users and API keys (V4 new)

| Method | Path | Min role | Description |
|---|---|---|---|
| `GET` | `/api/v1/users` | admin | List users |
| `POST` | `/api/v1/users` | admin | Create user |
| `PUT` | `/api/v1/users/<username>` | admin | Update user (role, password) |
| `DELETE` | `/api/v1/users/<username>` | admin | Deactivate user (cannot delete self) |
| `GET` | `/api/v1/apikeys` | admin | List API keys (name, role, created, expires_at; no secret) |
| `POST` | `/api/v1/apikeys` | admin | Issue API key (`{name, role, expires_in_days}`) → returns secret once |
| `POST` | `/api/v1/apikeys/<name>/rotate` | admin | Rotate key — invalidates old secret, issues new one |
| `DELETE` | `/api/v1/apikeys/<name>` | admin | Revoke API key |

### Passkeys (WebAuthn/FIDO2)

Registration endpoints require an active session (user must be logged in). Login endpoints are public (called before authentication). The RP ID is set via `[auth].webauthn_rp_id` in `config.toml`; should be the tailnet registrable domain (`example-tailnet.ts.net`) so credentials work across all nodes. The expected WebAuthn origin is derived dynamically from the browser's `Origin` header, so it works regardless of which hostname the dashboard is accessed from.

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/api/v1/auth/passkey/register/begin` | session | Generate registration challenge; challenge stored in session |
| `POST` | `/api/v1/auth/passkey/register/complete` | session | Verify attestation, store credential in `secrets.toml` |
| `POST` | `/api/v1/auth/passkey/login/begin` | none | Generate authentication challenge for `{username}` |
| `POST` | `/api/v1/auth/passkey/login/complete` | none | Verify assertion, call `login_user()`, return `{ok, redirect}` |
| `GET` | `/api/v1/auth/passkeys` | session | List current user's passkeys (metadata only, no key material) |
| `DELETE` | `/api/v1/auth/passkeys/<credential_id>` | session | Remove a passkey by base64url credential ID |

**Required config.toml keys** (`[auth]` section):
```toml
[auth]
webauthn_rp_id   = "example-tailnet.ts.net"   # registrable domain (not the node hostname)
webauthn_rp_name = "Forge"                    # display name shown by authenticator UI
# webauthn_origin is optional — leave unset to derive from request Origin header
```

**Required dependency:** `webauthn==2.7.1` (py_webauthn). Install with `uv pip install webauthn --python venv/bin/python`.

### Icons

| Method | Path | Min role | Description |
|---|---|---|---|
| `POST` | `/api/v1/upload/icon` | admin | Upload icon → `static/icons/` |
| `POST` | `/api/v1/icons/from-hf` | admin | Auto-fetch icon from HF repo |

### SSE event types (V4 — namespaced)

V4 introduces namespaced event types (`namespace:action`) to allow the frontend to subscribe selectively rather than parsing all events. The `event:` field in the SSE stream carries the namespaced name; the `data:` field carries the JSON payload.

| Event | Namespace | Trigger |
|---|---|---|
| `status:update` | status | 30s heartbeat; on connect |
| `mode:switch_started` | mode | Mode switch queued |
| `mode:switch_complete` | mode | Mode switch succeeded |
| `mode:switch_failed` | mode | Mode switch failed |
| `mode:load_started` | mode | Slot load queued |
| `mode:load_complete` | mode | Slot load succeeded |
| `mode:load_failed` | mode | Slot load failed |
| `mode:unload_complete` | mode | Slot unload finished |
| `mode:history_updated` | mode | New runtime history record written |
| `download:progress` | download | Every 2s for active HF downloads |
| `download:done` | download | Download completed |
| `download:failed` | download | Download error |
| `download:cancelled` | download | Download cancelled |
| `config:updated` | config | Mode or service config changed |
| `config:notes_updated` | config | Notes page sections changed |
| `storage:mount_changed` | storage | NFS mount state changed |
| `tailscale:status` | tailscale | Service created, removed, or daemon offline |
| `system:warning` | system | New system warning (GRUB, Vulkan, ROCm) |

Frontend JS can filter by prefix: `if (event.type.startsWith('mode:')) { ... }`. V3 event names are aliased during the transition window — both old and new names are emitted until the V3 client is retired.

---

## 9. Config Schema (V4)

### `/etc/forge/config.toml` (640 root:forge)

```toml
[forge]
node         = "forgehost"
models_dir   = "/opt/forge/models"
state_file   = "/var/lib/forge/current"
log_file     = "/var/lib/forge/switch.log"
audit_log    = "/var/log/forge/audit.log"
secrets_file = "/etc/forge/secrets.toml"
default_mode = "gemma4-31b"
primary_unit   = "forge-primary"
secondary_unit = "forge-secondary"

[ports]
primary   = 8080
secondary = 8081
embedding = 8083   # forge-embedding.service, CPU-only, checked by engine.py
stt       = 8084   # forge-stt.service, CPU-only, checked by engine.py
router    = 8085   # forge-router.service (a0 router) — see §18

[auth]
webauthn_rp_id   = "example-tailnet.ts.net"   # registrable domain, not a node hostname
webauthn_rp_name = "Forge"
session_ttl_hours = 8
remember_me_days  = 30
login_rate_limit  = "10/minute"

[system]
rocm_lib_path = "/opt/rocm-therock-7.13/lib"
[system.env]
HSA_ENABLE_SDMA  = "0"
MIOPEN_FIND_MODE = "FAST"

[tailscale]
primary_service   = "a1"
secondary_service = "a2"
dashboard_service = "svc:ops"

[[notes.sections]]
type  = "markdown"
title = "ComfyUI Output"
body  = "Images generated by PairNode."

[[notes.sections]]
type  = "nfs_gallery"
mount = "pairnode-output"

[storage]
# mounts are for external output access only; model storage is always local
[storage.mounts.pairnode-output]
server     = "pairnode.example.ts.net"
remote     = "/home/user/ComfyUI/output"
local      = "/mnt/forge/pairnode"
options    = "ro,hard,timeo=600"
automount  = false          # only enable after successful manual mount test

[modes.<name>]
label       = "Human label"
description = "Shown in UI"
family      = "Gemma"          # optional — groups modes in simple view; empty = standalone button
icon        = "name.svg"
color       = "#RRGGBB"
tags        = ["tag1", "tag2"]
backend     = "vulkan"         # "vulkan" | "rocm"

  [[modes.<name>.services]]
  unit              = "forge-primary"
  port_role         = "primary"
  model             = "file.gguf"
  alias             = "model-alias"
  context           = 131072
  mmproj            = ""
  startup_timeout_s = 480
  extra_args        = ["--parallel", "2", "--no-mmap"]

[ui]
theme               = "aurora"     # "aurora" | "fire" | "graphite" | "spring"
service_label       = "ops"        # browser tab and nav: "{node} // {service_label}"
sparkline_window_s  = 120
poll_interval_s     = 4
poll_fast_s         = 1

[ui.help_button]
label = "↗ NFS Shares"            # dashboard button text
title = "NFS Output Shares"        # popup modal title
body  = ""                         # Markdown body; empty = auto-generate from [[nfs.shares]]

[[ui.bookmarks]]
label      = "LibreChat"
url        = "https://librechat.example.ts.net"
icon       = "librechat.svg"
primary    = true
health     = "tailscale_node"
health_arg = "librechat"

[monitor]
inference_hang_stall_s = 90
inference_hang_min_tps = 0.1
gtt_warn_pct           = 85
```

### `/etc/forge/secrets.toml` (600 root:forge)

```toml
flask_secret = ""          # session signing key; auto-generated by the first-run wizard
hf_token     = ""          # HuggingFace auth token

[[users]]
username      = "testuser"
password_hash = "$argon2id$..."
role          = "admin"
active        = true

# Dashboard API bearer tokens (Authorization: sk-forge-<keyid>-<secret>).
# Field is `hash`, not `key_hash` — auth.verify_api_key() looks up entry["hash"].
# `keyid` (12 hex chars, secrets.token_hex(6)) routes the request to this single
# entry so only one Argon2 verify runs — added 2026-07, no back-compat for
# tokens minted before then (they lack keyid and must be re-minted).
[[api_keys]]
name       = "librechat"
keyid      = "a1b2c3d4e5f6"
hash       = "$argon2id$..."
role       = "operator"
created_at = "2026-05-17T00:00:00Z"
expires_at = "2026-08-17T00:00:00Z"   # omit or set to "" for no expiry
last_used  = "2026-05-18T09:14:00Z"   # updated by auth.py on each successful use

# WebAuthn passkey/security-key credentials, written by Settings → Security.
[[webauthn_credentials]]
username      = "testuser"
credential_id = "BASE64_CREDENTIAL_ID"
public_key    = "BASE64_PUBLIC_KEY"
sign_count    = 0
name          = "YubiKey 5C"
created_at    = "2026-05-20T00:00:00Z"

# Paired Forge Node Agents (§17). Written by POST /api/v1/nodes/pair.
[[nodes]]
id             = "pairnode"
label          = "PairNode"
address        = "https://ops-pairnode.example.ts.net"
token          = "..."
agent_identity = "user@github"
paired_at      = "2026-06-16T00:00:00Z"

# a0 router (§18) outbound provider credentials — plaintext, unlike
# api_keys/router_keys, since the router must present it upstream.
[[router_providers]]
name     = "deepseek"
api_key  = "..."
base_url = "https://api.deepseek.com"

# Bearer tokens accepted BY the a0 router (Authorization: sk-router-<keyid>-<secret>).
# Same shape as api_keys, including keyid.
[[router_keys]]
name       = "hermes"
keyid      = "2962d8558e91"
hash       = "$argon2id$..."
role       = "api"
created_at = "2026-07-02T00:00:00Z"
expires_at = ""
```

---

## 10. Mode Switching Flow

Unchanged from V3. See V3 §8 for the full state machine. The only V4 addition is an audit log entry written after `_save_mode()` completes.

---

## 11. Inference Modes

| Mode key | Label | Model | Context | Backend | Notes |
|---|---|---|---|---|---|
| `gemma4-31b` | Gemma 4 31B | gemma4-31b-abliterated-Q6_K | 262K | vulkan | Multimodal (mmproj), dense |
| `gemma4` | Gemma 4 26B | gemma4-26b-abliterated-Q8_0 | 262K | vulkan | Fast, low GTT |
| `qwen36` | Qwen 3.6 | Qwen3.6-35B-A3B-UD-Q6_K_XL | 524K (runs 262K) | vulkan | Thinking, multimodal, MoE |
| `qwen3coder-q6` | Qwen3 Coder Q6 | qwen3-coder-next-q6/UD-Q6_K_XL | 262K | vulkan | 3-shard, high fidelity |
| `coding` | Coding (Dual) | Qwen3.6 (primary) + Qwen3-Coder-Q4 (secondary) | 131K each | vulkan | Planner/worker dual-slot |
| `gemma4-e2b` | Gemma 4 E2B | gemma4-e2b-abliterated-Q4_K_M | 128K | vulkan | Ultrafast, multimodal |
| `gpt-oss-120b` | GPT-OSS 120B | GPTOSS-120B-Uncensored-MXFP4 | 131K | vulkan | Near ceiling; monitor GTT |
| `nemotron` | Nemotron Super | nemotron-super UD-Q4_K_XL (3 shards, 91GB) | 131K | **rocm** | Only mode requiring ROCm |
| `unloaded` | Unloaded | — | — | — | All inference stopped |

`creative` (ComfyUI) was previously listed here as an inference mode. It is now `type = "service"` — it appears in the Services panel with start/stop controls and is invisible to the mode-switching engine.

**Known issue — Qwen3.6 context reduction:** GTT contiguous allocation failure causes kernel to silently reduce from 524K to 262K. Engine logs a warning on each switch; hardware/kernel limitation.

---

## 12. Slot System

Unchanged from V3. Two inference slots, independent or atomic (via `switch_mode`). State in `slots.json`, reconciled by `_reconcile_slot_state()`.

| Slot | Unit | Port | Tailscale |
|---|---|---|---|
| primary | `forge-primary.service` | 8080 | `a1.example.ts.net` |
| secondary | `forge-secondary.service` | 8081 | `a2.example.ts.net` |

---

## 13. Hang Detection

Unchanged from V3. Polls `/metrics` every 4s; fires `INFERENCE_HANG` alert when `requests_processing > 0` AND both `prompt_tokens_seconds` and `predicted_tokens_seconds` < 0.1 t/s sustained for ≥ 90s. 120s cooldown after mode switch.

---

## 14. Deployment

### File layout on ForgeHost

```
/etc/forge/
├── config.toml                 (640 root:forge)
└── secrets.toml                (600 root:forge)    ← V4 new

/opt/forge/
├── engine.py, app.py, ...
├── validators.py               ← V4 new
├── audit.py                    ← V4 new
├── storage.py                  ← V4 new
├── notes.py                    ← V4 new
├── history.py                  ← V4 new
├── tailscale.py                ← V4 new
├── venv/
└── static/icons/

/opt/forge/models/
/opt/forge/llama.cpp/
├── build-vulkan/bin/llama-server
└── build/bin/llama-server

/usr/local/lib/forge/         (start-primary.sh, etc.)

/var/lib/forge/
├── current
├── slots.json
└── mode_history.json           ← V4 new; per-mode load records (ring buffer, last 10)

/var/cache/forge/
└── thumbs/                     ← V4 new; generated output thumbnails

/var/log/forge/
└── audit.log                   ← V4 new

/mnt/forge/                   ← V4 new; NFS mount base (ComfyUI output only)
└── <mount-name>/
```

### Systemd units

| Unit | Purpose |
|---|---|
| `forge-dashboard.service` | gunicorn+gevent app on port 5000 |
| `forge-primary.service` | Primary inference on port 8080 |
| `forge-secondary.service` | Secondary inference on port 8081 |
| `forge-boot.service` | Restores `default_mode` on startup |
| `forge-router.service` | a0 router on port 8085 — see §18 |
| `forge-embedding.service` | Always-on CPU embedding server, port 8083 (not mode-switched) |
| `forge-stt.service` | Always-on CPU speech-to-text server, port 8084 (not mode-switched) |
| `forge-tts.service` (+ `forge-tts-backup.service/timer`) | Qwen3-TTS server, port 8082, plus weekly voice-registry backup |
| `forge-agent.service` | Forge Node Agent — user unit deployed to remote nodes (e.g. PairNode), not ForgeHost |
| `forge-thumb-prune.service/timer` | V4 new — daily thumbnail cache cleanup |
| `forge-dep-scan.service/timer` | V4 new — weekly OSV + pip-audit dependency scan; findings surface as `system:warning` SSE events |
| `mnt-forge-<name>.mount` | V4 new — one per configured NFS share (generated by `storage.py`) |

**V4 dashboard service:** Runs `gunicorn -k gevent -w 1 -b 0.0.0.0:5000 app:app`. Single worker (state is in-process); gevent handles all concurrent SSE connections and background threads cooperatively without thread-per-connection overhead. `Restart=on-failure`, `RestartSec=5`, `ExecStartPost=/usr/bin/curl -sf http://localhost:5000/api/v1/health` for readiness.

**No `CAP_SYS_ADMIN` needed:** NFS mounts go through `systemctl start mnt-forge-*.mount` units, so the dashboard process requires no elevated capabilities.

### Deploy pattern

```bash
# Deploy a source file
cat forge/engine.py | tailscale ssh forgehost "cat > /opt/forge/engine.py"

# Deploy secrets (first time or rotation)
cat secrets.toml | tailscale ssh forgehost "cat > /etc/forge/secrets.toml && chmod 600 /etc/forge/secrets.toml"

# Fix SELinux labels after deploy
sudo chcon -t bin_t /usr/local/lib/forge/*
sudo chcon -R -t bin_t /opt/forge/venv/bin/

# Create NFS mount base directory
sudo mkdir -p /mnt/forge && sudo chown forge:forge /mnt/forge

# Grant forge user access to Tailscale socket (required for tailscale.py)
sudo usermod -aG tailscale forge

# Restart dashboard
sudo systemctl restart forge-dashboard.service
```

### Audit log rotation

`/etc/logrotate.d/forge-audit`:
```
/var/log/forge/audit.log {
    daily
    rotate 90
    compress
    missingok
    notifempty
    create 640 forge forge
}
```

### SELinux

Use `chcon -t bin_t` on all scripts and inference binaries. Do NOT use `restorecon` on `/usr/local/lib/forge/`. Apply `bin_t` to both llama-server binaries after any llama.cpp rebuild.

### Environment flags (ROCm-specific)

```
GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON   # Required — without this ROCm caps at ~63 GB
HSA_ENABLE_SDMA=0
MIOPEN_FIND_MODE=FAST
```

---

## 15. Install Script

### 15.1 Overview

V4 ships an `install.sh` that handles a complete fresh install or in-place upgrade from V3. The script is idempotent — safe to run multiple times. It detects existing state and skips steps that are already complete.

**Strict mode is on by default.** The script refuses to install or upgrade any Python package whose PyPI upload timestamp is less than `MIN_AGE_HOURS` old (default: 24). This defends against the first-day window of zero-day supply chain attacks, where malicious releases are picked up by automated systems before security researchers flag them.

**Fail closed.** If the PyPI API is unreachable and strict mode is active, the install aborts rather than proceeding unverified. An attacker who can block PyPI should not be rewarded with a bypassed check.

**Age ≠ safety — the dual-check problem.** The 24-hour hold has an inversion case: if the version currently in `uv.lock` is the compromised one (the attack is discovered after it aged through the window), strict mode will block the security fix because it is too new, trapping the system on the known-bad package. V4 addresses this with a parallel **OSV vulnerability scan** (§15.4). If a locked version has a confirmed CVE and a clean fix exists, the age check is suspended for that specific package under a structured security exception flow — not a blanket bypass.

### 15.2 Usage

```bash
# Standard install (strict mode on, 24h minimum age)
sudo ./install.sh

# Upgrade from V3 (detects existing install, migrates config)
sudo ./install.sh --upgrade

# Custom age window
sudo ./install.sh --min-age-hours 48

# Security exception — cite a CVE; suspends age check for affected packages only.
# The script verifies the CVE exists in OSV and that the target version resolves it.
sudo ./install.sh --cve CVE-2026-XXXXX

# Manual override for a specific package — for advisories not yet in OSV
sudo ./install.sh --override-package "cryptography==44.0.1" --reason "GHSA-2026-XXXXX, hash verified"

# Disable strict mode entirely — last resort; reason required
sudo ./install.sh --skip-strict --reason "Emergency recovery; OSV and PyPI both unreachable"

# Skip model download prompts (install app only; download models separately)
sudo ./install.sh --skip-models

# Dry run — show all packages, their ages, and OSV status; do not install
sudo ./install.sh --dry-run
```

### 15.3 Install flow

```
1. Parse arguments and load configuration

2. Platform verification  (no network; fast; hard failures abort immediately)
   │
   ├── OS
   │   ├── /etc/os-release: ID=fedora, VERSION_ID ≥ 44
   │   └── uname -r: kernel major.minor ≥ 7.0
   │
   ├── Firmware / BIOS  (informational — cannot read BIOS settings directly)
   │   │
   │   │   Linux userspace cannot read BIOS menu settings. The install script
   │   │   reads DMI tables and observes the effects of BIOS settings on the
   │   │   running system. Settings that cannot be observed are documented in
   │   │   docs/bios-setup.md and shown as a checklist at the end of this step.
   │   │
   │   ├── BIOS version + date (dmidecode -t bios, or /sys/class/dmi/id/bios_*)
   │   │     Display: vendor, version string, release date
   │   │     Cannot compare against "latest" — no standard API across vendors.
   │   │     Output includes the board vendor + model so the operator knows
   │   │     exactly where to check for updates.
   │   │     Warn if BIOS date is > 12 months old: "BIOS may be outdated —
   │   │     check <vendor> support page for <board_name>"
   │   │
   │   ├── Total memory (non-blocking)
   │   │     /proc/meminfo MemTotal should be ≥ 126 GB (~128 GB minus OS overhead)
   │   │     If < 126 GB: ⚠ "System reports only X GB — expected 128 GB.
   │   │       Check BIOS memory training completed (POST without beeps).
   │   │       See docs/bios-setup.md §Memory."
   │   │
   │   ├── Memory configured speed (non-blocking, requires dmidecode — root only)
   │   │     dmidecode -t 17: compare "Speed" (rated) vs "Configured Memory Speed"
   │   │     Rated speed for LPDDR5x-8000 = 8000 MT/s
   │   │     If configured < rated: ⚠ "Memory running at X MT/s, rated for 8000 MT/s.
   │   │       EXPO profile may not be active. See docs/bios-setup.md §Memory Speed."
   │   │     Note: some firmware reports in MHz (4000 MHz = 8000 MT/s DDR); the
   │   │     script normalises both representations before comparing.
   │   │
   │   ├── GTT pool size (observable consequence of BIOS GART setting)
   │   │     /sys/class/drm/card0/device/mem_info_gtt_total
   │   │     If < 100 GB: ⚠ "GTT pool is X GB — expected ~120 GB. BIOS GART
   │   │       aperture may be set too large, or amdgpu.gttsize kernel param
   │   │       not set. See docs/bios-setup.md §GART and GRUB settings."
   │   │     (GTT check overlaps with the GRUB check in engine.py; install-time
   │   │     version catches this before the dashboard starts)
   │   │
   │   ├── Secure Boot state (non-blocking)
   │   │     mokutil --sb-state or EFI var SecureBoot-*
   │   │     If enabled: ⚠ "Secure Boot is ON. Custom ROCm builds and unsigned
   │   │       kernel modules may require enrolling a MOK key. If ROCm fails to
   │   │       initialise, check dmesg for 'module verification failed'."
   │   │     (Secure Boot does not affect Vulkan; only ROCm kernel modules)
   │   │
   │   └── BIOS checklist reminder (always shown, regardless of check results)
   │         "Verify these BIOS settings manually — they cannot be checked from Linux:
   │           □ EXPO/XMP memory profile enabled (for 8000 MT/s)
   │           □ GART aperture set to minimum (512 MB)
   │           □ iGPU UMA frame buffer set to Auto or minimum
   │           □ BIOS is current — check: <vendor> <board_name> support page
   │         Full guide: docs/bios-setup.md"
   │
   ├── CPU / APU
   │   └── /proc/cpuinfo: "model name" must contain "Ryzen AI Max"
   │       (Strix Halo is the only supported platform; other hardware will
   │       produce different GTT ceilings, backend behaviour, and env requirements)
   │
   ├── GPU — Vulkan (BLOCKING — install cannot proceed without this)
   │   ├── /sys/class/kfd/kfd/topology/nodes/*/name: must contain "gfx1151"
   │   │   (Radeon 8060S / RDNA3.5 — the Strix Halo iGPU)
   │   └── vulkaninfo --summary: must report "RADV STRIX_HALO"
   │       Vulkan is required for ALL default inference modes. If not found:
   │         → ABORT with message and dnf install instructions for mesa-vulkan-drivers
   │       There is no --skip flag for this check. Install Vulkan, then re-run.
   │
   ├── ROCm (NON-BLOCKING — warning only)
   │   ├── /opt/rocm-therock-7.13/ exists → check rocm-smi for gfx1151 device
   │   ├── If ROCm absent or rocm-smi fails:
   │   │     ⚠ ROCm not available
   │   │       Models >63 GB cannot be loaded (Vulkan ceiling is ~63 GB GTT).
   │   │       Affected mode: 'nemotron' (91 GB, 3 shards) — will fail to load.
   │   │       All other configured modes use Vulkan and are unaffected.
   │   │       See docs/deployment.md for ROCm therock build instructions.
   │   └── Install continues regardless.
   │
   └── llama.cpp builds
       ├── /opt/forge/llama.cpp/build-vulkan/bin/llama-server  — exists + executable
       ├── /opt/forge/llama.cpp/build/bin/llama-server         — exists + executable
       └── Version smoke-test: each binary run with --version (timeout 5s)
           A binary that crashes or hangs here will cause cryptic failures later.

   Any hard failure in platform verification prints a diagnosis and aborts.
   Warnings (e.g., GPU not detected via one method but confirmed via another)
   are collected and shown together at the end of this step.

3. Model inventory  (no network unless download confirmed)
   │
   ├── Scan $models_dir (default: /opt/forge/models) for expected models:
   │
   │   Model               Pattern                      Required for modes
   │   ─────────────────────────────────────────────────────────────────
   │   Gemma 4 E2B         gemma4-e2b-*Q4_K_M*.gguf    gemma4-e2b (default)
   │   Qwen3.6             Qwen3.6-35B-*Q6_K*.gguf     qwen36, coding
   │   (others are optional; modes referencing missing models warn at config load)
   │
   ├── For each missing model, interactively offer download:
   │   ├── Show: model name, size, HuggingFace repo, required mode(s)
   │   ├── "Download now?" [Y/n/skip-all]
   │   ├── YES → run `hf download <repo> <file> --local-dir $models_dir`
   │   │         in the foreground (full progress output visible — not backgrounded)
   │   └── NO  → note that the mode will be unavailable; continue install
   │
   └── ComfyUI (PairNode) reachability check
       ├── Only runs if tailscale is active
       ├── curl --max-time 5 http://pairnode.<tailnet>:8188/system_stats
       ├── ✓ reachable  → note the NFS output path for Storage tab config
       └── ✗ unreachable → warn; 'creative' mode and Notes gallery will not function
           (non-blocking — PairNode may just be offline)

4. Check prerequisites
   ├── Python ≥ 3.12
   ├── uv (installs via official install script if missing — itself age-checked*)
   ├── tailscale daemon reachable
   ├── systemd (PID 1 check)
   └── curl (for PyPI API)

3. Verify lockfile integrity
   ├── Confirm uv.lock exists and is tracked in git
   ├── Check git log: was uv.lock modified in the last 24h?
   │   └── YES → print warning: "Lockfile recently modified — review git diff before proceeding"
   │             Prompt for confirmation (or --yes to skip)
   └── Verify uv.lock SHA256 matches .uv.lock.sha256 (a committed checksum of the lockfile)

4. [STRICT MODE] Dual-check all packages in uv.lock
   ├── Parse uv.lock to extract (package, version) pairs (including transitive deps)
   │
   ├── Pass A — OSV vulnerability scan (runs regardless of strict mode)
   │   ├── POST https://api.osv.dev/v1/querybatch  (batch all packages in one request)
   │   ├── For each response: record known CVEs against the locked version
   │   ├── VULNERABLE packages are flagged regardless of age — an old compromised
   │   │   version is not safer than a new one
   │   └── If OSV unreachable → WARN and continue (OSV is advisory, not blocking)
   │
   ├── Pass B — Age check
   │   ├── GET https://pypi.org/pypi/{pkg}/{ver}/json  (retry 3×, 10s timeout)
   │   ├── age_hours = (now_utc - earliest_upload_time).total_seconds() / 3600
   │   ├── if age_hours < MIN_AGE_HOURS:
   │   │     if package is VULNERABLE (from Pass A):
   │   │       → SECURITY_EXCEPTION: age check suspended (see §15.4)
   │   │     else:
   │   │       → FAIL (too new, no known CVE justifying urgency)
   │   └── if package not on PyPI → WARN and continue
   │
   ├── If PyPI unreachable → ABORT ("Cannot verify package ages — PyPI API unavailable")
   └── Report: collect all FAILs and VULNERABLE packages, show table, then ABORT if any

5. Verify lockfile integrity  (unchanged — see above)

6. [STRICT MODE] Dual-check all packages  (unchanged — see above)

7. Install Python environment
   └── uv sync --frozen --require-hashes

8. Create system user and directories
   ├── useradd --system --no-create-home --shell /sbin/nologin forge (if absent)
   ├── mkdir -p /etc/forge /opt/forge /var/lib/forge /var/log/forge
   │           /var/cache/forge/thumbs /mnt/forge
   └── chown/chmod per §14 file layout

9. Deploy application files
   └── rsync forge/*.py /opt/forge/

10. Config bootstrapping
    ├── Fresh install: copy config.toml.example → /etc/forge/config.toml if absent
    ├── Upgrade: run migrate_v3_to_v4.py
    └── Validate config with tomllib.loads()

11. Install systemd units
    ├── Copy *.service, *.timer → /etc/systemd/system/
    ├── systemctl daemon-reload
    └── systemctl enable forge-dashboard forge-boot forge-thumb-prune.timer

12. SELinux labels
    ├── chcon -t bin_t /usr/local/lib/forge/*
    ├── chcon -R -t bin_t /opt/forge/venv/bin/
    └── chcon -t bin_t /opt/forge/llama.cpp/build*/bin/llama-server

13. Tailscale group membership
    └── usermod -aG tailscale forge

14. Logrotate
    └── copy logrotate.d/forge-audit → /etc/logrotate.d/

15. Health check + summary
    ├── systemctl start forge-dashboard
    ├── Wait up to 30s for /api/v1/health → {"ok": true}
    └── Print install summary (see §15.7 — happy path output)
```

*`uv` itself: if `uv` is not installed, the script downloads the official `uv` installer, verifies its SHA256 against a hardcoded value committed in the repo, and checks that the `uv` binary's reported version was released more than `MIN_AGE_HOURS` ago before executing it. If the hash doesn't match, install aborts.

### 15.4 Dual-check: age verification + OSV vulnerability scan

**Age check** uses the PyPI JSON API. The script uses the **earliest** upload timestamp across all distribution files for a version (sdist and wheels may differ by minutes; earliest is conservative):

```python
# Embedded in install.sh, run via python3 -c
import sys, json
from datetime import datetime, timezone

data = json.load(sys.stdin)          # piped from curl pypi.org/pypi/{pkg}/{ver}/json
ver, min_age_hours = sys.argv[1], float(sys.argv[2])
files = data.get("releases", {}).get(ver, [])
if not files:
    print("NOT_FOUND"); sys.exit(0)
earliest = min(f["upload_time_iso_8601"] for f in files)
dt = datetime.fromisoformat(earliest.replace("Z", "+00:00"))
age_h = (datetime.now(timezone.utc) - dt).total_seconds() / 3600
print(f"TOO_NEW:{age_h:.1f}h:{earliest}" if age_h < min_age_hours else f"OK:{age_h:.1f}h")
```

**OSV scan** uses a single batch request to avoid per-package round-trips:

```python
# POST https://api.osv.dev/v1/querybatch
payload = {"queries": [
    {"package": {"name": pkg, "ecosystem": "PyPI"}, "version": ver}
    for pkg, ver in locked_packages
]}
# Response: {"results": [{"vulns": [{"id": "CVE-...", "summary": "..."}]}, ...]}
```

**Decision matrix — four outcomes per package:**

| Age | OSV status | Result | Action |
|---|---|---|---|
| ≥ 24h | Clean | ✅ OK | Install |
| ≥ 24h | Vulnerable | ⛔ BLOCK | Current version has known CVE — must resolve before install |
| < 24h | Clean | ⛔ BLOCK (strict) | Too new; no justification to bypass age hold |
| < 24h | Vulnerable | ⚠ SECURITY EXCEPTION | Age check suspended — see below |

**Security exception flow** (< 24h AND vulnerable current version):

The installer does not automatically proceed. It surfaces the situation explicitly:

```
SECURITY EXCEPTION DETECTED

  cryptography 44.0.0 (currently locked) has known vulnerabilities:
    CVE-2026-12345  "Buffer overflow in X.509 parser"

  cryptography 44.0.1 (available fix) is 3.2h old — below the 24h age threshold.

  OSV confirms 44.0.1 resolves CVE-2026-12345.
  PyPI hash for 44.0.1: sha256:abc123...

Options:
  [1] Install 44.0.1 as a security exception (recommended — CVE confirmed)
  [2] Remain on 44.0.0 and investigate manually
  [3] Wait until 44.0.1 is ≥24h old (approx 20h 48m)

  Your choice [1/2/3]:
```

Choosing option 1 logs a structured exception entry and updates `uv.lock` with the fix version before proceeding. The hash of the fix version is shown for manual verification.

Alternatively, passing `--cve CVE-2026-12345` on the command line pre-authorises the exception for that CVE without an interactive prompt. The script still verifies the CVE exists in OSV and that the candidate version resolves it — `--cve` is not a blanket bypass.

**Combined output when both issues exist:**

```
INSTALL BLOCKED — review required:

  STRICT VIOLATIONS (too new, no CVE):
  ─────────────────────────────────────────────────────────
  cffi           1.17.2     18.7h old    released 2026-05-16 19:31 UTC

  VULNERABLE VERSIONS (known CVE, fix available):
  ─────────────────────────────────────────────────────────
  cryptography   44.0.0     ✓ aged OK    CVE-2026-12345 → fix: 44.0.1 (3.2h old)

  BLOCKED — VULNERABLE + OLD (no fix available yet):
  ─────────────────────────────────────────────────────────
  requests       2.32.0     ✓ aged OK    CVE-2026-99999 → no fixed version on PyPI

To resolve:
  Wait ~5h for cffi to age through
  Run with --cve CVE-2026-12345 to install the cryptography fix now
  For requests: monitor https://osv.dev/vulnerability/CVE-2026-99999 for a fix release

All decisions are logged to /var/log/forge/install.log
```

### 15.5 Upgrade from V3

`--upgrade` mode:
1. Detects existing `/opt/forge/` and `/etc/forge/config.toml`
2. Backs up both to `/var/lib/forge/backups/YYYY-MM-DDTHHMMSS/`
3. Runs `migrate_v3_to_v4.py`:
   - Extracts `hf_token`, `[[users]]`, `[[api_keys]]` → `secrets.toml`
   - Replaces those fields in `config.toml` with `secrets_file` pointer
   - Runs round-trip validation on both files
   - Prints a diff of changes made
4. Continues with normal install flow from step 5
5. Restarts `forge-dashboard` after deploy

If migration fails, the backup is restored and the script exits non-zero.

### 15.6 Install log

Every install run appends to `/var/log/forge/install.log` as JSON lines:

```json
{"ts": "2026-05-17T14:23:01Z", "action": "install", "version": "4.0.0",
 "strict": true, "min_age_hours": 24, "packages_checked": 47,
 "osv_checked": 47, "vulnerabilities_found": 0, "exceptions": [], "result": "ok"}

{"ts": "2026-05-17T14:25:00Z", "action": "security_exception",
 "package": "cryptography", "from_version": "44.0.0", "to_version": "44.0.1",
 "cve": "CVE-2026-12345", "fix_age_hours": 3.2,
 "osv_confirmed": true, "pypi_hash": "sha256:abc123...", "result": "ok"}

{"ts": "2026-05-17T14:26:00Z", "action": "manual_override",
 "package": "cffi", "version": "1.17.2", "age_hours": 18.7,
 "reason": "GHSA-2026-XXXXX, hash verified", "result": "ok"}

{"ts": "2026-05-17T14:27:00Z", "action": "skip_strict",
 "reason": "Emergency recovery; OSV and PyPI both unreachable", "result": "ok"}
```

The distinction between entry types matters: `security_exception` entries are justified by an OSV-confirmed CVE; `manual_override` entries are operator-asserted; `skip_strict` entries are full bypasses. Auditing the log lets you distinguish a routine CVE patch from an unexplained bypass.

### 15.7 User feedback and output format

The script outputs to stderr (so stdout can be piped/redirected cleanly) using ANSI color codes. `--no-color` disables them for CI and logging contexts. `--quiet` suppresses everything except errors and the final summary. `--verbose` prints each package name as it is checked.

**Step banner format** — each step announces itself before doing any work:

```
[1/15] Platform verification
[2/15] Model inventory
[3/15] Checking prerequisites
...
```

The step counter is printed first so a hang is immediately locatable.

**Normal install — happy path:**
```
╔══════════════════════════════════════════════════════════╗
║  The Forge V4 — Installer                              ║
║  Mode: strict  ·  min-age: 24h  ·  fresh install        ║
╚══════════════════════════════════════════════════════════╝

[1/15] Platform verification

  Firmware
  ✓  BIOS: American Megatrends  v1.12.0  (2025-11-14)
     Board: ASRock X870E Taichi  —  check https://asrock.com/mb/AMD/X870E%20Taichi/ for updates
  ✓  Total memory: 128.0 GB  ✓
  ✓  Memory speed: 8000 MT/s configured  (EXPO active)
  ✓  GTT pool: 119.8 GB
  ⚠  Secure Boot: ENABLED
     Custom ROCm kernel modules may require MOK key enrollment.
     If ROCm fails: check dmesg for 'module verification failed'

  ┌─────────────────────────────────────────────────────────┐
  │  Verify these BIOS settings manually (cannot be read    │
  │  from Linux — check your BIOS menu):                    │
  │                                                         │
  │  □ EXPO memory profile active  (confirmed by speed ✓)   │
  │  □ GART aperture = 512 MB minimum                       │
  │  □ iGPU UMA frame buffer = Auto or minimum              │
  │  □ BIOS up to date — check asrock.com for v1.12.0+     │
  │                                                         │
  │  Full guide: docs/bios-setup.md                         │
  └─────────────────────────────────────────────────────────┘

  OS / CPU / GPU
  ✓  Fedora 44  (Workstation Edition)
  ...
```

**BIOS checks — memory speed warning:**
```
  Firmware
  ✓  BIOS: American Megatrends  v1.09.0  (2024-08-22)
     Board: ASRock X870E Taichi
     ⚠  BIOS date is 21 months old — check for updates at:
        https://asrock.com/mb/AMD/X870E%20Taichi/
  ✓  Total memory: 128.0 GB
  ⚠  Memory speed: 4000 MT/s configured  (rated: 8000 MT/s)
     EXPO profile may not be enabled in BIOS.
     Running at half the rated bandwidth reduces inference throughput.
     See docs/bios-setup.md §Memory Speed — enable EXPO, then reboot.
  ⚠  GTT pool: 63.1 GB  (expected ~120 GB)
     amdgpu.gttsize kernel parameter not set, or GART aperture too large in BIOS.
     See docs/bios-setup.md §GART and GRUB settings.
```

**Normal install — continued happy path:**
```
╔══════════════════════════════════════════════════════════╗
║  The Forge V4 — Installer                              ║
║  Mode: strict  ·  min-age: 24h  ·  fresh install        ║
╚══════════════════════════════════════════════════════════╝

[1/15] Platform verification
  ✓  Fedora 44  (Workstation Edition)
  ✓  Kernel 7.0.3-200.fc44.x86_64
  ✓  CPU: AMD Ryzen AI Max+ 395  (Strix Halo)
  ✓  GPU: gfx1151 detected  (Radeon 8060S / RDNA3.5)
  ✓  Vulkan: RADV STRIX_HALO driver active
  ✓  ROCm 7.13  at /opt/rocm-therock-7.13
  ✓  GTT pool visible  (rocm-smi: ~120 GB available)
  ✓  llama-server Vulkan  /opt/forge/llama.cpp/build-vulkan/bin/
  ✓  llama-server ROCm    /opt/forge/llama.cpp/build/bin/

[2/15] Model inventory
  ✓  Gemma 4 E2B      gemma4-e2b-abliterated-Q4_K_M.gguf    (5.4 GB)
  ✗  Qwen3.6          not found in /opt/forge/models

      Qwen3.6 (35B MoE, ~24 GB) is used by the 'qwen36' and 'coding' modes.
      Repo: <configured-repo-id>   File: Qwen3.6-35B-A3B-UD-Q6_K_XL.gguf

      Download now? [Y/n]:  y

      hf download <repo> Qwen3.6-35B-A3B-UD-Q6_K_XL.gguf --local-dir /opt/forge/models
      Fetching:  [████████████████████]  100%  24.1 GB  (4.2 MB/s)
  ✓  Qwen3.6 downloaded

  ✓  ComfyUI  reachable at pairnode.example.ts.net:8188
     Output path: /home/user/ComfyUI/output  (note for NFS mount config)

[3/15] Checking prerequisites
  ✓  Python 3.14.0
  ✓  uv 0.4.2  (released 2026-05-10, 7 days old)
  ✓  tailscale 1.68.0
  ✓  systemd (PID 1)
  ✓  curl 8.7.1

[4/15] Verifying lockfile integrity
  ✓  uv.lock present  (47 packages)
  ✓  uv.lock.sha256 matches

[5/15] Scanning for known vulnerabilities (OSV)
  Querying osv.dev for 47 packages...  done  (1.2s)
  ✓  No vulnerabilities found

[6/15] Checking package ages  (strict mode, min 24h)
  Querying PyPI  [████████████████████]  47/47  (8.3s)
  ✓  All 47 packages passed age check
     Oldest: setuptools 70.0.0  (412 days)
     Newest: filelock 3.14.0   (31 days)

[7/15] Installing Python environment
  →  uv sync --frozen --require-hashes
  ✓  47 packages installed  (14.1s)

[8/15] Creating system user and directories
  ✓  User 'forge' exists
  ✓  /etc/forge  /var/lib/forge  /var/cache/forge  /mnt/forge

[9/15] Deploying application files
  ✓  12 files → /opt/forge/

[10/15] Bootstrapping config
  ✓  /etc/forge/config.toml  (generated from template)

[11/15] Installing systemd units
  ✓  forge-dashboard  forge-primary  forge-secondary  forge-boot
  ✓  forge-thumb-prune.timer

[12/15] Applying SELinux labels
  ✓  /usr/local/lib/forge/*  →  bin_t
  ✓  /opt/forge/venv/bin/   →  bin_t
  ✓  llama-server (Vulkan + ROCm)  →  bin_t

[13/15] Configuring Tailscale group membership
  ✓  forge added to tailscale group

[14/15] Installing logrotate config
  ✓  /etc/logrotate.d/forge-audit

[15/15] Starting dashboard and health check
  →  systemctl start forge-dashboard
  Waiting for /api/v1/health  ....  ✓  (4s)

══════════════════════════════════════════════════════════
  ✓  The Forge V4 installed successfully  (48s total)

  Dashboard:   https://ops.example.ts.net
  Primary:     https://a1.example.ts.net
  Secondary:   https://a2.example.ts.net

  Next step:   https://ops.example.ts.net/setup
               (create your admin account)
══════════════════════════════════════════════════════════
```

**Platform failure — Vulkan not found (BLOCKING):**
```
[1/15] Platform verification
  ✓  Fedora 44
  ✓  Kernel 7.0.3
  ✓  CPU: AMD Ryzen AI Max+ 395
  ✓  GPU: gfx1151 detected
  ✗  Vulkan: RADV STRIX_HALO not found

  Vulkan is required for all default inference modes.
  The Forge cannot run without it.

  To install:
    sudo dnf install mesa-vulkan-drivers vulkan-tools
    vulkaninfo --summary | grep -i strix   # verify

  Install aborted.  (This check cannot be skipped.)
```

**ROCm absent — warning only, install continues:**
```
[1/15] Platform verification
  ✓  Fedora 44
  ✓  Kernel 7.0.3
  ✓  CPU: AMD Ryzen AI Max+ 395
  ✓  GPU: gfx1151  /  RADV STRIX_HALO
  ⚠  ROCm not found at /opt/rocm-therock-7.13
     Models >63 GB cannot be loaded (Vulkan GTT ceiling ~63 GB).
     Affected: 'nemotron' mode (91 GB) — will fail to load until ROCm is installed.
     All other modes (Vulkan) are unaffected.
     See: docs/deployment.md — ROCm therock build

  ✓  llama-server (Vulkan)  /opt/forge/llama.cpp/build-vulkan/bin/
  ✗  llama-server (ROCm)    not found  (expected at /opt/forge/llama.cpp/build/bin/)
     (non-blocking — only required if ROCm is installed)
```

**Platform failure — llama.cpp Vulkan build missing (BLOCKING):**
```
[1/15] Platform verification
  ✓  Fedora 44
  ✓  Kernel 7.0.3
  ✓  CPU: AMD Ryzen AI Max+ 395
  ✓  GPU: gfx1151  /  RADV STRIX_HALO
  ✓  ROCm 7.13
  ✗  llama-server (Vulkan)  not found at /opt/forge/llama.cpp/build-vulkan/bin/

  llama.cpp Vulkan build is required.  Build it first:
    cd /opt/forge/llama.cpp
    cmake -B build-vulkan -DGGML_VULKAN=ON -DGGML_HIP=OFF -DCMAKE_BUILD_TYPE=Release
    cmake --build build-vulkan --target llama-server -j32
    sudo chcon -t bin_t build-vulkan/bin/llama-server

  Install aborted.
```

**Model download declined:**
```
[2/15] Model inventory
  ✓  Gemma 4 E2B  (5.4 GB)
  ✗  Qwen3.6  not found

      Download now? [Y/n]:  n

  ⚠  Qwen3.6 skipped — modes 'qwen36' and 'coding' will not function.
     Download later:
       hf download <repo> Qwen3.6-35B-A3B-UD-Q6_K_XL.gguf --local-dir /opt/forge/models
```

**Strict mode violation:**
```
[4/13] Checking package ages  (strict mode, min 24h)
  Querying PyPI  [████████████████████]  47/47  (9.1s)
  ✗  2 packages below the 24h age threshold

  ┌─────────────────────────────────────────────────────────┐
  │  STRICT MODE — packages too new to install              │
  ├──────────────────┬─────────┬──────────┬─────────────────┤
  │  Package         │ Version │ Age      │ Released (UTC)  │
  ├──────────────────┼─────────┼──────────┼─────────────────┤
  │  cryptography    │ 44.0.1  │  3.2h    │ 2026-05-17 11:04│
  │  cffi            │  1.17.2 │ 18.7h    │ 2026-05-16 19:31│
  └──────────────────┴─────────┴──────────┴─────────────────┘

  Wait ~5h 18m for both packages to clear the threshold, or:
    --override-package "cryptography==44.0.1" --reason "<reason>"
    --override-package "cffi==1.17.2" --reason "<reason>"

  Install aborted.
```

**Security exception (OSV confirms CVE, fix is new):**
```
[3/13] Scanning for known vulnerabilities (OSV)
  Querying osv.dev for 47 packages...  done  (1.4s)
  ⚠  1 vulnerability found

[4/13] Checking package ages  (strict mode, min 24h)
  Querying PyPI  [████████████████████]  47/47  (8.8s)

  ┌─────────────────────────────────────────────────────────────────────┐
  │  SECURITY EXCEPTION — vulnerable package has a recent fix           │
  ├───────────────┬──────────────────────────────────────────────────── ┤
  │  Package      │  cryptography                                        │
  │  Locked ver.  │  44.0.0  ✓ aged (25 days)  ✗ CVE-2026-12345        │
  │               │  "Buffer overflow in X.509 parser"                  │
  │  Fix version  │  44.0.1  ⚠ only 3.2h old  ✓ resolves CVE-2026-12345│
  │  Fix hash     │  sha256:abc123def456...                              │
  └───────────────┴─────────────────────────────────────────────────────┘

  OSV confirms 44.0.1 resolves CVE-2026-12345.
  Age check suspended for this package only.

  Install fix now?  [Y/n]:
```

**Lockfile freshness warning:**
```
[2/13] Verifying lockfile integrity
  ✓  uv.lock present  (47 packages)
  ✓  uv.lock.sha256 matches
  ⚠  uv.lock was modified 18h ago

     git show HEAD:uv.lock | diff - uv.lock | head -20
     (shows the diff inline if --verbose)

  Review the lockfile change before proceeding.
  Continue?  [y/N]:
```

**Output flags:**

| Flag | Effect |
|---|---|
| *(default)* | Step banners + progress bars + result lines |
| `--verbose` | Also prints each package name during age/OSV checks |
| `--quiet` | Errors and final summary only |
| `--no-color` | Plain text; suitable for logging and CI |
| `--yes` | Skip all confirmation prompts (non-interactive installs) |
| `--skip-models` | Skip model inventory and download prompts entirely |
| `--skip-platform-check` | Override platform checks (unsupported; logged) |

`--yes` does not bypass security checks — it will still abort on strict violations and still prompts for security exceptions unless `--cve` is also provided. `--yes` **does** auto-accept model download prompts (downloads all missing default models without asking).

### 15.8 Files added to repo

```
install.sh                       ← Main install script
install.sh.sha256                ← SHA256 of install.sh
uv.lock                          ← Pinned dependency versions + hashes
uv.lock.sha256                   ← SHA256 of uv.lock
migrate_v3_to_v4.py              ← V3 → V4 config migration script
config/config.toml.example       ← Template for fresh installs
config/models.toml.example       ← Default model download definitions (repo IDs, filenames)
systemd/                         ← Unit files installed by install.sh
logrotate.d/forge-audit        ← logrotate config
docs/bios-setup.md               ← BIOS checklist for Strix Halo (linked from install output)
```

`config/models.toml.example` contains the default model definitions referenced by the installer:
```toml
[[models]]
name        = "Gemma 4 E2B"
pattern     = "gemma4-e2b-*Q4_K_M*.gguf"
hf_repo     = "<repo-id>"
hf_file     = "gemma4-e2b-abliterated-Q4_K_M.gguf"
size_gb     = 5.4
modes       = ["gemma4-e2b"]
required    = true

[[models]]
name        = "Qwen3.6"
pattern     = "Qwen3.6-35B-*Q6_K*.gguf"
hf_repo     = "<repo-id>"
hf_file     = "Qwen3.6-35B-A3B-UD-Q6_K_XL.gguf"
size_gb     = 24.1
modes       = ["qwen36", "coding"]
required    = true
```

Repo IDs are intentionally left as `<repo-id>` placeholders in the committed file — fill them in before distributing, or the operator is prompted to provide them during install if blank.
```

---

## 16. Open Work

Ordered by implementation phase:

| Priority | Item | Notes |
|---|---|---|
| 🔴 0 | **Install script (`install.sh`)** | Platform checks (Fedora 44+, kernel 7.0+, Strix Halo CPU/GPU, ROCm, Vulkan, both llama.cpp builds); BIOS observable checks (version+date display, total memory, memory configured speed, GTT pool, Secure Boot, BIOS checklist reminder); model inventory + download prompts; ComfyUI reachability; strict mode age+OSV dual-check; `uv sync --frozen --require-hashes`; V3→V4 migration; 15-step feedback with progress bars |
| 🔴 0a | **`docs/bios-setup.md`** | Strix Halo BIOS checklist: EXPO/XMP, GART aperture, UMA frame buffer, Secure Boot MOK, BIOS update procedure; linked from install output and help panel |
| 🔴 1 | **Auth wiring** | `auth.py` complete; wire Flask-Login into `app.py`, `/login` template, CSRF middleware — gate for all protected endpoints |
| 🔴 2 | **`secrets.toml` split + migration script** | Extract `hf_token` and `[[users]]` from `config.toml`; write `migrate_v3_to_v4.py` (backup → extract → validate → replace); update `config_writer.py` routing |
| 🔴 3 | **`validators.py`** | Pydantic models on all routes; Pillow + defusedxml + CSS sanitization for file uploads; path traversal guards |
| 🔴 4 | **gunicorn + gevent** | Replace Werkzeug dev server; `gunicorn -k gevent -w 1`; update `forge-dashboard.service`; `/api/v1/health` readiness probe |
| 🟠 5 | **TOML filelock** | `filelock.FileLock` in `config_writer.py`; wraps all reads + writes; eliminates concurrent-edit race |
| 🟠 5a | **`tailscale.py` + Settings → Tailscale tab** | Discovery via `tailscale status --json`; service verification; `tailscale serve` command execution; first-run wizard step; dashboard status bar badges; `usermod -aG tailscale forge` in deploy |
| 🟠 6 | **API versioning** | Move all routes to `/api/v1/`; 301 redirects from `/api/` |
| 🟠 7 | **Settings → Services tab** | UI + `/api/v1/services` PUT endpoint; validate before write |
| 🟠 8 | **Settings → Storage tab + `storage.py`** | systemd `.mount` unit generation; mount/umount via `systemctl`; auto-mount as secondary post-validation action |
| 🟠 9 | **Notes page (`notes.py`)** | Section CRUD (markdown, links, nfs_gallery); preconfigured with PairNode gallery; thumbnail cache + daily prune timer |
| 🟡 10 | **Per-mode runtime history (`history.py`)** | Three-way context tracking (trained/configured/actual); ring buffer 10 loads/mode; dashboard health indicators; GTT retry with checkpoints action |
| 🟡 11 | **Trained context detection (`read_gguf_metadata`)** | Add to `engine.py` via `gguf` package; annotates `get_local_models()`; drives mode builder context panel |
| 🟡 12 | **First-run wizard (`/setup`)** | Detects missing/empty `secrets.toml`; creates admin + HF token; generates minimal `config.toml` if absent |
| 🟡 13 | **SVG CSS injection fix** | Strip `<style>` blocks (or allowlist via `cssutils`) in addition to `<script>` + event handlers; or rasterize all SVGs to PNG at upload |
| 🟢 14 | **Audit log (`audit.py`) + logrotate + viewer** | JSON-lines writer; `logrotate.d` config; Settings → Audit Log tab (admin-only); paginated + filterable; `/api/v1/audit` endpoint |
| 🟢 15 | **Security headers + `X-Forge-Version`** | Single `after_request` hook; `unsafe-inline` for styles in V4 initial; nonce-based CSP as Phase 2 target |
| 🟢 16 | **API key lifecycle** | Users & API Keys tab; optional expiry (default 90d); rotation via `/api/v1/apikeys/<name>/rotate`; expired key `401` with `WWW-Authenticate` header; `last_used` tracking |
| 🟢 17 | **SSE event namespacing** | Rename all events to `namespace:action` format; emit both old and new names during transition window |
| 🟢 18 | **Mode builder presets** | Six templates (dense, MoE, vision, ROCm large, dual-slot, unloaded); field-level `?` tooltips on extra_args, backend, context, mmproj |
| 🟢 19 | **NFS form field tooltips** | Inline `?` tooltip on every NFS add-mount field; "ComfyUI output" preset fills optimised defaults |
| 🟢 20 | **Weekly dep scan (`forge-dep-scan.timer`)** | pip-audit + OSV scanner; results surfaced as `system:warning` SSE; systemd weekly timer |
| ✅ 21 | **Color themes (CSS custom properties)** | Shipped: `forge/themes.py`. Four named themes (`aurora` default, `fire`, `graphite`, `spring`); 24-property `--var` palette per theme; live-apply from topbar hamburger menu, no page reload. See §7.8. |
| 🔴 22 | **Help panel** | `/help` route (no auth); designed in §7.9 but not yet implemented — no route exists in `app.py`; renders `quick-reference`/`modes`/`infrastructure`/`pitfalls`/`adding-a-model`/`bios-setup` docs; context-sensitive popovers; `?` keyboard shortcut |
| — | **Disk cleanup on ForgeHost** | `Qwen3.6-35B-A3B-UD-Q6_K_XL-nomtp.gguf` (30 GB), `/opt/forge/llama-mtp/`, old binary `.bak` |
| — | **Qwen3.6 context reduction** | GTT contiguous alloc fails above 262K; may improve with kernel GTT defrag or reboot-after-load |
| ✅ **FIM autocomplete service** | Deployed on PairNode as `pairnode-ghost.service` (CUDA llama-server :8090, Qwen2.5-Coder-7B base Q4_K_M). Not on ForgeHost — avoids GTT contention. |
| — | **Nemotron/Gemma4 MTP** | Needs upstream llama.cpp support |

---

## 17. Forge Node Agent — Design Reference

Shipped 2026-06-16. Code: `forge-agent/agent.py`. First deployment: PairNode.

### Purpose

A generic, config-driven control-plane agent for homelab machines that are not the Forge's
primary inference server. Enables Ops to discover, monitor, and control on-demand AI services
on remote nodes without writing node-specific code. Any machine can run it; Ops builds its
controls dynamically from the manifest it reports.

### Architecture

```
Tailnet (ACL-gated)
  ↓ tailscale serve → svc:ops-pairnode → localhost:9200
Forge Node Agent (Flask, bound 127.0.0.1:9200)
  ├── GET  /v1/health          (unauthenticated)
  ├── GET  /v1/manifest        (paired callers only — full system description)
  ├── GET  /v1/status          (paired callers only — live GPU + service state)
  ├── POST /v1/services/<id>/start|stop  (paired callers only — unit control)
  ├── POST /v1/pair/initiate   (pairing code → mutual token exchange)
  └── POST /v1/pair/revoke     (paired callers — unpair)
Config: /etc/forge-agent/agent.toml
State:  /var/lib/forge-agent/
Audit:  /opt/forge-agent/audit.log (JSON-lines)
```

### Manifest schema (`GET /v1/manifest`)

```json
{
  "agent_version": "1.0.0",
  "node": {
    "hostname": "pairnode",
    "tailscale_name": "pairnode.example.ts.net",
    "tailscale_ips": ["100.x.x.x", "fd7a::…"],
    "os": "Ubuntu 24.04",
    "arch": "x86_64"
  },
  "gpu": [{"name": "RTX 5080", "vram_total_mb": 16303, "vram_used_mb": 2113}],
  "services": [
    {"id": "comfyui", "label": "ComfyUI", "kind": "image", "port": 7860, "active": false},
    {"id": "tts",     "label": "TTS (Qwen3-TTS)", "kind": "tts", "port": 8082, "active": false},
    {"id": "ghost",   "label": "Ghost-text", "kind": "fim", "port": 8090, "active": false}
  ],
  "models": [
    {"name": "Qwen2.5-Coder-7B", "kind": "fim", "size_gb": 4.5, "service": "ghost", "path": "…"}
  ]
}
```

### Pairing protocol

1. Operator runs `forge-agent show-pairing` on the remote machine — prints a single-use,
   10-minute code (8 chars, e.g. `7F3K9QXM`).
2. In Ops → Services → + Add node: enter the agent address + code.
3. Ops sends `POST /v1/pair/initiate` with `{code, controller_token}` to the agent.
4. Agent validates the code (single-use, expiry-checked), stores the controller's Tailscale
   identity + token, and replies with `{agent_token, node_id, agent_identity}`.
5. Ops stores the node in `secrets.toml` (`[[nodes]]`: id, label, address, token,
   agent_identity, paired_at).
6. On every subsequent call: bearer token (compare_digest) + Tailscale identity check.

**Revocation:** `forge-agent unpair` or Ops "Remove node" calls `POST /v1/pair/revoke` on the
agent, then removes the local `[[nodes]]` entry.

### Security layers (all must pass for control/manifest calls)

1. **Tailnet ACL** — only tailscale peers with an explicit ACL grant can reach `svc:ops-pairnode`.
2. **Bearer token** — `secrets.compare_digest(stored_token, presented_token)` on every call.
3. **Tailscale identity** — `tailscale whois` (or `Tailscale-User-Login` header) of caller
   must match the stored `agent_identity` from pairing.
4. **Unit allowlist** — agent will only `systemctl [--user] start|stop` units declared in its
   `agent.toml`. No arbitrary commands, no shell=True, no path input from callers.
5. **Unprivileged** — runs as user testuser (no sudo); controls only user-scope systemd units.
6. **No secrets in responses** — generic 401/403/404; rate-limit control calls; audit log.

### Ops integration (`forge/app.py`)

- `_node_agent(node_id, method, path, timeout, **kwargs)` — mirrors `_tts_proxy`; reads the
  node's token from `secrets.toml`, attaches `Authorization: Bearer`, handles timeouts and
  unreachable errors gracefully (502/504).
- `GET /api/v1/nodes` — list paired nodes (tokens stripped).
- `POST /api/v1/nodes/pair` + `DELETE /api/v1/nodes/<id>` — pair / unpair.
- `GET /api/v1/nodes/<id>/status` — poll agent `/v1/status`; caches `last_manifest_services`
  in the node entry for TTS failover routing.
- `GET /api/v1/nodes/<id>/manifest` — fetch full manifest; caches in node entry.
- `POST /api/v1/nodes/<id>/services/<svc>/start|stop` — relay to agent.

### Dashboard Nodes panel (`templates/index.html`)

- Merged into the Services panel (2026-07-01) — node cards render directly under the single
  "Services" heading, right after the services-panel grid and above the mode grid. No separate
  "Nodes" section label.
- **Per-node card:** reachability dot + node name + hostname, GPU VRAM bar, per-service pill
  toggles (built from manifest — no hardcoded service names). Visually distinct from the
  services-panel's `svc-item` pills — kept as richer cards, not flattened into the grid.
- **Add node** button opens a pairing dialog (address + code + optional label).
- Polls `GET /api/v1/nodes/<id>/status` every 5 s (client-side; never blocks `_build_status()`).

### TTS routing/failover (`_tts_route()`)

`_tts_route(prefer_faster_node: bool)` in `app.py`:
- If `prefer_faster_node=True`: iterate paired nodes, find one with `kind=tts` + `active=True`
  in `last_manifest_services`, do a short HEAD probe → return that node's address on success.
- Falls back to `_TTS_BASE` (`http://127.0.0.1:8082` — ForgeHost) on any failure or if
  `prefer_faster_node=False`.
- Used by `POST /api/v1/tts/speak` (Generate Speech tab) and the `--target auto` flag in
  `tts-batch.py`.

---

## 18. a0 Router — Design Reference

Built 2026-07-02. Full detail in `docs/llm-router.md`; this section is the pointer from the
main spec so the component isn't undiscoverable from here.

### Purpose

A thin OpenAI-compatible routing/failover proxy on ForgeHost (port 8085, `svc:a0`) sitting between
external AI agents (Hermes) and every model backend — Forge's own dynamic slots (a1/a2) and
remote OpenAI-compatible providers (DeepSeek, later GLM/Gemini). It owns 100% of the
routing/failover decision so a calling agent never reconciles multiple providers' credentials
itself. Built to work around two real bugs in an external dependency (`hermes-agent`): the
Forge mode-switch dead window (connection-refused during slot-load switches) and a
client-side credential-pool bug that could silently swap in the wrong provider's `base_url`
while keeping the old `model` name attached.

### Design invariant

A request matches a **route** (keyed by logical model name) → an ordered list of **backend**
names → each backend record bundles `(how to reach it, wire model name, credential)` as one
atomic unit. There is no code path that resolves a backend's `base_url`/credential
independently of its `wire_model` — this is the structural fix for the credential-pool bug.

### Files

| File | Role |
|---|---|
| `forge/router_app.py` | Flask app + gunicorn entrypoint (`router_app:app`). Routes: `POST /v1/chat/completions`, `GET /v1/models`, `GET /healthz`. Bearer `sk-router-*` auth, no sessions/CSRF. |
| `forge/router.py` | Routing/failover engine — route lookup, health/busy gating, atomic backend resolution, bounded retry, streaming passthrough. |
| `forge/router_catalog.py` | Live `/health`+`/props`+`/metrics` probing (TTL-cached) for Forge-local slots; static entries for remote providers. |
| `forge/gunicorn-router.conf.py` | Single gevent worker, same pattern as the dashboard's `gunicorn.conf.py`. |
| `systemd/forge-router.service` | Clone of `forge-dashboard.service`'s pattern. |

Config lives under `[router]` / `[[router.backends]]` / `[[router.routes]]` in `config.toml`;
credentials under `[[router_providers]]` (outbound, plaintext) and `[[router_keys]]` (inbound,
Argon2id-hashed) in `secrets.toml` — see §9. Full schema, request-flow state machine, and the
busy-slot `wait`/`fail_fast` toggle are documented in `docs/llm-router.md`.

**Status:** built and verified standalone (curl only). **Not wired to Hermes yet** — that
cutover is a separate, deliberate future step (swap Hermes' `model.base_url`/`model.api_key`
to point at `a0` and drop its own `fallback_providers`), not implied by this build existing.
