# Mino

A lightweight, self-hosted headless CMS. One installation manages many websites from one
admin panel and one PostgreSQL database. Each website gets its own content types, entries,
media library, API tokens and REST API.

It covers the parts of Strapi most projects use — Content-Type Builder, Content Manager,
Media Library, roles and permissions, draft & publish, an automatic REST API — in a single
15 MB Go binary that runs in about 30 MB of memory.

- **Multi-site:** `/api/blog/articles`, `/api/shop/products`, … from one install.
- **Content-Type Builder:** 12 field types, relations, validation rules; schema changes that
  would delete data are shown first and must be confirmed; impossible ones are blocked.
- **Content Manager:** forms generated from the schema, rich text, relation and media pickers,
  search, filters, sorting, drafts, publishing, duplication, API preview.
- **REST API:** Strapi-style filters, sorting, pagination, field selection, populate.
- **Media:** S3-compatible storage (AWS S3, Cloudflare R2, MinIO, …) or local disk.
- **Access control:** super admins, per-site roles (Site Admin, Editor, Author, custom),
  "own entries only" rules, per-site API tokens, opt-in public read access.

Built with Go, chi, pgx, PostgreSQL, server-rendered templates, HTMX, Alpine.js, Tailwind
CSS/DaisyUI and TipTap. No Node.js at runtime.

## Quick start (Docker)

```bash
cp .env.example .env          # set POSTGRES_PASSWORD
docker compose up -d
open http://localhost:1337/admin
```

The first visit shows a setup page: create the super admin account and your first site.
**Complete setup right after deploying** — until an account exists, anyone who can reach the
server can claim it.

Then:

1. **Content-Type Builder** → create a type (e.g. *Article*) and add fields.
2. **Content Manager** → create and publish entries.
3. **Site settings → Public API access** → allow `find` on *Article*, or create an
   **API token** for your frontend.
4. `curl http://localhost:1337/api/<site>/articles`

Add more websites under **Installation → Sites**; give people access under **Users**.

## Running without Docker

Requirements: Go 1.26+, PostgreSQL 14+ (Debian/Ubuntu builds recommended: their collation
sorts text the way editors expect).

```bash
createdb mino
MINO_DATABASE_URL=postgres://localhost/mino make run
```

Migrations run automatically at startup.

## Configuration

All settings are environment variables.

| Variable | Default | |
|---|---|---|
| `MINO_DATABASE_URL` | — (required) | PostgreSQL connection URL |
| `MINO_ADDR` | `:1337` | Listen address |
| `MINO_PUBLIC_URL` | — | Public base URL, e.g. `https://cms.example.com`. `https://` turns on secure cookies. Also used for absolute media URLs with local storage. |
| `MINO_TRUST_PROXY` | `false` | Set `true` behind a reverse proxy (Traefik, Caddy, nginx) so rate limiting and logs see client IPs. Mino trusts the right-most `X-Forwarded-For` entry, i.e. exactly one proxy hop. |
| `MINO_SECURE_COOKIES` | from `MINO_PUBLIC_URL` | Force the `Secure` cookie flag on/off |
| `MINO_SESSION_TTL` | `12h` | Admin session idle timeout |
| `MINO_SESSION_MAX_AGE` | `168h` | Admin session absolute lifetime |
| `MINO_CORS_ORIGINS` | `*` | Comma-separated origins allowed to call `/api` from browsers |
| `MINO_UPLOAD_MAX_MB` | `25` | Maximum size per uploaded file |
| `MINO_UPLOAD_DIR` | `./uploads` | Local media directory (when S3 is not configured) |
| `MINO_S3_BUCKET` | — | Enables S3-compatible storage |
| `MINO_S3_ENDPOINT` | — | e.g. `https://s3.eu-west-1.amazonaws.com`, `https://<account>.r2.cloudflarestorage.com` |
| `MINO_S3_REGION` | `us-east-1` | `auto` for Cloudflare R2 |
| `MINO_S3_ACCESS_KEY`, `MINO_S3_SECRET_KEY` | — | Credentials |
| `MINO_S3_PUBLIC_URL` | — | Base URL clients load files from (public bucket, CDN, `r2.dev` domain) |
| `MINO_S3_PATH_STYLE` | `true` | `endpoint/bucket/key`; set `false` for `bucket.endpoint/key` |
| `MINO_DB_MAX_CONNS` | `5` | Connection pool size |
| `MINO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `MINO_LOG_FORMAT` | `pretty` in a terminal, else `text` | `pretty` (readable, colored), `text` (key=value) or `json`. `NO_COLOR=1` disables colors. |
| `MINO_DEV` | `false` | Reload templates and static files from disk |
| `GOMEMLIMIT` | `128MiB` | Go soft memory limit |

### Media on S3

Create a bucket, make it readable by the public (or put a CDN in front of it), and set the
`MINO_S3_*` variables. Objects are stored as `<site id>/<random>.<ext>` and uploaded with
immutable cache headers. Mino signs requests itself (AWS Signature V4) and needs only
`PutObject`, `DeleteObject` and `ListBucket` permissions.

### Behind a reverse proxy

Terminate TLS at the proxy and forward to port 1337 with the original `Host` header. Set
`MINO_PUBLIC_URL=https://…` and `MINO_TRUST_PROXY=true`. Mino logs a warning if it sees
`X-Forwarded-For` while `MINO_TRUST_PROXY` is off.

