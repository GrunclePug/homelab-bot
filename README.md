# homelab-bot

A Discord bot for a self-hosted media server: access requests, Seerr issue reports with one-click fixes, and a download queue for members.

## Overview

homelab-bot relays Seerr issue reports into a Discord channel, with buttons that let an admin replace a bad file in one click: the release behind the file is blocklisted, the file deleted and a new one searched for in Sonarr or Radarr. New members ask for access from a panel in the server, and an approver onboards them with one click: a Plex invite, the media role and a welcome message. Members can also check what qBittorrent is downloading with `/queue`. It is for people who run Seerr, Sonarr, Radarr and qBittorrent for a small Discord community, and it ships as a single static binary, usually run with Docker Compose.

## Features

* **Access requests**: members press a button and give their Plex account; approvers approve or deny it in a private channel.
* **Onboarding**: approving sends the Plex invite for every library, gives the media role and DMs the member how to get started.
* **Issue relay**: each Seerr issue becomes one post in the support channel, kept up to date as it is commented on, resolved and reopened.
* **Replace**: an admin-only button that blocklists the current release, deletes the file and starts a search, after a confirmation that lists exactly what will change.
* **Episode picker**: for an issue about a whole season or show, the admin picks the episodes or seasons to replace.
* **Resolve**: an admin-only button that resolves the issue in Seerr.
* **`/queue`**: shows members what is downloading, stalled, queued or stopped, visible only to them.
* **Health check**: `GET /healthz`, plus a `-healthcheck` flag for container health checks.
* **Pure Go**: no CGO, one static binary, with SQLite through `modernc.org/sqlite`.
* **Outbound only**: connects to Discord over the gateway; the only listener is the webhook for Seerr on your own network.

## Installation

### Requirements

* Go 1.27.3 or newer
* Make
* Docker with Compose, to run it as a container
* A Discord bot application, and Seerr, Sonarr v4, Radarr and qBittorrent 5.2 or newer reachable from the bot

### Build

```sh
git clone https://github.com/grunclepug/homelab-bot.git
cd homelab-bot
make build
sudo make install    # optional, installs to /usr/local/bin
```

## Usage

Copy `.env.example` to `.env`, fill it in, and run the bot:

```sh
cp .env.example .env
make run
```

It registers `/queue` and `/access-panel` in your server, connects to Discord and listens for Seerr on `LISTEN_ADDR`. Point Seerr's webhook at it as described in [Seerr webhook](#seerr-webhook).

```sh
homelab-bot -version        # print the version
homelab-bot -healthcheck    # exit 0 if the running bot's /healthz is healthy
```

### Configuration

Settings come from environment variables, loaded from `.env` if it exists. Variables already set in the environment win. The bot checks them at startup and stops with a message naming anything missing or invalid.

| Variable | Flag | Default | Description |
|---|---|---|---|
| `DISCORD_TOKEN` | | | Bot token |
| `DISCORD_GUILD_ID` | | | The server the bot serves |
| `DISCORD_SUPPORT_CHANNEL_ID` | | | Channel for issue posts |
| `DISCORD_ADMIN_ROLE_ID` | | | Role that may press Replace and Resolve |
| `DISCORD_MEDIA_ROLE_ID` | | | Role that may use `/queue` (the admin role may too), given to approved members |
| `DISCORD_ACCESS_CHANNEL_ID` | | | Private channel where access requests are posted for approvers |
| `DISCORD_APPROVER_ROLE_ID` | | | Role that may approve and deny access requests |
| `SEERR_URL` | | | Seerr base URL |
| `SEERR_API_KEY` | | | Seerr API key |
| `SEERR_WEBHOOK_SECRET` | | | Value Seerr sends in the `Authorization` header |
| `SEERR_PUBLIC_URL` | | | Optional. The Seerr address members use, shown in the welcome DM |
| `PLEX_TOKEN` | | | The Plex server owner's token, for sending invites |
| `PLEX_SERVER_ID` | | | The Plex server's machine identifier |
| `SONARR_URL` | | | Sonarr base URL |
| `SONARR_API_KEY` | | | Sonarr API key |
| `RADARR_URL` | | | Radarr base URL |
| `RADARR_API_KEY` | | | Radarr API key |
| `QBIT_URL` | | | qBittorrent Web UI base URL |
| `QBIT_API_KEY` | | | qBittorrent Web UI API key |
| `LISTEN_ADDR` | | `:8080` | Address for the Seerr webhook and `/healthz` |
| `PORT` | | `8080` | Docker Compose only: host port mapped to the listener |
| `DB_PATH` | | `data/homelab-bot.db` | SQLite database file |
| `LOG_LEVEL` | | `info` | `debug`, `info`, `warn` or `error` |
| | `-version` | | Print the version and exit |
| | `-healthcheck` | | Request `/healthz` on the local listener (port from `LISTEN_ADDR`, read from the environment or `.env`) and exit 0 or 1 |

The Discord bot needs the `bot` and `applications.commands` scopes. In the support and access channels, and wherever `/access-panel` is run, it needs View Channel, Send Messages, Embed Links and Read Message History, and in the server Manage Roles, with its own role above the media role so it can hand that out. It uses only the Guilds gateway intent.

