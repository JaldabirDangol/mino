# Mino REST API

Every website managed by Mino has its own API under `/api/{site}`, where `{site}` is the
site's slug (for example `blog`). Content types, entries, media, tokens and public
permissions never cross sites.

```
GET    /api/{site}/{plural}            list entries of a collection type
GET    /api/{site}/{plural}/{id}       get one entry
POST   /api/{site}/{plural}            create an entry
PUT    /api/{site}/{plural}/{id}       update an entry (partial)
DELETE /api/{site}/{plural}/{id}       delete an entry

GET    /api/{site}/{single}            get a single type's entry
PUT    /api/{site}/{single}            create or update it
DELETE /api/{site}/{single}            delete it

GET    /api/{site}/upload/files        list media files
GET    /api/{site}/upload/files/{id}   get one file
POST   /api/{site}/upload              upload files (multipart, field "files")
DELETE /api/{site}/upload/files/{id}   delete a file

GET    /healthz                        "ok" when the database is reachable
```

`{plural}` is a collection type's plural API id (`articles`); `{single}` is a single type's
API id (`homepage`).

## Authentication

Send an API token created under **Settings → API Tokens** of the site:

```
Authorization: Bearer mino_xxxxxxxxxxxxxxxx
```

| Token type    | Allows |
|---------------|--------|
| `read-only`   | `find`, `findOne` on every content type; listing media |
| `full-access` | everything on the site |
| `custom`      | the permissions listed on the token, e.g. `find:article`, `create:article`, `upload:read`, `upload:write` |

Requests **without** a token act as the public. By default the public can do nothing; enable
`find` / `findOne` per content type under **Settings → API Access**. The public can never
write or read drafts.

A token only works on the site it was created for. A missing, invalid, expired or
foreign-site token returns `401`; a valid caller without permission gets `403`.

## Responses

Successful responses use one envelope:

```json
{
  "data": { "id": 1, "title": "Hello", "createdAt": "…", "updatedAt": "…", "publishedAt": "…" },
  "meta": {}
}
```

Lists add pagination:

```json
{
  "data": [ { "id": 1, "title": "Hello", "…": "…" } ],
  "meta": { "pagination": { "page": 1, "pageSize": 25, "pageCount": 4, "total": 87 } }
}
```

Entry shape:

- `id`, `createdAt`, `updatedAt`, `publishedAt` (null for drafts) are always present
  (timestamps are omitted when `fields` is used and does not list them).
- Every scalar field is present; fields without a value are `null`.
- **Private** fields are never returned.
- Relation and media fields are **omitted unless populated** (see `populate`). When
  populated, a single-valued field is an object or `null`; a multi-valued field is an array.

Media object:

```json
{ "id": 7, "name": "beach.jpg", "alternativeText": "Sunset", "caption": "", "mime": "image/jpeg",
  "size": 123456, "width": 1600, "height": 900, "url": "https://cdn.example.com/1/9f…e2.jpg",
  "createdAt": "…", "updatedAt": "…" }
```

Errors:

```json
{
  "data": null,
  "error": {
    "status": 400,
    "name": "ValidationError",
    "message": "title: This field is required",
    "details": { "errors": [ { "path": ["title"], "message": "This field is required" } ] }
  }
}
```

| Status | `name` |
|--------|--------|
| 400 | `ValidationError` — invalid body, field values or query parameters |
| 401 | `UnauthorizedError` — bad or foreign token |
| 403 | `ForbiddenError` — not permitted |
| 404 | `NotFoundError` — unknown site, type or entry (also drafts in published mode) |
| 405 | `MethodNotAllowedError` — e.g. `PUT /articles` without an id |
| 413 | `PayloadTooLargeError` — body over 2 MB or file over the upload limit |
| 500 | `InternalServerError` |

## Query parameters (GET)

Bracket syntax as in Strapi. Remember to URL-encode or disable globbing (`curl -g`).

### Pagination

```
pagination[page]=2&pagination[pageSize]=50
```

Defaults and the maximum page size come from **Settings → General** (25 and 100).

### Sorting

```
sort=title                 ascending
sort=views:desc,title:asc  several fields
sort[0]=views:desc&sort[1]=title
```

Sortable: `id`, `createdAt`, `updatedAt`, `publishedAt` and fields of type short/long
text, email, enumeration (case-insensitive), number, boolean, date, datetime.
Ties are broken by `id`. Private fields cannot be used for sorting or filtering.

### Filters

