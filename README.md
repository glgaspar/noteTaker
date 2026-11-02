# NoteTaker

A self-hosted note-taking web app inspired by AFFiNE and Evernote: workspaces, nested folders,
tags, a rich editor with code highlighting, real-time co-editing, sharing, full-text search and
imports from Evernote, Markdown, HTML, text and zip archives (Obsidian, Notion).

Stack: Go, htmx, Tailwind, PostgreSQL, TipTap + Yjs in the browser, and optionally S3-compatible
storage (RustFS) for images and attachments.

## Development

Requirements: Go 1.26, Node 24, Docker.

```sh
make db                                                  # Postgres and RustFS in Docker
go run ./cmd/notetaker create-admin you@example.com "Your Name"   # prompts for a password
make dev                                                 # builds assets, serves http://localhost:8080
```

Users only join by invitation: as admin, open **Users** and send an invite. Without SMTP the invite
link is shown on the page (and logged) so you can pass it on.

While working on the frontend, `npm run watch:js` and `npm run watch:css` rebuild assets on change;
the Go binary embeds them, so restart `make dev` to pick them up.

## Running with Docker

```sh
docker compose --profile app up -d --build
docker compose --profile app run --rm app create-admin you@example.com "Your Name"
```

Put a TLS-terminating reverse proxy in front, set `BASE_URL` to the public `https://` address and
`TRUST_PROXY=true`. Change the RustFS and Postgres credentials in `docker-compose.yml` before
exposing anything.

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | `postgres://notetaker:notetaker@localhost:5432/notetaker?sslmode=disable` | Postgres connection |
| `ADDR` | `:8080` | Listen address |
| `BASE_URL` | `http://localhost:8080` | Public URL, used in emailed links; `https://` makes cookies secure |
| `TRUST_PROXY` | | `true` to take the client address from `X-Forwarded-For` (only behind a proxy that sets it) |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM` | port `587` | Email for invites, password resets and share notifications; unset = links shown/logged instead |
| `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, `S3_SSL` | bucket `notetaker` | Attachment storage; unset = images and files are disabled |
| `TZ` | | Server time zone, used until a browser reports its own |

## Commands

- `notetaker serve` – run the server (the default).
- `notetaker create-admin <email> <name>` – create an admin; reads the password from the terminal or stdin.
- `notetaker sweep-files` – delete stored files whose notes are gone. `serve` also does this every 6 hours.

Migrations in `migrations/` run automatically on startup.

## Tests

```sh
make test   # Go integration tests; recreate the notetaker_test database, need `make db`
make e2e    # browser checks against a running server (E2E_URL, E2E_ADMIN_EMAIL/PASSWORD, CHROME)
```

## Backups

Back up the database first, then the files: a file uploaded in between is merely extra (the sweep
removes it), while a file deleted in between would be missing from the restore.

```sh
docker compose exec -T db pg_dump -U notetaker -Fc notetaker > notetaker.dump
docker run --rm -v notetaker_files:/data -v "$PWD":/backup alpine tar czf /backup/files.tgz -C /data .
```

Restore with `pg_restore -U notetaker -d notetaker --clean notetaker.dump` and by extracting the
archive into the `files` volume (or mirror the bucket with any S3 tool).

## How it works

Code layout: `cmd/notetaker` (entry point and commands), `internal/app` (HTTP handlers, pages,
permissions), `internal/collab` (live editing), `internal/importer` (document parsing),
`internal/storage` (S3 client), `internal/db` (migrations). `assets.go` embeds `templates/`,
`static/` and `migrations/`; frontend sources are in `web/`.

- **Pages** are server-rendered Go templates; htmx boosts navigation and swaps fragments.
- **Permissions** come from the `effective_role` SQL function: a user's role in a folder is the
  strongest of their workspace role and any share on the folder or its ancestors.
- **Co-editing**: each note is a Yjs document. The server keeps an ordered log of Yjs updates per
  note and relays them over a WebSocket (`internal/collab` documents the protocol); it never parses
  them. Clients compact the log into snapshots when asked. Rooms live in memory, so run a single
  server process.
- **Search** uses a generated `tsvector` with an accent-insensitive configuration and prefix matching.
- **Imports** are converted to HTML on the server and run as background jobs; the first editor to
  open an imported note turns the HTML into its Yjs document.
