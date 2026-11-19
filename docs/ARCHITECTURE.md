# Architecture

Mino is a modular monolith: one Go binary, one PostgreSQL database, no background workers,
caches or message brokers. Business logic lives in services; the REST API and the admin
panel are thin HTTP layers over the same services.

```
                    ┌────────────────────────── cmd/mino ───────────────────────────┐
  HTTP ──► server ──┤  /api/{site}/…  ──► api      ┐                               │
  (chi, logging,    │  /admin/…       ──► admin    ├──► content (engine) ──► schema │
   security hdrs)   │  /uploads/…     ──► local fs │    media   apitokens  users     │
                    │  /healthz                    ┘    sites   settings   auth      │
                    └───────────────────────────────────────── db (pgx pool, migrations)
```

## Packages

| Package | Responsibility |
|---|---|
| `config` | Environment variables → `Config` |
| `db` | Connection pool, embedded versioned migrations (advisory-locked), error helpers |
| `sites` | Websites (projects): CRUD, stats, cascading delete |
| `schema` | Content type & field model, definition validation, value coercion, change diff, change impact planning, persistence and history |
| `content` | The content engine: entry validation, CRUD, relations, media links, query parsing (filters/sort/pagination/fields/populate), SQL generation, serialization |
| `media` | Upload validation, metadata, library queries; `Storage` interface with `Local` and `S3` (SigV4) backends |
| `sanitize` | Allowlist HTML sanitizer for rich text |
| `auth` | Argon2id passwords, DB-backed sessions, in-memory rate limiter |
| `users` | Admin users, roles, per-site memberships, permission checks (`Access`) |
| `apitokens` | Per-site API tokens, public permissions, API caller (`Principal`) |
| `settings` | Installation-wide settings (kept in memory after first read) |
| `api` | REST handlers: HTTP ↔ engine, token/public permission checks, CORS |
| `admin` | Server-rendered admin panel: handlers, templates, static assets (embedded) |
| `server` | Router assembly, logging, panic recovery, security headers, proxy IP, uploads |

Dependencies point downward only: `api`/`admin` → services → `schema`/`db`. `schema` and
`sanitize` have no HTTP or database knowledge in their core types.

## Data model

```
sites ─┬─< content_types ─< entries ─┬─< relations >── entries
       │        │                    └─< media_links >── media
       ├─< media                                           │
       ├─< api_tokens                       sites ─────────┘
       ├─< public_permissions
       └─< site_members >── admin_users ─< sessions
                 │
               roles ─< role_permissions
content_types ─< content_type_history
settings, schema_migrations
```

- **Schemas** are rows in `content_types` with the field list in `fields` (JSONB).
- **Entries** keep scalar values in `entries.data` (JSONB). No table is created per content
  type, so schema changes never run `ALTER TABLE`.
- **Relations** are stored once, from the owning side, in `relations (source, field, target,
  position)`. Cardinality is enforced by the engine: single-valued fields accept one id;
  `oneToOne`/`oneToMany` targets are exclusive (linking elsewhere moves them).
- **Media links** live in `media_links`; deleting a file or an entry cascades.
- **Unique fields** are enforced by partial unique indexes,
  `CREATE UNIQUE INDEX … ON entries ((data->>'slug')) WHERE content_type_id = N`, created and
  dropped with the schema. Violations surface as field errors.
- **Draft & publish** uses `published_at` (NULL = draft). Required fields are checked when an
  entry is published, so incomplete drafts can be saved.

### Indexes

`entries` has B-tree indexes on `(content_type_id, created_at)`, `(content_type_id,
updated_at)`, `(content_type_id, published_at)` and a GIN `jsonb_path_ops` index on `data`.
Equality filters are generated as JSONB containment (`data @> '{"field": value}'`) so they
use the GIN index. Relation lookups use `relations (target_id, field)` and the primary keys.

## Content engine

One request path for both the API and the admin:

1. **Resolve** the content type within the site (`schema.Service.GetByRoute` / `Get`).
2. **Validate input** (`content.prepare`): unknown fields rejected; each scalar coerced by
   `schema.CoerceValue` (types, lengths, ranges, regex, enums, dates normalized to UTC, rich
   text sanitized); link ids parsed; defaults applied on create.