```
filters[title][$eq]=Hello
filters[category]=news                       implicit $eq
filters[views][$gte]=100
filters[title][$containsi]=guide
filters[slug][$in][0]=a&filters[slug][$in][1]=b
filters[$or][0][featured][$eq]=true&filters[$or][1][views][$gt]=1000
filters[$not][category][$eq]=news
filters[author][id][$eq]=3                   relation by related id
filters[cover][$null]=true                   media/relation present or not
filters[publishedAt][$gte]=2024-01-01
```

| Operator | Meaning |
|----------|---------|
| `$eq`, `$ne` | equal / not equal (`$ne` also matches missing values) |
| `$eqi` | case-insensitive equal (text fields) |
| `$lt`, `$lte`, `$gt`, `$gte`, `$between` | comparison (numbers numerically; dates and text lexically) |
| `$in`, `$notIn` | value in list |
| `$contains`, `$notContains`, `$containsi`, `$notContainsi`, `$startsWith`, `$endsWith` | text matching |
| `$null`, `$notNull` | `=true` / `=false` |
| `$and`, `$or`, `$not` | logical grouping (nesting up to 6 levels) |

Values are converted to the field's type (`views` → number, `featured` → `true`/`false`,
datetimes accept RFC 3339 or `YYYY-MM-DD`). Unknown or private fields, unknown operators and
values of the wrong type return `400`. JSON fields only support `$null`/`$notNull`.

### Field selection

```
fields=title,slug
fields[0]=title&fields[1]=slug
```

Restricts scalar fields. `id` is always returned.

### Populate

```
populate=*                 every relation and media field
populate=author,cover
populate[0]=author
```

One level deep. Populating a relation requires `find` permission on the target content
type for the caller (public or token). Populated relations only include published targets
unless `status=draft` is used.

### Draft & publish

For content types with draft & publish enabled:

```
status=published   (default) published entries only
status=draft       unpublished entries only — requires an API token
```

Types without draft & publish ignore `status`.

## Writing

Bodies are JSON wrapped in `data`:

```http
POST /api/blog/articles?status=published
Content-Type: application/json

{ "data": { "title": "Hello", "views": 3, "author": 5, "tags": [1, 2], "cover": 7 } }
```

- `PUT` is a **partial update**: only keys present change. `null` clears a value.
- New entries are **drafts** unless `?status=published` is given. `PUT …?status=published`
  publishes, `PUT …?status=draft` unpublishes. Types without draft & publish are always published.
- **Required fields are enforced when an entry is published** (drafts may be incomplete).
- Relations and media take an id, `{ "id": n }`, a list of those, or `null` / `[]` to clear.
  Setting a link field replaces all its links; order is preserved.
- `oneToOne` / `oneToMany` targets belong to one source entry: linking a target that is
  already linked elsewhere moves it.
- Unknown fields are rejected. `id`, `createdAt`, `updatedAt`, `publishedAt` in the body are ignored.
- `POST`/`PUT` return the written entry (accepting `populate` and `fields`); `DELETE` returns `204`.

### Field value formats

| Type | JSON value |
|------|------------|
| Short Text | string, max 255 characters unless a max length is set |
| Long Text | string |
| Rich Text | HTML string; sanitized to a safe subset (scripts, event handlers and unsafe URLs removed) |
| Number | number (integer format rejects decimals) |
| Boolean | `true` / `false` |
| Date | `"YYYY-MM-DD"` |
| DateTime | RFC 3339; stored and returned as UTC `"2024-05-01T09:30:00.000Z"` |
| Email | string |
| Enumeration | one of the configured values |
| JSON | any JSON value |
| Media | media id(s) from the same site's library |
| Relation | entry id(s) of the target content type in the same site |

Empty strings are treated as "no value".

## Uploads

```bash
curl -H "Authorization: Bearer $TOKEN" \
     -F "files=@photo.jpg" -F "files=@brochure.pdf" \
     https://cms.example.com/api/blog/upload
```

Returns `201` with `data` as an array of media objects. Up to 20 files per request; each up
to `MINO_UPLOAD_MAX_MB` (default 25 MB).

Accepted types are detected from file content, not the name: JPEG, PNG, GIF, WebP, AVIF,
MP4, WebM, MP3, WAV, OGG, PDF, ZIP, Office/OpenDocument files, plain text, CSV, Markdown and
JSON. SVG, HTML, XML and JavaScript are rejected because they can carry scripts.

`GET /upload/files` accepts `pagination[page]`, `pagination[pageSize]`, `search` (name, alt
text, caption) and `type` (`images`, `videos`, `audios`, `files`).

## CORS

Browsers may call the API from any origin by default (`MINO_CORS_ORIGINS=*`). Restrict it with
a comma-separated list, e.g. `MINO_CORS_ORIGINS=https://www.example.com,https://shop.example.com`.
