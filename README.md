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
| `LLM_API_URL`, `LLM_API_KEY`, `LLM_MODEL` | | Default model that turns task inbox items into tasks: any OpenAI-compatible chat API, e.g. Open WebUI `http://host:3024/api` or Ollama `http://host:11434/v1`; unset = titles are guessed, unless a user sets their own |
| `SECRET_KEY` | | Encrypts credentials users save (mailbox passwords, their own model's API key); unset = they can't add mailboxes or keys. Changing it makes saved ones unreadable |
| `MS_CLIENT_ID`, `MS_TENANT` | tenant `common` | Microsoft Entra app for Microsoft 365 / Outlook.com mailboxes (see below); unset = those can't be added |
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

## Export

Notes export as Markdown (with front matter and an `attachments/` folder, which imports back
into NoteTaker or opens in Obsidian) or as HTML, from the note, folder, workspace and user menus.
Single notes download right away; larger exports run in the background (two at a time, one per
user) and stay downloadable for 24 hours. Their zips are written to the system temp directory.
Exports only contain notes the person can see. "Print or save as PDF" in a note's menu prints
just the note.

## Tasks

Each user has a private to-do list at `/tasks` (in the workspace menu): tasks with subtasks,
tags and due dates; a Today list picked from them; and a Completed history. A task can only be
checked off in Today, and at the user's midnight checked tasks move to Completed while unchecked
ones go back to the main list (nothing runs for this: it follows from the date each task was
moved to Today).

Tasks can also come from **sources**, set up per user under Inbox → Sources: webhooks
(`POST /hooks/<secret>` with JSON or text), IMAP mailboxes and JSON APIs polled on a schedule,
any number of each. What they send waits in the Inbox to be accepted or discarded. A language
model (`LLM_*` settings, or each user's own) turns each item into a title, details, due date,
tags and subtasks; without one, or when it fails, the title is guessed. Mailbox passwords and
API headers are stored encrypted with `SECRET_KEY`. Mailboxes and APIs only import what is new
after they were added, unless asked otherwise, and mailboxes are opened read-only.

### Microsoft 365 and Outlook.com mailboxes

Microsoft refuses passwords over IMAP, so these mailboxes sign in with Microsoft instead (a code
entered at microsoft.com/devicelogin). Register the app once in the Microsoft Entra admin center:

1. App registrations → New registration. For work and personal accounts choose "any
   organizational directory and personal Microsoft accounts" (then `MS_TENANT=common`); for one
   organisation only, set `MS_TENANT` to its tenant ID. No redirect URI is needed.
2. Authentication → Advanced settings → Allow public client flows: Yes.
3. API permissions → Add → APIs my organization uses → Office 365 Exchange Online → Delegated →
   `IMAP.AccessAsUser.All`; also Microsoft Graph → Delegated → `offline_access`, `openid`, `email`.
   If users may not consent to apps themselves, an admin clicks "Grant admin consent".
4. Put the Application (client) ID in `MS_CLIENT_ID`. There is no client secret.

IMAP must also be enabled for the mailbox (Exchange admin center → the mailbox → Email apps).

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
