-- Core schema: sites, users & permissions, sessions, content types, entries, relations,
-- media, API tokens, settings.

-- A site is a project boundary: content types, media, API tokens and public permissions
-- all belong to exactly one site. The slug is used in API and admin URLs.
CREATE TABLE sites (
    id          bigserial PRIMARY KEY,
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Roles are shared templates applied per site through site_members.
CREATE TABLE roles (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- subject is '*' (every content type), 'ct:<content type id>', or '' for non-content actions.
CREATE TABLE role_permissions (
    role_id  bigint NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    action   text   NOT NULL,
    subject  text   NOT NULL DEFAULT '',
    own_only boolean NOT NULL DEFAULT false,
    PRIMARY KEY (role_id, action, subject)
);

CREATE TABLE admin_users (
    id            bigserial PRIMARY KEY,
    email         text NOT NULL,
    name          text NOT NULL,
    password_hash text NOT NULL,
    super_admin   boolean NOT NULL DEFAULT false, -- all sites, users, roles and settings
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);
CREATE UNIQUE INDEX admin_users_email_key ON admin_users (lower(email));

-- Non-super-admin users only see the sites they are members of, with one role per site.
CREATE TABLE site_members (
    site_id bigint NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES admin_users (id) ON DELETE CASCADE,
    role_id bigint NOT NULL REFERENCES roles (id),
    PRIMARY KEY (site_id, user_id)
);
CREATE INDEX site_members_user_idx ON site_members (user_id);

-- Only a hash of the session token is stored; the raw token lives in the cookie.
CREATE TABLE sessions (
    token_hash   bytea PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES admin_users (id) ON DELETE CASCADE,
    csrf_token   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    ip           text NOT NULL DEFAULT '',
    user_agent   text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- uid is the singular API id (also the route of a single type); plural_name is the collection route.
-- Both are unique within a site.
CREATE TABLE content_types (
    id            bigserial PRIMARY KEY,
    site_id       bigint NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    uid           text NOT NULL,
    plural_name   text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('collection', 'single')),
    display_name  text NOT NULL,
    description   text NOT NULL DEFAULT '',
    draft_publish boolean NOT NULL DEFAULT true,
    fields        jsonb NOT NULL DEFAULT '[]',
    version       integer NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (site_id, uid),
    UNIQUE (site_id, plural_name)
);

-- Every schema change keeps the previous definition for auditing and manual recovery.
CREATE TABLE content_type_history (
    id              bigserial PRIMARY KEY,
    content_type_id bigint NOT NULL REFERENCES content_types (id) ON DELETE CASCADE,
    version         integer NOT NULL,
    definition      jsonb NOT NULL,
    changed_by      bigint REFERENCES admin_users (id) ON DELETE SET NULL,
    changed_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX content_type_history_type_idx ON content_type_history (content_type_id, version DESC);

CREATE TABLE entries (
    id              bigserial PRIMARY KEY,
    content_type_id bigint NOT NULL REFERENCES content_types (id) ON DELETE CASCADE,
    data            jsonb NOT NULL DEFAULT '{}',
    published_at    timestamptz,
    created_by      bigint REFERENCES admin_users (id) ON DELETE SET NULL,
    updated_by      bigint REFERENCES admin_users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX entries_type_created_idx ON entries (content_type_id, created_at DESC, id DESC);
CREATE INDEX entries_type_updated_idx ON entries (content_type_id, updated_at DESC);
CREATE INDEX entries_type_published_idx ON entries (content_type_id, published_at);
-- Serves equality filters, which the engine expresses as JSONB containment (data @> ...).
CREATE INDEX entries_data_idx ON entries USING gin (data jsonb_path_ops);

-- Relations are stored from the owning (source) side only.
CREATE TABLE relations (
    source_id bigint  NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    field     text    NOT NULL,
    target_id bigint  NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    position  integer NOT NULL DEFAULT 0,
    PRIMARY KEY (source_id, field, target_id)
);
CREATE INDEX relations_target_idx ON relations (target_id, field);

CREATE TABLE media (
    id          bigserial PRIMARY KEY,
    site_id     bigint NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    name        text NOT NULL,
    alt         text NOT NULL DEFAULT '',
    caption     text NOT NULL DEFAULT '',
    mime        text NOT NULL,
    size        bigint NOT NULL,
    width       integer,
    height      integer,
    storage     text NOT NULL,
    storage_key text NOT NULL UNIQUE,
    created_by  bigint REFERENCES admin_users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX media_site_created_idx ON media (site_id, created_at DESC, id DESC);

CREATE TABLE media_links (
    entry_id bigint  NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    field    text    NOT NULL,
    media_id bigint  NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    position integer NOT NULL DEFAULT 0,
    PRIMARY KEY (entry_id, field, media_id)
);
CREATE INDEX media_links_media_idx ON media_links (media_id);

CREATE TABLE api_tokens (
    id           bigserial PRIMARY KEY,
    site_id      bigint NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    name         text NOT NULL,
    description  text NOT NULL DEFAULT '',
    type         text NOT NULL CHECK (type IN ('read-only', 'full-access', 'custom')),
    token_hash   bytea NOT NULL UNIQUE,
    token_prefix text NOT NULL,
    permissions  jsonb NOT NULL DEFAULT '[]',
    expires_at   timestamptz,
    last_used_at timestamptz,
    created_by   bigint REFERENCES admin_users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (site_id, name)
);

-- Actions the unauthenticated public may perform on a site's REST API ("find", "findOne", ...).
-- subject is a content type uid of that site.
CREATE TABLE public_permissions (
    site_id bigint NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    action  text NOT NULL,
    subject text NOT NULL,
    PRIMARY KEY (site_id, action, subject)
);

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Seed data ---------------------------------------------------------------

INSERT INTO roles (name, description) VALUES
    ('Site Admin', 'Everything on the site: content, media, content types and API access.'),
    ('Editor', 'Manages and publishes all content and media.'),
    ('Author', 'Creates content and manages their own entries.');

INSERT INTO role_permissions (role_id, action, subject, own_only)
SELECT r.id, p.action, p.subject, p.own_only
FROM roles r
JOIN (VALUES
    ('Site Admin', 'content.read',    '*', false),
    ('Site Admin', 'content.create',  '*', false),
    ('Site Admin', 'content.update',  '*', false),
    ('Site Admin', 'content.delete',  '*', false),
    ('Site Admin', 'content.publish', '*', false),
    ('Site Admin', 'media.read',      '',  false),
    ('Site Admin', 'media.upload',    '',  false),
    ('Site Admin', 'media.update',    '',  false),
    ('Site Admin', 'media.delete',    '',  false),
    ('Site Admin', 'schema.manage',   '',  false),
    ('Site Admin', 'tokens.manage',   '',  false),
    ('Editor', 'content.read',    '*', false),
    ('Editor', 'content.create',  '*', false),
    ('Editor', 'content.update',  '*', false),
    ('Editor', 'content.delete',  '*', false),
    ('Editor', 'content.publish', '*', false),
    ('Editor', 'media.read',      '',  false),
    ('Editor', 'media.upload',    '',  false),
    ('Editor', 'media.update',    '',  false),
    ('Editor', 'media.delete',    '',  false),
    ('Author', 'content.read',    '*', false),
    ('Author', 'content.create',  '*', false),
    ('Author', 'content.update',  '*', true),
    ('Author', 'content.delete',  '*', true),
    ('Author', 'media.read',      '',  false),
    ('Author', 'media.upload',    '',  false)
) AS p (role, action, subject, own_only) ON p.role = r.name;

INSERT INTO settings (key, value) VALUES
    ('general', '{"siteName": "Mino", "defaultPageSize": 25, "maxPageSize": 100}');
