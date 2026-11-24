# ChatGPT subscription proxy

A small local Go proxy inspired by [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It forwards OpenAI-style Responses and image generation requests to the Codex backend using a ChatGPT subscription, without an OpenAI API key or fallback to the paid API.

## Account commands

Requires Go 1.24+ to build and the Codex CLI on PATH to sign in. From PowerShell:

```powershell
go build -o chatgpt-proxy.exe .
.\chatgpt-proxy.exe login
.\chatgpt-proxy.exe usage
.\chatgpt-proxy.exe logout
```

- **`login`** opens ChatGPT sign-in. Run it again to add another account or renew an existing login. It does not touch your normal Codex login.
- **`logout`** prompts you to pick a saved account and removes its local proxy login. It does not sign you out of ChatGPT elsewhere.
- **`usage`** shows each account’s current Codex usage windows, reset times when available, and any additional limits the backend reports. If an account’s session expires, run `login` again.

Credentials are stored under `~/.chatgpt-proxy/accounts` in your user profile. Keep this directory private; on Windows it inherits access permissions from your profile.

## Start the proxy

```powershell
$env:CHATGPT_PROXY_KEY = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N')
.\chatgpt-proxy.exe serve
```

Configure your Responses client with base URL `http://127.0.0.1:8787/v1` and the generated `CHATGPT_PROXY_KEY` as its API key. The proxy uses the accounts saved at startup. Use `serve -account <id>` to pin one account (IDs are shown by `usage`), or `serve -port 8788` for another port. Restart after adding a new account to the pool; renewing or removing an existing login takes effect without a restart.

To try the endpoint directly from a shell that has the same key:

```powershell
$body = @{ model = '<model available in your Codex account>'; input = 'Say hello.' } | ConvertTo-Json
Invoke-RestMethod http://127.0.0.1:8787/v1/responses `
  -Method Post -Headers @{ Authorization = "Bearer $env:CHATGPT_PROXY_KEY" } `
  -ContentType 'application/json' -Body $body
```

Set `stream: true` for server-sent events instead of one JSON response.

## Routing and cache metrics

```powershell
.\chatgpt-proxy.exe serve -routing cache-aware -cache-log
```

| `-routing` | Account choice for new or unidentified sessions |
| --- | --- |
| `round-robin` (default) | Cycle through available accounts independently for each model. |
| `fill-first` | Choose the first available account. |
| `cache-aware` | Prefer the best recent cached-input-token ratio for the model after a warm-up, with occasional round-robin sampling. |

Use `-order id1,id2` to put accounts first in the order used by fill-first, round-robin, and cache-score ties. Reuse an `X-Session-ID` header or a `prompt_cache_key` in the request body for session affinity within a model. Bindings expire after an hour of inactivity, or sooner if the in-memory table fills. The proxy retries on another account only for HTTP 401 or 429 before a response starts; it never replays a started stream or an ambiguous network failure. An HTTP 429 pauses that account for the affected model, honoring reset times when Codex supplies them. A failed fallback does not replace an established binding.

`-cache-log` reports per-response cached vs. total input tokens and the rolling ratio per account and model when upstream supplies usage. The proxy does not move warm sessions just because their cache ratio falls. Cache observations and session bindings reset on restart.

## Generate an image

`POST /v1/images/generations` forwards JSON to the Codex image endpoint. It defaults to `gpt-image-2`; set `model` to another `gpt-image-*` model supported by your plan if needed.

```powershell
$body = @{ prompt = 'A blue square on a white background' } | ConvertTo-Json
$image = Invoke-RestMethod http://127.0.0.1:8787/v1/images/generations `
  -Method Post -Headers @{ Authorization = "Bearer $env:CHATGPT_PROXY_KEY" } `
  -ContentType 'application/json' -Body $body
[IO.File]::WriteAllBytes((Join-Path $PWD 'image.png'), [Convert]::FromBase64String($image.data[0].b64_json))
```

The example saves the default PNG response. The returned image format and availability depend on the Codex backend and account. Partial-image streaming is not supported.

### Edit an image

`POST /v1/images/edits` supports a prompt, one PNG/JPEG/WebP upload (`image` or `image[]`), and an optional `mask`. The total request limit is 8 MiB. For example:

```powershell
curl.exe -sS --fail-with-body http://127.0.0.1:8787/v1/images/edits `
  -H "Authorization: Bearer $env:CHATGPT_PROXY_KEY" `
  -F 'prompt=Replace the background with pale blue' -F 'image=@image.png'
```

The result uses the same JSON format as generation. Clients can also send JSON with `images: [{"image_url":"..."}]` (a URL or data URL) or `images: [{"file_id":"..."}]`, plus an optional `mask` with the same reference shape. Codex determines whether a file reference or mask is usable.

## Discover models

`GET /v1/models` returns an authenticated OpenAI-style list of visible models reported by the saved accounts. Successful account catalogs are cached for five minutes; if some accounts fail, the endpoint returns the models it could load. For new bindings, fresh catalogs make Responses routing prefer available accounts that list the requested model. Established bindings take precedence, and other accounts remain eligible as a fallback. Unsupported or unlisted models are not globally blocked, and image model availability is checked by Codex itself.

## Scope

- This uses Codex, not the ChatGPT web chat interface. Model availability and rate limits come from the selected account. The backend is not the public OpenAI API and may change.
- The proxy is stateless: send the full conversation in `input`. Stored responses, `previous_response_id`, conversation IDs, and background requests are not supported. Optional fields remain subject to Codex backend support.
- The proxy drops unsupported sampling, context-management, truncation, prompt-cache options/retention, identity, and stream-options fields. It always requests `reasoning.encrypted_content` and retains supported optional `include` values.
- No Chat Completions, WebSockets, or management UI. Use a client that supports Responses. Do not expose the listener to other machines.
