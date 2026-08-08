# Mattermost Mention Notifier

A server-side Mattermost plugin that watches configured teams and/or channels, extracts `@username` mentions from new posts, looks up each username in a CSV mapping, and sends an external HTTP `POST` for every matched user.

Mattermost calls what this project describes as a "group" a **team**. The plugin therefore exposes `team_ids` and `channel_ids` filters.

## How it works

1. Mattermost calls `MessageHasBeenPosted` for a new post.
2. The plugin checks the configured team and channel filters.
3. User mentions are extracted from the post text.
4. `@all`, `@channel`, and `@here` are ignored because they are broadcast mentions, not individual users.
5. Each username is normalized to lowercase and looked up in `data.csv`.
6. Duplicate mentions in the same post are collapsed.
7. Each matched mapping is sent to the configured HTTP endpoint in a background goroutine, so the Mattermost hook is not blocked by the external service.
8. A non-2xx response or transport failure is logged.

## Project layout

```text
.
├── data.csv
├── go.mod
├── Makefile
├── plugin.go
├── plugin_test.go
├── plugin.json
├── .env.example
├── .gitignore
└── README.md
```

## CSV format

The CSV has two columns:

```csv
mention,number
alice,0901234567
bob,0907654321
```

The header is optional. Usernames may be written with or without `@`:

```csv
mention,number
@alice,0901234567
bob,0907654321
```

The value in the second column is passed to the external endpoint **verbatim**. The plugin does not validate it as a phone number, because the external service may use another identifier format.

Do not commit real phone numbers or other sensitive identifiers. The repository `.gitignore` excludes `data.csv`; keep a private copy in the deployment bundle.

## External request

For a post containing:

```text
Please call @alice and @bob.
```

with:

```csv
mention,number
alice,0901234567
bob,0907654321
```

the plugin sends one POST per matched user.

Example JSON body:

```json
{
  "name": "alice",
  "number": "0901234567",
  "post_id": "POST_ID",
  "user_id": "AUTHOR_USER_ID",
  "team_id": "TEAM_ID",
  "channel_id": "CHANNEL_ID",
  "message": "Please call @alice and @bob."
}
```

The request headers include `Content-Type: application/json`, `Accept: application/json`, a plugin User-Agent, and an optional `Authorization: Bearer <token>` header.

## Configuration

Configure the plugin in **System Console → Plugins → Mention Notifier**.

| Setting | Meaning |
|---|---|
| `enabled` | Turns processing on/off. |
| `team_ids` | Comma-separated Mattermost team IDs. Empty means every team. |
| `channel_ids` | Comma-separated Mattermost channel IDs. Empty means every channel. |
| `notify_url` | HTTP/HTTPS endpoint receiving notification POSTs. Required when enabled. |
| `csv_file` | CSV path. Defaults to `data.csv`. Relative paths are resolved beside the plugin executable. |
| `timeout_seconds` | HTTP request timeout. Defaults to 10 seconds. |
| `auth_token` | Optional bearer token for the external endpoint. |

### Filtering behavior

Both filters are optional:

- Empty `team_ids` + empty `channel_ids`: monitor every post.
- Team IDs only: monitor all channels in those teams.
- Channel IDs only: monitor those channels regardless of team.
- Both configured: a post must match **both** a configured team and a configured channel.

The plugin matches IDs, not human-readable team/channel names.

## Environment variables

`.env.example` documents optional deployment defaults:

```text
MENTION_NOTIFIER_NOTIFY_URL
MENTION_NOTIFIER_CSV_FILE
MENTION_NOTIFIER_TIMEOUT_SECONDS
MENTION_NOTIFIER_AUTH_TOKEN
```

These are fallbacks when the corresponding System Console setting is empty. Mattermost does not automatically load a `.env` file; `.env.example` is documentation for your deployment environment. Do not put real credentials in `.env.example`.

For production, prefer Mattermost plugin configuration for operational settings and your deployment system's secret/environment mechanism for the token.

## Building

### Requirements

- Go compatible with the `go.mod` version (Go 1.24 for this revision).
- A Mattermost server that supports the plugin version in `plugin.json`.
- Network access to download Go dependencies on the first build.

Run:

```bash
make fmt
make test
make vet
make build
make package
```

`make package` creates `rocks.sherwin.mention-notifier-0.2.0.tar.gz` containing `plugin`, `plugin.json`, and `data.csv`.

### Cross-compiling

The plugin executable is platform-specific. For a Linux amd64 Mattermost server:

```bash
GOOS=linux GOARCH=amd64 make build
```