### Docker

```sh
cp .env.example .env    # then fill it in
mkdir data && sudo chown 65532:65532 data
docker compose up -d --build
```

The container runs as the distroless non-root user (UID 65532), so `data/` must be writable by that UID. The database lives in `data/homelab-bot.db`. Compose maps host port `${PORT}` (default `8080`) to the listener, and its health check runs `/homelab-bot -healthcheck`. On `docker compose down` or a rebuild, the bot gets up to 2m30s to finish Seerr events and clicks in progress, such as a Replace; clicks that arrive meanwhile are asked to try again in a minute.

## Access requests

Run `/access-panel` in the channel where new members should find it, for example a welcome channel, and pin the message it posts. The command is visible only to members with Manage Server, and only the admin role may use it.

A member presses **Request access** and fills in their Plex email or username, plus an optional note. They then see what they typed, visible only to them, and press **Send request** or **Edit**; an email address that doesn't look like one has to be fixed first. This catches typos, since an invite to a mistyped address goes to whoever owns it. The request is posted to `DISCORD_ACCESS_CHANNEL_ID` with **Approve** and **Deny** buttons, which only the approver role may press. An email address is shown masked, for example `d•••@e•••.com`, and is kept in full only until the request is decided; a Plex username is shown as typed. Keep the channel private all the same.

Approving a request:

1. Invites the Plex account to every library on the server. If the invite fails, nothing else happens: the request is closed, the post shows the error, and the member is asked by DM to check their Plex account and request again.
2. Gives the member the media role.
3. DMs the member how to accept the invite and sign in to Seerr.

A failed role or DM is noted on the request post, to be done by hand. Denying a request DMs the member a short note, and they may ask again later. A member can have one open request at a time. If the bot stops while handling a click, at startup it finishes an approval whose Plex invite already went out, and reopens any other request for the approver to press again.

Seerr needs no setup from the bot if its Plex sign-in for new users is enabled: once members accept the invite, they sign in to Seerr with Plex. Libraries added to Plex later aren't shared with existing members automatically.

To find the settings, take `PLEX_TOKEN` from the owner account (Plex's support article "Finding an authentication token") and `PLEX_SERVER_ID` from `machineIdentifier` at `http://<plex-host>:32400/identity`. The invite goes through plex.tv's sharing API, which Plex doesn't document and may change.

## Seerr webhook

In Seerr, under Settings, Notifications, Webhook:

* **Webhook URL**: `http://<bot-host>:<port>/seerr`
* **Authorization Header**: the value of `SEERR_WEBHOOK_SECRET`
* **Notification types**: Issue Reported, Issue Commented, Issue Resolved and Issue Reopened only
* **JSON Payload**:

```json
{
  "event": "{{event}}",
  "notification_type": "{{notification_type}}",
  "subject": "{{subject}}",
  "message": "{{message}}",
  "image": "{{image}}",
  "issue_id": "{{issue_id}}",
  "issue_type": "{{issue_type}}",
  "issue_status": "{{issue_status}}",
  "reported_by": "{{reportedBy_username}}",
  "commented_by": "{{commentedBy_username}}",
  "comment": "{{comment_message}}",
  "media_type": "{{media_type}}",
  "tmdb_id": "{{media_tmdbid}}",
  "tvdb_id": "{{media_tvdbid}}"
}
```

Seerr's Test button posts "Seerr test notification received." to the support channel. For each issue the bot also fetches the issue from Seerr's API to learn which season and episode it is about.

## Replace

Pressing Replace on an issue post works out the file from the issue:

* **Movie**: the Radarr movie with the issue's TMDB ID, and its file.
* **Episode**: the Sonarr series with the issue's TVDB ID, and that episode's file.
* **Season or whole show**: a menu, visible only to the admin, of the season's episodes or the show's seasons, plus "All of them".

An episode stored in the same file as another, such as S01E01E02, is replaced together with it, since deleting the file removes both. The admin then sees what will be replaced (quality, release group and size) and confirms or cancels. Nothing changes until it is confirmed. For each file the bot then:

1. Blocklists the release by marking its grab failed. This is the grab whose download produced the current file, or the most recent grab if none matches. Without any grab history, for example after a manual import, this step is skipped and the post says so.
2. Deletes the file.
3. Searches for a new one, in a single search for every file that was deleted.

A failed blocklist doesn't stop the delete and search; a failed delete means that file isn't searched for. The issue post gets a "Replacement started by" line with the result of each step, and the bot comments on the Seerr issue so the reporter sees it there too. The comment is posted as the Seerr API key's user. The issue stays open until an admin resolves it.

## Development

```sh
make run      # run locally as version dev
make test     # run the tests
make fmt      # format the code
make lint     # go vet and staticcheck
make build    # build bin/homelab-bot
make clean    # remove build output
```

## Contributing

Fork the repository and open a pull request.

## License

MIT. See [LICENSE](LICENSE).

## Author

Chad Humphries | [Website](https://grunclepug.com/) | [GitHub](https://github.com/GrunclePug) | [More projects](https://github.com/GrunclePug?tab=repositories)