## Sites, roles and permissions

- A **site** is a project boundary. Content types, entries, media, API tokens and public
  permissions belong to exactly one site; relations and media links cannot cross sites.
- **Super Admins** see every site and manage sites, users, roles and general settings.
- Other users get **one role per site**. Sites they are not assigned to are invisible.
- **Roles** are shared templates. Built-in: *Site Admin* (everything on the site including
  content types and API tokens), *Editor* (all content and media), *Author* (creates content,
  edits and deletes only their own entries, cannot publish). Custom roles can grant content
  actions for all types or specific types, with "own only" restrictions.

## REST API

See [docs/API.md](docs/API.md). Examples:

```bash
# Public (after enabling find on Article)
curl 'https://cms.example.com/api/blog/articles?sort=publishedAt:desc&pagination[pageSize]=10'

# With a token, including drafts and relations
curl -H "Authorization: Bearer $TOKEN" \
  'https://cms.example.com/api/blog/articles?status=draft&populate=author,cover'

# Create and publish
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"data":{"title":"Hello","author":3}}' \
  'https://cms.example.com/api/blog/articles?status=published'
```

## Resource use

Measured with the Docker Compose stack (Mino + `postgres:17` with the bundled settings):
10 sites, 30 content types, 2,000 articles with relations, 16 concurrent clients sending a mix
of filtered/populated API lists, single reads, public reads and admin page renders for 60 s.

| | Result |
|---|---|
| Throughput | 2,233 requests/s, 0 errors |
| Latency | p50 4.0 ms · p95 8.5 ms · p99 11.8 ms |
| Mino memory | 29 MB idle · ~31 MB under load |
| PostgreSQL memory | ~30 MB idle · ~49 MB under load |
| Docker image | 28 MB |
| Startup | migrations + listening in ~10 ms after the database is reachable |

Your numbers depend on content size and traffic; measure with your own data.

## Development

```bash
make db            # PostgreSQL for development/tests on 127.0.0.1:5440
make test          # all tests (unit + PostgreSQL integration), with the race detector
make dev           # run with templates/static read from disk
make assets        # rebuild admin CSS/JS/fonts (needs Node; output is committed)
make docker        # build the image
```

Code map and design notes: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Backups

Back up the PostgreSQL database (`pg_dump`) and the media: the `uploads` volume with local
storage, or the bucket with S3. Every content type change also keeps the previous definition
in `content_type_history`.

## Out of scope

Plugins, GraphQL, internationalization, webhooks, SSO, workflows, real-time collaboration.
