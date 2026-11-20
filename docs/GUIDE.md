# Fetching content from Mino

A practical guide to reading and writing content on a Mino site. For every operator, error
code and field format, see the [API reference](API.md).

```
Base URL:  {MINO_URL}/api/{site}
Example:   https://cms.example.com/api/blog
Auth:      Authorization: Bearer <API token>     (optional for public reads)
```

The query syntax is the same as Strapi's. From a shell, use `curl -g` or URL-encode the
`[` `]` brackets.

## How it works

Mino has no built-in "pages" or "sections". Each site defines its own **content types**,
and every content type is one of two kinds:

| Kind | URL | Returns |
|------|-----|---------|
| Collection type (many entries) | `/api/{site}/{plural}` and `/api/{site}/{plural}/{id}` | `data: [...]` (list) or `data: {...}` (one) |
| Single type (one entry) | `/api/{site}/{single}` | `data: {...}` |

"Pages" and "sections" are just content types a site defines. A typical website might have:

| Content type | Kind | URL | Links (need `populate`) |
|--------------|------|-----|-------------------------|
| Page | collection | `/pages` | `sections`, `seo_image` |
| Section | collection | `/sections` | `image`, `additional_fields` |
| Section Item | collection | `/section-items` | `file` |
| FAQ | collection | `/faqs` | none |
| Business Info | single | `/business-info` | none |

The content types of a site, and their fields, are listed in the admin panel under
**Content-Type Builder**.

## Authentication

Create a token under **Settings → API Tokens** of the site and send it as
`Authorization: Bearer mino_…`. A token only works on the site it was created for.

| Token type | Allows |
|------------|--------|
| `read-only` | `find` and `findOne` on every content type, plus listing media |
| `full-access` | everything on the site |
| `custom` | only the listed permissions, e.g. `find:article`, `create:lead`, `upload:write` |

A request **without** a token counts as the public. By default the public can do nothing.
Turn on `find` / `findOne` for each type under **Settings → API Access**. The public can never
write or read drafts.

A bad, expired or other-site token returns `401`. A valid caller without the needed
permission gets `403`.

## Response shape

Every successful response uses the same `data` + `meta` envelope. Lists also include
pagination:

```json
{
  "data": [ { "id": 84, "slug": "booking", "title": "Booking", "...": "..." } ],
  "meta": { "pagination": { "page": 1, "pageSize": 25, "pageCount": 1, "total": 1 } }
}
```

Each entry always includes `id`, `createdAt`, `updatedAt`, `publishedAt` and every scalar
field. Scalar fields with no value come back as `null`. Private fields are never returned.

> **Relations and media are left out entirely unless you `populate` them.** A page fetched
> without `populate` has no `sections` key at all.

## Fetching

List a collection:

```
GET /api/{site}/pages?fields=slug,title&pagination[pageSize]=100
```

Get one entry by id. An unknown id returns `404 NotFoundError`:

```
GET /api/{site}/sections/82?populate=*
```

Get one entry by slug. There is no special slug route, so filter on the slug and take
`data[0]`. The result is always a list: if nothing matches, `data` is `[]`, not a 404.

```
GET /api/{site}/pages?filters[slug][$eq]=booking&populate=sections,seo_image
```

Get a single type:

```
GET /api/{site}/business-info
```

## Populate

Use `populate` to include relation and media fields in the response.

```
populate=*                                    every relation and media field
populate=sections,seo_image                   only these
populate[0]=sections&populate[1]=seo_image    array form
```

Without `populate`:

```json
{ "id": 84, "slug": "booking", "title": "Booking", "meta_title": "...", "createdAt": "..." }
```

With `populate=sections,seo_image`:

```json
{
  "id": 84, "slug": "booking", "title": "Booking", "...": "...",
  "sections": [
    { "id": 82, "heading": "Book Medical Cover", "section_type": "Hero", "rank": 1 },
    { "id": 83, "heading": "Booking Enquiry", "section_type": "Booking Form", "rank": 2 }
  ],
  "seo_image": {
    "id": 58, "name": "hero-booking.jpg", "mime": "image/jpeg", "width": 1600, "height": 900,
    "url": "https://cms.example.com/uploads/3/238c….jpg"
  }
}
```

