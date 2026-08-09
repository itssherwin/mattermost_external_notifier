# Mattermost External Notifier

Mattermost server plugin that watches configured teams/channels, extracts `@username` and `@group-name` mentions, looks up each recipient's number, and delivers one notification per matched user — either as a generic HTTP webhook or directly via the **KavehNegar SMS API**.

## Requirements

- Mattermost Server 10.0 or newer
- Go 1.24 or newer for building
- A CSV mapping Mattermost usernames to notification numbers
- Either an HTTP/HTTPS endpoint that accepts the generic notification payload, **or** a [KavehNegar](https://kavenegar.com) account and API key

## How it works

1. Mattermost calls `MessageHasBeenPosted` when a post is created.
2. The plugin resolves the post's channel with `GetChannel(post.ChannelId)`.
3. The channel provides the `TeamId`; `model.Post` itself does not contain `TeamId`.
4. The configured team and channel filters are evaluated.
5. `@username` / `@group-name` mentions are extracted from the message.
6. `@all`, `@channel`, and `@here` are ignored.
7. Each mention token is checked against real Mattermost Groups (`GetGroupByName`). If it matches a group, **every member of that group** is resolved via `GetGroupMemberUsers` and treated as a recipient. If it doesn't match a group, the token is treated as a plain username.
8. Duplicate recipients in the same post — whether from a direct mention, a group expansion, or both — are notified only once.
9. Each recipient username is looked up in `data.csv`.
10. A matching number is notified asynchronously, via the configured provider (`generic` or `kavenegar`).
11. For the generic provider, a 2xx HTTP response is required for success. For KavehNegar, both the HTTP status **and** the API's own `return.status` field must indicate success — KavehNegar returns HTTP 200 even for some API-level failures (insufficient credit, invalid sender, etc.).

## Configuration

Configure these values in the Mattermost System Console plugin settings.

### Enabled

Enable or disable the plugin.

### Team IDs

Comma-separated Mattermost team IDs. Empty means all teams.

### Channel IDs

Comma-separated Mattermost channel IDs. Empty means all channels.

If both Team IDs and Channel IDs are configured, a post must match both filters.

### Provider

Selects how notifications are delivered:

| Value | Delivery |
|---|---|
| `generic` (default) | JSON `POST` to **Notification endpoint** |
| `kavenegar` | SMS via the **KavehNegar** REST API |

#### `generic` provider settings

**Notification endpoint** — example:

```
https://notifications.example.com/mattermost
```

Only `http` and `https` URLs are accepted.

**Bearer token** — optional. When configured, the request contains:

```
Authorization: Bearer YOUR_TOKEN
```

#### `kavenegar` provider settings

**KavehNegar API key** — required when Provider is `kavenegar`. Get this from your [KavehNegar panel](https://panel.kavenegar.com/client/setting/account). Can also be supplied via the `KAVENEGAR_API_KEY` environment variable (see [Environment variables](#environment-variables)), which is the recommended way to set it in production.

**KavehNegar API base URL** — optional. Defaults to `https://api.kavenegar.com/v1`. Can also be supplied via `KAVENEGAR_API_URL`.

**KavehNegar sender line** — optional. The sender line/number configured on your KavehNegar account. Leave blank to use your account default.

**SMS message template** — optional. Supports `{{user}}` and `{{message}}` placeholders, e.g.:

```
You were mentioned by @{{user}}: {{message}}
```

If left blank, this same template is used as the default.

### CSV path

Default:

```
data.csv
```

Relative paths are resolved relative to the plugin executable.

### HTTP timeout

Default: `10`. Allowed range: `1`–`120` seconds. Applies to both providers.

### Expand @group mentions to members

Default: enabled. When on, mentioning a real Mattermost Group (e.g. `@on-call`) notifies **every member of that group**, resolved via the Mattermost Groups API — not just a literal username lookup. See [Group mention behavior](#group-mention-behavior) for details and caveats.

### Debug logging

Default: disabled. When enabled, the plugin appends verbose diagnostics — tagged messages, extracted mentions, resolved usernames, their numbers, and provider response codes — to a log file. See [Debug logging](#debug-logging) below; **read the security note there before enabling this in production.**

### Debug log path

Optional. Defaults to `mention-notifier-debug.log` beside the plugin executable if left blank. Only used when Debug logging is enabled.

## Environment variables

`KAVENEGAR_API_KEY` and `KAVENEGAR_API_URL`, if set, **override** the corresponding System Console settings, so the real API key never has to live in Mattermost's `config.json`.

They can be set two ways:

1. **Real environment variables**, exported in the environment the Mattermost *server process* runs in (the plugin binary inherits this — not your local shell). For example, via systemd `EnvironmentFile=`, a Docker Compose `environment:` block, or your process manager's config.
2. **A `.env` file** placed beside the installed plugin executable (same directory rule as `CSVPath`). See `.env.example` for the format. The plugin reads this file once on activation via a small built-in parser (no external dependency) and only fills in variables that aren't already set in the real environment — an explicit environment variable always wins over the file.

```
# .env
KAVENEGAR_API_KEY=your-kavenegar-api-key
KAVENEGAR_API_URL=https://api.kavenegar.com/v1
```

In production, the installed plugin binary typically lives under your Mattermost data directory at:

```
plugins/<plugin-id>/server/dist/<plugin-id>-<os>-<arch>
```

Place the `.env` file in that same directory, or set the environment variables on the Mattermost server process itself.

## CSV format

The recommended format is:

```
username,number
alice,0901234567
bob,0909876543
charlie,0912345678
```

A leading `@` is also accepted:

```
username,number
@alice,0901234567
@bob,0909876543
```

The username lookup is case-insensitive. A header is optional; the loader recognizes common headers such as:

```
username,number
user,phone
name,phone_number
```

Malformed rows and rows without both values are skipped.

For the `kavenegar` provider, numbers are lightly normalized before sending (spaces and dashes stripped, a leading `+` removed) so `0912xxxxxxx`, `+98912xxxxxxx`, and `98912xxxxxxx` all work as CSV entries.

## Notification payload

### `generic` provider

For a post containing:

```
Please check this @alice
```

and a CSV mapping:

```
username,number
alice,0901234567
```

the endpoint receives approximately:

```json
{
  "name": "alice",
  "number": "0901234567",
  "post_id": "post-id",
  "user_id": "author-id",
  "team_id": "team-id",
  "channel_id": "channel-id",
  "message": "Please check this @alice"
}
```

Request method: `POST`, with `Content-Type: application/json`.

### `kavenegar` provider

A `GET` request is sent to:

```
{KAVENEGAR_API_URL or default}/{API-KEY}/sms/send.json?receptor=0901234567&message=...&sender=...
```

`sender` is omitted from the query string if not configured. The message body defaults to `You were mentioned by @alice: Please check this @alice` unless a custom **SMS message template** is configured.

## Mention behavior

Supported examples:

```
@alice
hello @alice
hello @Alice
(@alice), please review this
```

Ignored:

```
@all
@channel
@here
alice@example.com
```

Repeated mentions are deduplicated:

```
@alice please help @alice
```

produces one notification for `alice`.

A trailing `.` or `-` is trimmed from a mention when it's the very last character (e.g. `cc @charlie.` at the end of a sentence resolves to `charlie`, not `charlie.`), since these are valid mid-username characters but almost always sentence punctuation when trailing.

## Group mention behavior

When **Expand @group mentions to members** is enabled, a mention token is first checked against Mattermost's Groups API (`GetGroupByName`):

- **Matches a group** → every member is fetched via `GetGroupMemberUsers` (paginated) and each one with a CSV entry is notified. The literal group name itself is *not* looked up as a username.
- **Group lookup errors after a match** (e.g. member listing fails) → the plugin does **not** fall back to treating the group name as a literal username, since that's very unlikely to be a real recipient. The error is logged instead and no one is notified for that mention.
- **No matching group** → the token is treated as a plain `@username` mention, exactly as before.

Two things worth knowing:

- Group membership expansion is **not** scoped to the channel where the mention happened — every group member is notified regardless of whether they're actually in that channel. This matches how Mattermost's own `@group` notifications behave.
- Expansion costs one extra API call (`GetGroupByName`) per unique mention token per post, even for ordinary `@username` mentions. This is negligible at normal message volumes.

## Debug logging

When **Debug logging** is enabled, the plugin appends lines like this to the debug log file:

```
2026-08-09T12:03:11Z TAGGED post_id=abc123 channel_id=xyz team_id=t1 mentions=[alice bob] message="cc @alice @bob please check"
2026-08-09T12:03:11Z GROUP token=oncall members=[alice dave]
2026-08-09T12:03:11Z EXTRACTED user=alice from_token=alice number=0912xxxxxxx
2026-08-09T12:03:11Z EXTRACTED user=bob from_token=bob number=<no csv mapping>
2026-08-09T12:03:11Z KAVENEGAR RESPONSE user=alice number=0912xxxxxxx http_status=200 api_status=200 api_message="" decode_error=<nil>
2026-08-09T12:03:11Z GENERIC RESPONSE user=alice number=0912xxxxxxx http_status=200
```

The log file:

- Opens (or reopens, if the path changed) whenever the plugin's configuration reloads — no restart needed to turn logging on/off.
- Is append-only with **no built-in rotation**. If you leave this on for any length of time in production, put the file behind `logrotate` or similar, or it will grow indefinitely.
- Closes cleanly when the plugin deactivates.

### ⚠️ Security note

This log intentionally contains **full notification numbers and full post message text in plain text**, which is a deliberate departure from the plugin's normal behavior of never logging numbers on success (see [Security](#security)). Only enable Debug logging when actively troubleshooting, and:

- Keep the debug log path **out of version control**, same as `data.csv`.
- Exclude it from log-shipping / centralized logging pipelines unless that destination is as trusted as your CSV data.
- Turn it back off once you're done debugging.

## Build

Install Go 1.24 or newer and run:

```
go mod tidy
```

Then:

```
make check
```

Build:

```
make build
```

Package:

```
make package
```

The package is written to:

```
dist/rocks.sherwin.mention-notifier-v0.2.0.tar.gz
```

## Network problems during `go mod tidy`

If `proxy.golang.org` is unavailable in your environment, try:

```
GOPROXY=direct go mod tidy
```

Do not manually create `go.sum`. It should be generated by Go after the dependency is successfully downloaded.

## Installation

Build the package:

```
make package
```

Install the generated plugin archive through the Mattermost System Console or your normal Mattermost plugin deployment process.

After installation:

1. Upload/enable the plugin.
2. Choose a **Provider** (`generic` or `kavenegar`) and fill in its settings.
3. If using `kavenegar`, either set the **KavehNegar API key** in the System Console, or set `KAVENEGAR_API_KEY` (and optionally `KAVENEGAR_API_URL`) via a real environment variable or a `.env` file beside the plugin executable.
4. Configure team/channel filters.
5. Make sure `data.csv` is available beside the plugin executable, or configure an absolute CSV path.
6. Optionally enable **Expand @group mentions to members** if you want `@group-name` mentions to notify every group member.
7. Enable the plugin.
8. Post a message containing a configured username or group mention.

## Security

- Use HTTPS for the generic notification endpoint.
- Do not commit real API keys, bearer tokens, or the `.env` file with real values — only commit `.env.example`.
- Do not commit production phone/notification numbers unless your repository is appropriately protected.
- Keep `data.csv` out of version control if it contains sensitive data.
- Restrict the external endpoint (generic provider) so it accepts requests only from trusted infrastructure when possible.
- The plugin logs failures but does not log the notification number on successful requests — **except** when Debug logging is explicitly enabled, which intentionally logs full numbers and message content; see [Debug logging](#debug-logging).
- Prefer environment variables / `.env` over the System Console for the KavehNegar API key, since System Console values are stored in `config.json`.

## Troubleshooting

### Missing `go.sum`

Run:

```
go mod tidy
```

If the dependency download times out:

```
GOPROXY=direct go mod tidy
```

### `post.TeamId undefined`

Do not add `TeamId` to `model.Post`. The plugin deliberately resolves the team like this:

```go
channel, appErr := p.API.GetChannel(post.ChannelId)
teamID := channel.TeamId
```

### No notification is sent

Check:

- Plugin is enabled.
- Team ID matches.
- Channel ID matches.
- Mentioned username (or group member) exists in the CSV.
- CSV path is correct.
- For `generic`: notification endpoint is reachable and returns a 2xx status.
- For `kavenegar`: the API key is set (System Console or `KAVENEGAR_API_KEY`), and the account has SMS credit.
- Enable **Debug logging** temporarily and check the debug log file for `EXTRACTED` / `KAVENEGAR RESPONSE` / `GENERIC RESPONSE` lines to see exactly where the pipeline stopped.
- Mattermost plugin logs for errors (`LogError` entries from this plugin).

### `@group-name` mention isn't notifying members

- Confirm **Expand @group mentions to members** is enabled.
- Confirm the group's Mattermost mention name (not its display name) matches the token used in the message.
- Check the debug log for a `GROUP token=... members=[...]` line — an empty `members` list means the group exists but has no members, or none of its members have a CSV entry.

## Development commands

```
make fmt
make test
make vet
make check
make build
make package
make clean
```

## Project files

```
.
├── .env.example
├── .gitignore
├── Makefile
├── README.md
├── data.csv
├── go.mod
├── plugin.go
├── plugin.json
└── plugin_test.go
```