# Mino — Implementation Plan

A lightweight, self-hosted headless CMS inspired by Strapi. Go + chi + PostgreSQL (pgx),
server-rendered admin (Go templates + HTMX + Alpine.js + Tailwind/DaisyUI + TipTap).

**One installation serves many websites.** Each site is a separate project with its own
content types, entries, media, API tokens and public permissions. One admin panel (with a
site switcher) and one PostgreSQL database. API: `/api/{site}/{plural}`.

Phases build strictly on the previous ones. The admin UI (phase 6) is a client of the same
content engine the REST API uses, so business logic exists once.

Review checkpoints: after phases **2**, **5** and **6**.

---

## Phase 0 — Foundations

- [x] Folder structure (`cmd/`, `internal/<module>/`, `docs/`)
- [x] Environment configuration (`MINO_*` variables)
- [x] PostgreSQL pool (small, idle connections released)
- [x] Embedded, versioned migrations with advisory lock, one transaction per migration
- [x] Core tables: roles, role_permissions, admin_users, sessions, content_types,
      content_type_history, entries, relations, media, media_links, api_tokens,
      public_permissions, settings
- [x] Test helper: isolated schema per test (`MINO_TEST_DATABASE_URL`)
- [x] Builds; migrations apply on an empty DB; re-running is a no-op (tested)
- [x] Sites: `sites` table; content types, media, API tokens and public permissions
      belong to a site; `site_members` assigns users a role per site
- [x] Sites service (create, rename, delete with confirmation) + tests

## Phase 1 — Schema & Content-Type Builder logic

- [x] Field types: Short Text, Long Text, Rich Text, Number, Boolean, Date, DateTime,
      Email, Enumeration, JSON, Media, Relation
- [x] Field settings: name, label, required, unique, default, description, editable,
      private, validation rules (min/max length, min/max, regex), type options
- [x] Relations: oneToOne, oneToMany, manyToOne, manyToMany
- [x] Schema validation (names, reserved words, options, defaults, relation targets)
- [x] Change planning: safe / needs confirmation / blocked — no silent data loss
- [x] Unique constraints via partial unique indexes
- [x] Schema history (every version kept)
- [x] Rich-text HTML sanitizer
- [x] Tests: schema validation, value coercion, diff, impact planning, sanitizer
- [x] Content types scoped to a site (names unique per site, relations only within a site)

## Phase 2 — Content engine

- [x] Entry validation against the saved schema (partial updates, required, defaults)
- [x] CRUD + duplicate; single-type get/upsert
- [x] Relations with cardinality enforcement; media links
- [x] Filtering (`$eq $ne $lt $lte $gt $gte $in $notIn $contains $containsi
      $startsWith $endsWith $null $notNull $between`, `$and/$or/$not`, relation id)
- [x] Sorting, pagination, field selection, populate
- [x] Draft & publish (`published_at`)
- [x] Batched loading of relations/media (no N+1)
- [x] Unique violations mapped to field errors
- [x] Tests: CRUD, relations, filters, drafts, unique, site isolation, query count
- Notes: required fields are only enforced on published entries (drafts may be incomplete);
  text sorting is case-insensitive; use a Debian-based Postgres image in production
  (Alpine/musl collation is byte order).

**Checkpoint 1 — review engine before building on it.**

## Phase 3 — Media library

- [x] Storage interface; S3-compatible backend (AWS S3, R2, MinIO) as the main store,
      local disk fallback when no bucket is configured (dev/tests)
- [x] Media library per site
- [x] Upload validation: size limit, sniffed MIME allowlist (no SVG/HTML)
- [x] Metadata: name, size, MIME, width/height, alt text, caption
- [x] Browse, search, update, delete (with usage count)
- [x] Tests (incl. SigV4 against the AWS test vector and an S3 integration test)

## Phase 4 — Authentication & permissions