Rules:

| Rule | What it means |
|------|---------------|
| One level deep | Populated sections include their scalar fields, but not their own `image` or `additional_fields`. |
| No nested syntax | Strapi's `populate[sections][populate]=image` is not supported and returns `400`. |
| Single vs many | A single-valued link becomes an object or `null`. A multi-valued link becomes an array. |
| Published only | Populated relations include only published targets, unless you use `status=draft`. |
| Permission | The caller needs `find` on the target type, e.g. public `find` on Section. |
| Not in `fields` | Putting a relation in `fields=` returns `400`. Use `populate` instead. |

## Full page recipe

Because `populate` only goes one level deep, a complete page (sections, their images and
their items) takes three requests. Each request after the first uses the ids from the one
before it.

1. **Page with its sections.** Collect the `sections[].id` values.

   ```
   GET /pages?filters[slug][$eq]=booking&populate=sections,seo_image
   ```

2. **Sections with images and items.** Collect the `additional_fields[].id` values.

   ```
   GET /sections?filters[id][$in][0]=82&filters[id][$in][1]=83&populate=image,additional_fields&sort=rank:asc&pagination[pageSize]=100
   ```

3. **Section items with their files.** Then put them back together: items go into their
   section, and sections go into the page.

   ```
   GET /section-items?filters[id][$in][0]=52&filters[id][$in][1]=53&populate=file&pagination[pageSize]=100
   ```

A minimal client:

```js
const B = "https://cms.example.com/api/blog";
const get = (q) => fetch(B + q).then((r) => r.json()).then((j) => j.data);
const inIds = (list) => list.map((x, i) => `filters[id][$in][${i}]=${x.id}`).join("&");

const [page] = await get(`/pages?filters[slug][$eq]=booking&populate=sections,seo_image`);
const sections = page.sections.length
  ? await get(`/sections?${inIds(page.sections)}&populate=image,additional_fields&sort=rank:asc&pagination[pageSize]=100`)
  : [];
const refs = sections.flatMap((s) => s.additional_fields);
const items = refs.length
  ? await get(`/section-items?${inIds(refs)}&populate=file&pagination[pageSize]=100`)
  : [];
const byId = new Map(items.map((i) => [i.id, i]));
page.sections = sections
  .filter((s) => !s.hide_section)
  .map((s) => ({ ...s, additional_fields: s.additional_fields.map((r) => byId.get(r.id)).filter(Boolean) }));
```

Skip a request when its id list is empty.

## Filters

The pattern is `filters[field][$operator]=value`. Values are converted to the field's type,
so a number field compares numerically and a boolean field takes `true` or `false`.

```
filters[slug][$eq]=home
filters[section_type]=Hero                         implicit $eq
filters[rank][$gte]=2
filters[heading][$containsi]=medical
filters[slug][$in][0]=a&filters[slug][$in][1]=b
filters[$or][0][rank][$eq]=1&filters[$or][1][section_type][$eq]=Hero
filters[$not][hide_section][$eq]=true
filters[sections][id][$eq]=82                      by related id
filters[seo_image][$null]=true                     link present or not
filters[publishedAt][$gte]=2026-01-01
```

