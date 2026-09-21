# zot-usage-limites

A Go extension for zot that reports provider usage limits through `/limits`.

The first provider is ChatGPT/Codex subscription usage. Provider requests and response mappings live in JSON so additional providers can be added without changing the report renderer.

## Status

Codex support is feature-flagged and uses the unofficial ChatGPT backend usage endpoint. The endpoint can change without notice.

## Build and install

```sh
go build -o zot-usage-limites .
zot ext install .
```

The extension reads zot's existing `$ZOT_HOME/auth.json`. Install only extensions you trust: this extension needs to read the OpenAI OAuth token in that file to query the Codex endpoint. It never prints or logs the token.

## Enable

Create `$ZOT_HOME/zot-usage-limites.json`:

```json
{
  "enabled": true,
  "cache_ttl_seconds": 60,
  "providers": {
    "openai-codex": {
      "enabled": true,
      "definition": "openai-codex.json"
    }
  }
}
```

Then restart zot and run:

```text
/limits
```

A missing configuration file, or `enabled: false`, leaves the command available but disabled.

## Provider definitions

Definitions are loaded from the extension's `providers/` directory. A definition declares the provider identity, auth mode, request, and JSON Pointer paths for usage windows:

```json
{
  "id": "example",
  "display_name": "Example Provider",
  "auth": { "provider": "openai", "mode": "oauth" },
  "request": {
    "method": "GET",
    "url": "https://example.test/usage",
    "headers": { "x-account-id": "$auth.account_id" }
  },
  "windows": [
    {
      "name": "Daily",
      "used_percent": "/limits/daily/used_percent",
      "reset_at": "/limits/daily/reset_at"
    }
  ]
}
```

Supported substitutions are `$auth.access_token` and `$auth.account_id`. Provider files must not contain credentials.

The normalized report model supports provider name, plan, percentage usage, reset times, and multiple windows. Providers that need custom signing, non-JSON responses, or a different credential source will need a future adapter capability.

## Authentication behavior

The extension parses the current zot OpenAI credential shape:

- `openai.oauth.access_token`
- `openai.oauth.account_id`
- `openai.oauth.expiry`
- `openai.api_key` for future API-key definitions

It does not refresh OAuth tokens. If the token is missing or expired, run `zot login`.

## Security and privacy

- Requests use HTTPS in the bundled definition.
- Authorization is added by the extension from `auth.json`.
- Tokens are not included in errors, output, or logs.
- Cross-host redirects are rejected.
- Provider definitions are executable only as data; they do not run shell commands.

## Test

```sh
go test ./...
```

Tests should use local HTTP fixtures and synthetic credentials. Do not make paid or credentialed provider requests from tests.