- [x] Argon2id password hashing
- [x] Sessions: hashed token in DB, secure cookie, idle + absolute expiry
- [x] CSRF tokens for admin mutations + Origin check
- [x] Login rate limiting
- [x] First-run setup (create Super Admin)
- [x] Super Admins see every site and manage users, roles, sites and global settings
- [x] Per-site membership: a user gets a role on each site they are assigned to
- [x] Roles: Site Admin, Editor, Author (own-entries-only rules); custom roles
- [x] Permissions for content types, entries, media, schema, API tokens
- [x] API tokens (read-only, full-access, custom) and public API permissions, per site
- [x] Tests

## Phase 5 — REST API

- [x] `GET/POST /api/{site}/{plural}`, `GET/PUT/DELETE /api/{site}/{plural}/{id}`
- [x] `GET/PUT/DELETE /api/{site}/{single}`
- [x] Tokens only work for the site they were created in
- [x] Query: `pagination[page|pageSize]`, `sort`, `filters[...]`, `fields`, `populate`, `status`
- [x] Strapi-style response and error envelopes
- [x] Token / public permission checks
- [x] Upload endpoints
- [x] `docs/API.md`
- [x] Endpoint tests
- [x] CORS for browser frontends (`MINO_CORS_ORIGINS`)
- Measured (release binary, 200 entries, 16 concurrent clients): RSS 19 MB idle, 22 MB peak.

**Checkpoint 2 — review API contract.**

## Phase 6 — Admin UI

- [x] Frontend build (Tailwind + DaisyUI, HTMX, Alpine, TipTap) — output embedded in the binary
- [x] Site switcher; site management (Super Admin)
- [x] Layout, sidebar, shared components: tables, modals, confirm dialogs, toasts,
      loading indicators, empty states, pagination
- [x] Login / logout / first-run setup
- [x] Dashboard
- [x] Content-Type Builder: create/edit types, add/edit/reorder/remove fields, change confirmation
- [x] Content Manager: list, search, filter, sort, paginate, create, edit, duplicate, delete,
      publish/unpublish, JSON preview; dynamic forms incl. relation and media pickers, rich text
- [x] Media Library UI
- [x] Settings: users, roles & permissions, API tokens, public API access, general settings
- [x] Tests: HTTP-level setup, login/logout, rate limiting, CSRF + Origin, open redirect,
      form validation/publish, per-site and per-role enforcement, deactivation
- [x] Browser walkthrough (Chrome): 11 flows incl. builder, pickers, uploads, author view, mobile, dark mode
- Measured (release binary, 16 concurrent admin requests): RSS 21 MB at start, 50 MB peak, ~35 MB settled.

**Checkpoint 3 — review full product in the browser.**

## Phase 7 — Delivery

- [ ] Dockerfile (static binary, distroless) and docker-compose
- [ ] README (setup, configuration, deployment)
- [ ] `docs/ARCHITECTURE.md`
- [ ] Memory/startup measurements under representative load
- [ ] Security pass

---

## Decisions

| Topic | Decision |
|---|---|
| Draft/publish | One version per entry + `published_at` (Strapi v4 model). Editing a published entry changes it live. |
| Relations | Unidirectional, stored from the owning side in `relations`. No automatic inverse fields. |
| IDs | Numeric entry ids. |
| Authenticated API | API tokens. No end-user registration/login. |
| Storage of content | `entries.data` JSONB; relations and media in link tables; no table per content type. |
| Frontend tooling | Node used only to build CSS/JS; built assets are embedded, runtime needs only the Go binary. |
| Dev database | `mino-dev-db` container on `127.0.0.1:5440`. |
| Multiple websites | One install, many sites. Site = project boundary for content types, entries, media, tokens, public permissions. Admin URLs carry the site (`/admin/s/{site}/...`). |
| Site access | Super Admins: all sites. Others: assigned per site with a role on each. Roles are shared templates; content permissions target `*` or a specific content type. |
| Media storage | S3-compatible by default; local disk fallback. |

## Out of scope (initially)

Plugins, GraphQL, i18n, marketplace, SSO, workflows, audit system, email providers,
webhooks, AI, real-time collaboration, microservices. Multi-site is in scope; full tenant
isolation (separate databases, per-tenant billing or quotas) is not.