| Operator | Meaning |
|----------|---------|
| `$eq`, `$ne` | Equal or not equal. `$ne` also matches missing values. |
| `$eqi` | Equal, ignoring case (text fields) |
| `$lt`, `$lte`, `$gt`, `$gte`, `$between` | Comparisons. Numbers compare numerically, dates and text alphabetically. |
| `$in`, `$notIn` | Value is (or isn't) in a list |
| `$contains`, `$notContains`, `$containsi`, `$notContainsi` | Substring match. The `i` versions ignore case. |
| `$startsWith`, `$endsWith` | Prefix or suffix match |
| `$null`, `$notNull` | Use `=true` or `=false` |
| `$and`, `$or`, `$not` | Group conditions, nested up to 6 levels |

An unknown or private field, an unknown operator, or a value of the wrong type returns `400`.
JSON fields only support `$null` and `$notNull`.

## Sorting

```
sort=rank                        ascending
sort=rank:desc,title:asc         several fields
sort[0]=rank:desc&sort[1]=title
```

You can sort by `id`, the timestamp fields, and text, email, enumeration, number, boolean,
date and datetime fields. Ties are broken by `id`, so the order is always stable.

## Pagination

```
GET /api/{site}/pages?pagination[page]=2&pagination[pageSize]=2&fields=slug,title&sort=title:asc
```

```json
{
  "data": [
    { "id": 81, "slug": "contact-us", "title": "Contact Us" },
    { "id": 62, "slug": "home", "title": "Home" }
  ],
  "meta": { "pagination": { "page": 2, "pageCount": 4, "pageSize": 2, "total": 7 } }
}
```

| Param | Default | Notes |
|-------|---------|-------|
| `pagination[page]` | 1 | Must be 1 or more |
| `pagination[pageSize]` | 25 | Values above the maximum (100) are **silently capped**. Both limits are set in **Settings → General**. |

To fetch everything, keep requesting while `page < pageCount`.

## Choosing fields

```
fields=slug,title
fields[0]=slug&fields[1]=title
```

This limits which scalar fields come back. `id` is always included. Timestamps are left out
unless you list them. You can use `fields` and `populate` together.

## Drafts

```
status=published   default: published entries only
status=draft       unpublished entries only, needs an API token
```

Content types without draft & publish ignore `status`. In published mode, a draft fetched
by id returns `404`.

## Writing

Send a JSON body wrapped in `data`. For example, a contact form submission:

```http
POST /api/{site}/leads?status=published
Content-Type: application/json
Authorization: Bearer mino_…

{ "data": { "name": "Sam", "email": "sam@example.com", "message": "Hello" } }
```

Use the field names defined on your content type.

| Rule | Detail |
|------|--------|
| New entries are drafts | Unless you add `?status=published`. `PUT …?status=draft` unpublishes. |
| `PUT` is partial | Only the keys you send change. `null` clears a value. |
| Required fields | Checked when the entry is published. Drafts can be incomplete. |
| Links | Send an id, `{ "id": n }`, a list of those, or `null` / `[]` to clear. Setting a link replaces all of its links, in the order you send them. |
| Unknown fields | Rejected with `400`. Any `id` or timestamp in the body is ignored. |
| Responses | `POST` and `PUT` return the saved entry (and accept `populate` and `fields`). `DELETE` returns `204`. |

## Uploads

```bash
curl -H "Authorization: Bearer $TOKEN" \
     -F "files=@photo.jpg" -F "files=@brochure.pdf" \
     https://cms.example.com/api/blog/upload
```

Returns `201` with an array of media objects. You can send up to 20 files per request, each
up to 25 MB by default (`MINO_UPLOAD_MAX_MB`). The file type is detected from its content,
and SVG, HTML, XML and JavaScript are rejected.

## Errors

```json
{ "data": null,
  "error": { "status": 400, "name": "ValidationError",
             "message": "cannot populate \"image\": not a relation or media field",
             "details": {} } }
```

| Status | Name | Typical cause |
|--------|------|---------------|
| 400 | `ValidationError` | Bad query parameter, body or field value |
| 401 | `UnauthorizedError` | Bad, expired or other-site token |
| 403 | `ForbiddenError` | The caller doesn't have the permission |
| 404 | `NotFoundError` | Unknown site, type or id, or a draft in published mode |
| 405 | `MethodNotAllowedError` | e.g. `PUT /pages` with no id |
| 413 | `PayloadTooLargeError` | Body over 2 MB or file over the upload limit |