For other deployment targets, set `GOOS` and `GOARCH` as appropriate.

## Installing

Mattermost plugin bundles are `.tar.gz` archives containing the manifest and plugin executable. Install the generated archive from:

**System Console → Plugins → Plugin Management**

Then enable the plugin and configure it.

For production, make sure the bundled `data.csv` contains the mapping expected by the plugin.

## Development checks

Use:

```bash
make check
```

This runs formatting, `go test ./...`, and `go vet ./...`.

The included tests cover mention extraction, ignoring broadcast mentions, email-address false positives, team/channel filtering, URL validation, System Console string-list parsing, JSON notification delivery, and bearer authentication.

## Review notes and fixes made

The original implementation had several concrete problems that are corrected here:

1. **Makefile/package mismatch:** the original target packaged `mentions.csv`, but the supplied file is `data.csv`.
2. **No team/group filter:** `team_ids` support was added. In Mattermost terminology, a group-like scope is represented here by a team.
3. **Configuration reloaded on every post:** configuration is now cached and refreshed through `OnConfigurationChange`.
4. **Configuration changes did not refresh runtime state:** CSV and HTTP client settings are refreshed when configuration changes.
5. **Endpoint URL validation was too permissive:** only absolute HTTP/HTTPS URLs are accepted.
6. **HTTP timeout was missing:** external requests now have a configurable timeout, defaulting to 10 seconds.
7. **Optional authentication was missing:** an optional bearer token is supported.
8. **Shutdown handling was missing:** notification goroutines are tracked and waited for on deactivation.
9. **Broadcast mentions were treated as users:** `@all`, `@channel`, and `@here` are ignored.
10. **CSV parsing was rigid:** the `mention,number` header is recognized, but headerless two-column files are also accepted.
11. **Sensitive runtime data could be committed:** `.gitignore` excludes `data.csv`, `.env`, and build artifacts.
12. **No automated tests existed:** `plugin_test.go` was added for core parsing, filtering, URL validation, and HTTP behavior.
13. **No System Console schema existed:** `plugin.json` now exposes configuration settings in the plugin UI.
14. **The number was unnecessarily added to the URL query:** it is now sent in the JSON body, avoiding needless exposure in URL logs/proxies.

## Important deployment/security considerations

- Treat `data.csv` as sensitive if it contains phone numbers or other personal identifiers.
- Use HTTPS for the external endpoint.
- Protect `auth_token` as a secret.
- Restrict the endpoint to the minimum network access needed.
- The plugin sends the original Mattermost message text in the JSON payload. If the external service does not need it, remove the `message` field from `Notification` before production use.
- Each matching user causes a separate HTTP request. A post mentioning five mapped users produces five requests.
- Unknown mentions are skipped and logged at debug level.
- Duplicate mentions in one post generate only one request per username.
- The plugin does not retry failed requests. Add a queue/retry mechanism if the notification service requires guaranteed delivery.

## Current sample data

The supplied `data.csv` contains:

```csv
mention,number
alice,0901234567
bob,https://notify.example/bob
all,https://notify.example/all
```

The plugin treats the second column as an opaque identifier, so the `bob` and `all` values are technically accepted even though they are URLs rather than phone numbers. `@all` in a Mattermost post is ignored, so that mapping is not triggered by the broadcast mention.

For a phone-number-only deployment, replace the sample values with actual identifiers and keep the file private.

## Troubleshooting

### Plugin fails to activate

Check Mattermost plugin logs. Common causes include a missing `data.csv`, a CSV with no usable mappings, an enabled plugin with no valid `notify_url`, or an invalid CSV path.

### Messages are ignored

Check:

1. The plugin is enabled.
2. The post is in a configured team/channel.
3. The message contains a supported `@username`.
4. The username exists in the CSV after lowercase normalization.
5. The external endpoint is reachable.

### Endpoint receives no request

Check Mattermost logs for `invalid notification URL`, `notification request failed`, or `notification returned non-2xx`. Also verify that the endpoint accepts `POST application/json`.

## Official Mattermost references

- Plugin overview: https://developers.mattermost.com/integrate/plugins/overview/
- Server plugins: https://developers.mattermost.com/integrate/plugins/components/server/
- Manifest reference: https://developers.mattermost.com/integrate/plugins/manifest-reference/
- Server SDK reference: https://developers.mattermost.com/integrate/reference/server/server-reference/
- Plugin best practices: https://developers.mattermost.com/integrate/plugins/best-practices/

## License

Add the license appropriate for your project before publishing or distributing the plugin.