3. **Write** in a transaction: insert/update the row, replace links for supplied link fields
   after checking every target exists in the same site (and media type is allowed).
4. **Read** with `List`/`Get`: SQL is built from a validated `Query`. Field names reach SQL
   only after matching the schema and the `^[a-zA-Z][a-zA-Z0-9_]*$` pattern; all values are
   bind parameters.
5. **Load links in batches**: one query for relation ids, one for media (joined), one per
   page for populated targets — constant query count per page regardless of size (tested).
6. **Serialize**: private fields removed; unpopulated links omitted; timestamps in UTC.

The admin parses form values into the same input map the API receives, and uses
`content.ParseQuery` for its list filters, so validation rules are identical.

## Schema changes

`schema.Service.Update` diffs the old and new definitions and assesses each change against
stored data:

| Level | Examples | Behavior |
|---|---|---|
| none | add field, drop unique, loosen rules | applied |
| warning | make required with empty values, tighten rules | needs confirmation |
| destructive | remove a field with data, remove enum values in use, retarget a relation with links | needs confirmation; data purged in the same transaction |
| blocked | incompatible type change with data, unique with duplicates, collection→single with >1 entry | refused |

Every applied change increments `version` and stores the new definition in
`content_type_history`.

## Security model

- **Admin sessions:** random 256-bit token in an `HttpOnly`, `SameSite=Lax` cookie (Secure on
  HTTPS); only its SHA-256 is stored. Idle and absolute expiry; revoked on logout, password
  change, deactivation. Expired sessions are pruned at login.
- **CSRF:** per-session token required on every admin mutation (form field or
  `X-CSRF-Token` header for HTMX/fetch), plus an `Origin`/`Sec-Fetch-Site` check that also
  covers the login form.
- **Passwords:** Argon2id (12 MiB, t=3). Unknown emails take the same time as wrong passwords.
- **Login throttling:** per IP and per email, fixed window, in memory.
- **Authorization:** every admin handler checks the user's `Access` for the site (super
  admin, or role permissions; "own only" compares `created_by`). Non-members get 404 for a
  site. API calls check the token's (or public) permission per action and content type;
  tokens are bound to one site; drafts require a token; populating a relation needs read
  permission on the target type; private fields are never returned, filterable or sortable.
- **Content Security Policy** on admin pages: scripts only from the same origin (Alpine needs
  `'unsafe-eval'`), no framing, no plugins.
- **Rich text** is sanitized server-side against an allowlist; unsafe URL schemes removed.
- **Uploads:** size limit, type detected from content, script-capable formats (SVG, HTML,
  XML, JS) rejected, random storage keys; local files are served with a sandboxing CSP,
  `nosniff`, and as attachments unless they are images, audio, video or PDF.
- **API tokens:** `mino_` + 256 random bits, shown once, stored as SHA-256.

## Frontend

Pages are Go `html/template`s rendered on the server. HTMX handles list filtering,
pagination, uploads, pickers and modals; Alpine.js holds local state where needed (the
Content-Type Builder editor, relation/media pickers, toasts, confirmations). The TipTap
editor is bundled with esbuild and loaded only on entry forms. Tailwind CSS v4 with DaisyUI
v5 provides the styles; fonts (Schibsted Grotesk, IBM Plex Sans/Mono) are self-hosted. All
assets are built by `make assets` into `internal/admin/static` and embedded in the binary.

Each site has a hue derived from its slug, shown as a rail along the sidebar and on site
tiles, so editors working across many sites always see which one they are in.

## Testing

`make test` runs unit tests and PostgreSQL integration tests. Each test gets a fresh,
migrated PostgreSQL schema (`internal/testutil`). Coverage includes schema validation and
change planning, value coercion, the HTML sanitizer, CRUD, relations (all kinds), media
links, filters/sort/pagination, query counts, site isolation, sessions, passwords, rate
limiting, roles and permissions, API tokens, every REST endpoint, admin HTTP flows (setup,
login, CSRF/Origin, permissions), migrations, and the S3 signer (AWS test vector; plus an
integration test against a real bucket when `MINO_TEST_S3_*` is set).
