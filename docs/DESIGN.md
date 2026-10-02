# trunkcms — Design Plan

A small, stateless blog engine written in Go. Content lives in a GitHub repository.
Editors sign in with GitHub, edit Markdown and site settings in a web UI, and each save
becomes a commit. Every running instance picks up the new commit, rebuilds the site
in memory, and swaps it in with no downtime.

The only external dependency is GitHub. No database, no object storage, no queue, no
Redis. It runs the same way on Kubernetes, Cloud Run, ECS/Fargate, or Lambda.

---

## 1. Core idea: the Git repo is the database, and each instance is a cache

```
                ┌──────────────────────── GitHub ────────────────────────┐
                │  content repo (posts/, pages/, assets/, site.yaml)      │
                │  GitHub App: auth, API access, push webhooks            │
                └───────▲───────────────┬───────────────────▲─────────────┘
          commit (Git   │               │ tarball @ SHA     │ webhook (push)
          Data API)     │               │ + ETag polling    │
                ┌───────┴───────────────▼───────────────────┴─────────────┐
                │ trunkcms instance (N replicas, stateless)                 │
                │                                                          │
                │  source ──► snapshot on disk (src/<sha>/)                │
                │                │                                         │
                │                ▼                                         │
                │  build ──► objects/ on disk + URL index in memory        │
                │                │   atomic.Pointer swap                   │
                │                ▼                                         │
                │  public handler  /*          admin handler  /admin/*     │
                └──────────────────────────────────────────────────────────┘
```

**"Rebuild and redeploy" happens inside the process, not as a new container deploy.**
When a new commit shows up, the instance streams the repo tarball at that SHA onto local disk,
renders the whole site into a content-addressed file store on disk, and swaps a pointer to the new
URL index. Memory only holds the index and the parsed content (see §4.1). Going this way instead of
"GitHub Actions builds a new image and redeploys it" has these advantages:

- It works the same on all target platforms. There is no per-platform deploy pipeline and no cloud credentials in GitHub.
- A change goes live in seconds, not minutes.
- The container image is the engine only, so a content change never needs an image rebuild.

A blog with a few hundred posts should render in well under a second, so rebuilding the whole
site each time is fine. Incremental builds are a non-goal for v1.

---

## 2. Content repository layout

The content repo is separate from the engine repo. One engine image can serve any content repo.

```
site.yaml                 # site settings (editable from the UI)
posts/
  2026-09-30-hello-world.md
  2026-10-02-second-post/  # optional bundle form: index.md + co-located images
    index.md
    diagram.png
pages/
  about.md                 # → /about/
authors/
  alice-gh.md              # public author profile → /authors/alice-gh/
assets/                    # uploaded images/files → /assets/...
.trunkcms/
  users.yaml               # permissions only (admin-editable), see §3.2
themes/                    # OPTIONAL selectable themes, chosen with site.yaml theme.name
  paper/
    theme.yaml             # name, version, description, author, homepage, license
    screenshot.png         # optional; also .jpg/.jpeg/.webp. Shown in the theme picker
    templates/*.html
    static/*
theme/                     # OPTIONAL site-specific overrides, on top of the selected theme
  templates/*.html
  static/*
```

Front matter is YAML:

```yaml
---
title: Hello, world
date: 2026-09-30T09:00:00Z
slug: hello-world          # optional, derived from filename
tags: [go, meta]
author: alice-gh           # GitHub login; set automatically to the creator
summary: Optional excerpt
draft: false
---
```

`site.yaml` looks like this:

```yaml
title: My Blog
description: Notes on things
base_url: https://blog.example.com
language: en
author: { name: Jane Doe, email: jane@example.com }
posts_per_page: 10
permalink: /posts/:slug/     # default; also supports :year, :month, :day
nav:
  - { title: About, url: /about/ }
feeds: { rss: true, atom: true }
theme: { name: paper }       # a themes/ dir; empty = the built-in theme
search: { enabled: true }    # off unless set; new sites get it on (§4.2)
```

An author profile (`authors/<login>.md`) is public content, rendered like a page:

```yaml
---
name: Alice Example
avatar: /assets/authors/alice.jpg   # optional; defaults to the GitHub avatar
links:
  - { title: Website, url: https://alice.example }
  - { title: Mastodon, url: https://example.social/@alice }
---
Alice writes about distributed systems and sourdough. *(Markdown bio)*
```

Profiles and permissions are kept in separate files on purpose:
- **Different edit rights.** Every user can edit their *own* profile, but only admins can change roles.
- **Different lifetimes.** Removing someone's access must not remove the byline on their old posts. Guest authors can have a profile without ever logging in.
- **Different visibility.** Profiles are rendered on the site. `users.yaml` is config and is never rendered.

The `author:` field in a post resolves to the profile. It links the byline to `/authors/<login>/`, which lists that author's posts, and sets the feed `<author>`. If an author has no profile file, the byline falls back to the login.

Settings that belong to the engine, and all secrets, come from environment variables. They are
never stored in the repo (see §7).

---

## 3. GitHub integration

### 3.1 Use a GitHub App (not an OAuth App or a PAT)
A single GitHub App provides everything the engine needs:

| Need | GitHub App feature |
|---|---|
| Read content, server side | **Installation token**. The engine signs an RS256 JWT with the App private key and exchanges it for a 1-hour token scoped to the one repo. |
| Editor login | **User-to-server OAuth** (the App's OAuth flow). The resulting user token is used once to identify the user, then discarded. |
| Commits show the real author | Commits are made with the **installation token**, and the git *author* is set to the user as `Name <ID+login@users.noreply.github.com>`. GitHub links that noreply address to the account, so the commit shows the user's avatar and profile. |
| Change notifications | **Push webhook**, delivered to `/admin/webhook` and checked with an HMAC-SHA256 signature. |

App permissions: `Contents: read & write` and `Metadata: read`. Webhook events: `push`.

### 3.2 Authorization (hybrid roles)
GitHub login is used **only for identity** (login and numeric ID). The engine never stores the user's token.
Permissions come from two places:

1. **Anyone who can push to the repo (write, maintain, or admin on GitHub) is always trunkcms `admin`.** This is checked at login with
   `GET /repos/{o}/{r}/collaborators/{user}/permission`. It also covers first run: a new repo with
   no users file still has an admin.
2. **Everyone else is listed in `.trunkcms/users.yaml`** and needs **no access to the repo**:

```yaml
users:
  alice-gh:  { id: 1234567, role: author }
  sally-foo: { id: 7654321, role: editor }
```

The `id` is filled in automatically when an admin adds someone in the UI. It stops a renamed-then-reclaimed
username from inheriting a role.

| Role | Can do |
|---|---|
| `author` | Create posts, edit/delete **own** posts (`author:` == self; changing `author` is rejected), edit own profile, upload assets |
| `editor` | All posts and pages, all profiles |
| `admin` | Everything, plus `site.yaml` and `.trunkcms/users.yaml` |

**Roles are checked again on every write** against `users.yaml` in the current snapshot, which is in memory
and costs nothing. Removing someone takes effect on the next commit, not when their session expires.

**What this protects, and what it doesn't.** People listed in `users.yaml` do not need GitHub access to the
repo, so for them the service is the *only* way in and its rules are fully enforced. Anyone with direct push
access can edit anything with git, including `users.yaml`. That is expected: repo write access on GitHub is
equivalent to trunkcms admin.

**Keep the content repo private.** In a public repo, drafts and `users.yaml` can be read by anyone on
github.com. The admin dashboard shows a warning when the repo is public.

#### Capabilities, not role checks
Code never checks `role == "admin"`. Every protected action goes through one policy function, and the roles
are just named sets of capabilities:

```go
policy.Can(user, policy.PostEdit, post)   // resource-aware: authors only match their own posts
```

| Capability | author | editor | admin |
|---|:-:|:-:|:-:|
| `post.create` | ✓ | ✓ | ✓ |
| `post.edit` / `post.delete` / `draft.view` | own | all | all |
| `page.edit` | | ✓ | ✓ |
| `profile.edit` | own | all | all |
| `asset.upload` | ✓ | ✓ | ✓ |
| `settings.edit` | | | ✓ |
| `users.manage` | | | ✓ |

The same `Can` check is used in two places:
- **When rendering**, so the UI only shows what the user can use. Authors see "My posts" and "New post", not
  Pages, Settings, Users, or other people's drafts.
- **When handling requests**, including when the commit is built: every changed path is checked against the
  user's capabilities before it is committed. Hiding a button is never the only protection.

New features (nav/menu editing, custom pages, theme editing in the UI) add a capability and a column
entry. They don't add new `if` statements throughout the handlers. Custom roles defined in `users.yaml`
(`roles: { designer: [theme.edit, page.edit] }`) fit this model and can come later.

### 3.3 Reading content: one tarball call per build
`GET /repos/{o}/{r}/tarball/{sha}` is a single API call. The engine streams it through
gzip and tar straight into `src/<sha>/` on disk (limits: 100 MB per file, 10 GB total), and the builder
reads it through `os.DirFS`. Local mode copies the content directory the same way.
With `TRUNKCMS_REPO_PATH` set, only files under that directory are extracted, relative to it,
and commits add the directory back to every path. The rest of the engine only sees site paths.

### 3.4 Writing content: atomic multi-file commits
Writes go through the **Git Data API**, not the Contents API. That way one save can contain
the post, its uploaded images, and settings changes together:

1. `POST /git/blobs` for each binary file (text files can go inline in the tree)
2. `POST /git/trees` with `base_tree = <base commit's tree>`
3. `POST /git/commits` with `parents = [<base SHA>]`
4. `PATCH /git/refs/heads/<branch>` with `force: false`

**Optimistic concurrency:** the editor form carries the `base SHA` it was loaded from.
If another commit landed first, step 4 fails because it is not a fast-forward. The UI
then shows a conflict ("this file changed since you opened it") with the option to reload.
If the other commit touched *different* files, the engine can rebase automatically by rebuilding the
tree on the new head and retrying once.

### 3.5 Change detection: webhook + ETag polling
Replicas are independent, and a webhook only reaches one of them. So there are three triggers,
and all of them lead to the same `source.Sync()`:

1. **Webhook** (`push` to the configured branch): immediate sync on the replica that receives it.
2. **Read-your-writes**: the replica that made a commit syncs right away to that SHA, so the
   editor sees the change immediately.
3. **Polling**: `GET /repos/{o}/{r}/commits/{branch}` with `If-None-Match`.
   A `304` response does **not** count against the rate limit. The default interval is 30s.
   The same check runs from two places, whichever comes first once `now - lastCheck > interval`:
   a background timer, and each public request. This works the same whether the process runs
   continuously or is frozen between requests and scaled to zero, so there's no per-platform mode.
   Request-triggered checks use stale-while-revalidate: the current site is served and the sync runs
   in the background, and only one check runs at a time.

`POLL_INTERVAL=0` turns polling off for setups that only use webhooks and run a single replica.

### 3.6 Rate limits
An installation token gets at least 5,000 requests per hour. Normal traffic uses almost none of them:
one tarball call per actual change, conditional polls that return 304 and cost nothing, and a
handful of calls per save. The admin UI reads file contents from the on-disk
snapshot, not from the API.

---

## 4. Build pipeline

```
fs.FS ──► load ──► Model ──► render ──► Output ──► swap
```

- **load**: parse `site.yaml`, walk `posts/` and `pages/`, parse front matter, validate
  (duplicate slugs, bad dates). Errors are collected, not fatal per file: a broken post
  is skipped and reported in the admin UI.
- **Model**: `Site{Config, Posts (sorted), Pages, Tags map, Assets}`.
- **render**: goldmark (GFM, footnotes, heading IDs, typographer) + chroma syntax
  highlighting (CSS classes, so the theme's stylesheet picks the colors), then `html/template` with the theme. Output includes index pagination,
  post pages, tag pages, static pages, RSS/Atom, `sitemap.xml`, `robots.txt`, 404, and the search index (§4.2).
- **Output**: an in-memory index `map[urlPath]*Entry{object, gzip, contentType, etag}`. Each
  rendered file is written to `objects/<sha256>` (plus `<sha256>.gz` for compressible types)
  in a content-addressed store. Identical content is stored once, so a rebuild only writes pages that changed.
  Assets are streamed from the snapshot and never loaded into memory.
- **swap**: `atomic.Pointer[Result]`. After the swap, snapshots and objects that neither the current nor the
  previous build uses are deleted. Keeping the previous build means requests already in progress still find their files.
- **Serving**: look up the URL in the index, open the object, then `http.ServeContent` (ETag/304, HEAD, Range).

### 4.1 Footprint (measured, 1,000-word posts)

| Site | Memory | Disk (snapshot + rendered) | Full build |
|---|---|---|---|
| 1,000 posts | ~7 MB | ~20 MB | 0.8 s |
| 5,000 posts | ~36 MB | ~97 MB | 3.8 s |
| 1,000 posts + 200 MB of images | ~8 MB | ~410 MB | 1.0 s |

Memory is mostly the parsed Markdown. Images only cost disk. Reproduce with
`TRUNKCMS_MEMTEST=1 go test -run MemoryFootprint -v ./internal/build/`.

**Validation builds** (run before every save) render against an overlay of the snapshot plus the pending
changes, using a nil object store: every page is rendered, but nothing is written or copied.

**Failed builds never replace a good site.** If the build fails (for example, `site.yaml` is
invalid), the previous output keeps serving and the error shows up in the admin dashboard
and the logs. The admin UI **validates before committing**: it runs the same load step on the
proposed tree first, so most bad saves are rejected before they reach Git.

**Theme:** a default theme is embedded with `embed.FS`. A content repo can hold more themes under
`themes/<name>/`, and `site.yaml`'s `theme.name` selects one (empty means the built-in default). Files
are looked up in layers (`fs.FS`): the repo's `theme/` first, then `themes/<name>/`, then the embedded
default. So a theme only needs the files it changes, and a site can tweak any theme through `theme/`.
Admins pick the theme on the Settings page, from cards that show each theme's `theme.yaml` details and
optional screenshot (`screenshot.png`, `.jpg`, `.jpeg` or `.webp` at the theme root; SVG isn't accepted
because the admin serves it from its own origin). The screenshot stands in for a live preview. Like every settings save, the change is
test-built first, so a missing or broken theme is rejected before it's committed. Theme files
themselves are read-only in the UI for v1.

`theme.yaml` describes a theme (name, version, description, author, homepage, license). It follows the
same layering, except that a selected theme without one is named after its directory. It is shown in
the admin and is available to templates as `.Theme`. It doesn't affect rendering, but a malformed one
fails the build. Theme names must be a single path segment (`[A-Za-z0-9][A-Za-z0-9._-]*`).

Themes have **no options** for now: a different look (colors, dark variant, code highlighting style) is
a different theme, which is cheap because a theme only contains the files it changes. That keeps each
theme's screenshot accurate and keeps the settings page free of settings that some themes ignore.
Code blocks are highlighted with Chroma CSS classes (`.chroma .kn` …), and the theme's CSS styles them.
The built-in theme ships Chroma's `github` and `github-dark` styles, one per color scheme.

**Drafts** (`draft: true`) are rendered into a separate **draft overlay** (`map[path]*Entry`),
not into the public output. They are left out of index pages, tag pages, feeds, and the sitemap. When the public handler gets a
request that isn't found in the public output, it checks the overlay, but only if the request carries a valid
**session cookie** for any logged-in role (see §6):
- Logged-in user with `draft.view` for that post (authors: their own; editors/admins: all): gets the draft at its real URL (e.g. `/posts/my-blog-post/`) with a "DRAFT" banner
  and `Cache-Control: private, no-store`, so a CDN or proxy never caches it.
- Anonymous visitor: gets the normal 404, exactly the same as a post that doesn't exist.

The cookie is only checked when the public lookup misses, so the anonymous hot path is unchanged.
List pages don't show drafts, even to editors. The admin post list is where drafts are found.
Posts with a future `date` stay hidden until that time. The poll check also triggers a rebuild
when the next scheduled post's publish time passes, so scheduled posts work without cron.

### 4.2 Search

Search runs in the browser against a JSON index written at build time, like the feeds. There is no
search endpoint: the public side stays a lookup of prebuilt files, static exports get search too,
and queries cost the server nothing.

```yaml
search:
  enabled: true
  pages: true                  # index pages as well as posts (default true)
  stopwords: [because]         # added to the built-in list for `language`
  min_kw_length: 2             # shortest word indexed (default 2)
  keep: [go, ai, ui, js]       # always indexed, even if short or a stopword
  title_boost: 1               # title match = this many best-possible body matches; 0 = off
```

Search is off unless `site.yaml` turns it on, so existing sites don't change. "Initialize site" turns
it on for new ones. The Settings page has all six options, and the dashboard shows the index size.

**What's indexed:** published posts (title ×1, tags ×3, summary ×2, body ×1) and, unless
`pages: false`, pages (title ×1, body ×1). Titles get their weight at query time instead (see Ranking). Body text is the rendered HTML with tags stripped, so
Markdown syntax and link URLs aren't indexed; code blocks are. **Drafts and scheduled posts are never
indexed**: the index is public, and it's built from the same list as the feeds.

**Words:** lowercased, NFKD-normalized with combining marks and apostrophes removed ("Café's" →
`cafes`), then split on anything that isn't a letter or digit. Words shorter than `min_kw_length`
and stopwords are dropped unless listed in `keep`. The built-in English list holds function words
only. It leaves out short words that are often topics (go, ai, ui, os, js) and common page names
(about). Other languages have no built-in list yet, so they rely on BM25 alone. `search.js`
tokenizes queries with the same rules (`internal/search/tokenize.go` and `words()` must match).

**Format** (`/search.json`, with the usual `max-age=60` and ETag). The URL is deliberately not
content-hashed like theme assets: the index changes on every save, and a page cached from two builds
back would point at an index that had already been garbage-collected.

```json
{"v": 1, "lang": "en", "title_boost": 1,
 "docs":  [["/posts/hello/", "Hello", "2026-09-30", 412, "Summary"], ...],
 "terms": {"hello": [0, 7, 3, 1], ...}}
```

`docs` holds URL, title, date (empty for pages), length (total weight, for BM25) and summary, so
results render without fetching pages. Newest posts come first, then pages. Each term's postings are
`[gap, weight, ...]`: doc indexes ascending, each stored as the gap from the previous one so gzip
compresses them well. Weights are the word's count times its field weight.

**Ranking:** BM25 (k1 = 1.2, b = 0.75), summed over query words. The last word also matches as a prefix
while typing, at 0.7× and only its best match per doc. Ties go to the newer post. The index loads the
first time the search box is focused.

A query word found in a doc's title adds `title_boost × idf × (k1 + 1)`. That's `title_boost` times the
most BM25 can ever give the word, since repeated words saturate toward `idf × (k1 + 1)`. That
saturation is why titles don't get a heavier field weight: at ×10, a post titled "Docker is great"
still loses to one that says "docker" 20 times. With the bonus, at the default of 1, a title match beats
every post that only mentions the word in the body, however often, while a post matching *more* of the
query's words can still win (`docker compose`). At 2, a title match on one word also outweighs matching
an extra query word. The browser tokenizes titles from `docs` itself, so the bonus costs no index space.

**Size**, measured on 300 real 1,000-word documents: about 530 KB raw, **160 KB gzipped**, with
~11,000 distinct words. Stopword and length settings barely change this (at most ~12%), since
most of the size is the long tail of rare words. That makes the settings about result quality, not
size. Splitting the index into one file per first letter is the next step if sites get big enough
to need it.

**Themes** get the box from the `search` partial, which the default `base.html` includes. A theme
that replaces `base.html` adds `{{template "search" .}}`, or builds its own UI from the
`searchIndex` template function, which returns the index URL or `""` when search is off. Themes
should never hard-code the index path.

**Several languages (planned, additive only).** The `search:` keys stay flat. A future top-level
`languages:` map gives per-language overrides for *any* `site.yaml` key, with the same shape as the
top level: maps merge, plain values and lists replace, and tags fall back (`pt-BR` → `pt`).

```yaml
language: en                   # the top level is the default language
languages:
  fr:
    title: Mon Blog
    search: { stopwords: [parce, contre] }
```

Posts would get an optional `lang:` front matter key (default: `language`), permalinks a `:lang`
token, and each language its own `/search.<lang>.json`, picked through `searchIndex`. File-name
suffixes like `post.fr.md` are ruled out because they would change existing slugs. None of this
changes today's files or the `v: 1` index format.

---

## 5. Admin UI

Server-rendered `html/template` plus about 50 lines of plain JavaScript (`admin.js`), all embedded. There is no
Node build step and no CDN. The Markdown editor is a plain `<textarea>` with a side-by-side
preview. On a debounce, the form is submitted to `/admin/preview` with the response loaded into a sandboxed
`<iframe>`, so the preview is a real page rendered with the live theme and never runs scripts. A richer editor can be
added later without changing the backend.

Uploaded images are **staged into the next save**. They are held in the browser form and
committed together with the post in a single atomic commit, so history never has orphaned uploads.

| Route | Purpose |
|---|---|
| `GET /admin/` | Dashboard: current SHA, last sync, build errors, recent commits |
| `GET /admin/posts`, `/admin/pages` | Lists with draft/scheduled status |
| `GET /admin/edit?path=…` | Editor (front matter fields as a form + Markdown body) |
| `POST /admin/preview` | Renders unsaved content with the real theme (no commit) |
| `POST /admin/save` | Validates, commits, syncs. Commit message is optional and auto-filled when left blank |
| `POST /admin/delete` | Commits the deletion |
| `POST /admin/upload` | Image upload, staged into the next save or committed right away |
| `GET/POST /admin/settings` | Form generated from the `site.yaml` schema (admin) |
| `GET/POST /admin/users` | Add/remove users and change roles; the GitHub ID is looked up automatically (admin) |
| `GET/POST /admin/profile` | Edit your own `authors/<login>.md` (name, avatar, links, bio) |
| `GET /admin/login`, `/admin/callback`, `POST /admin/logout` | GitHub App OAuth |
| `POST /admin/webhook` | Push webhook (HMAC-verified, not session-authenticated) |

The UI works with **front matter as structured fields** (title, date, tags, draft) and
writes it back out in a stable order, so diffs stay clean.

---

## 6. Sessions and security (stateless)

- **Session** = one AES-256-GCM encrypted cookie holding `{login, id, issued_at, expiry}`, with
  **no GitHub token**. After login, the OAuth token is used once to read `/user` and the repo permission,
  then thrown away. The key comes from `TRUNKCMS_SESSION_KEY` (supports a comma-separated list to allow key rotation).
  Cookie settings: `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`. Because it's scoped to the whole site,
  the public handler can use it to unlock draft URLs (§4). Sessions last 7 days by default. The role is
  **not** stored in the cookie. It is looked up on each request from the snapshot and the cached repo permission.
- **CSRF**: `SameSite=Lax` plus a per-session token in forms and htmx headers,
  checked on every state-changing request. The OAuth `state` parameter is a signed, short-lived cookie.
- **Webhook**: constant-time HMAC check of `X-Hub-Signature-256`. Pushes to other branches are ignored.
- **Paths**: editable paths are limited to `posts/`, `pages/`, `authors/`, `assets/`, `site.yaml`, and
  `.trunkcms/users.yaml` (the last two admin-only),
  after cleaning with `path.Clean`. `..`, absolute paths, and anything else are rejected.
- **Markdown HTML**: pages always render raw HTML, since only editors and admins can change them. Posts and
  author profiles use goldmark's safe mode unless `site.yaml: markdown.unsafe_html: true`; authors write those
  and may not have repo access, and the site shares an origin with `/admin`, so a script there would run with
  the viewer's admin session.
- **Limits**: request body size, upload size (default 10 MB), allowed upload MIME types,
  tarball total/entry size caps.
- Security headers on admin pages (CSP without inline scripts, `X-Frame-Options: DENY`).

---

## 7. Configuration (env)

| Variable | Notes |
|---|---|
| `TRUNKCMS_REPO` | `owner/name`; also accepts `github.com/owner/name` or `https://github.com/owner/name` |
| `TRUNKCMS_REPO_PATH` | optional subdirectory of the repo holding the site (default: the repo root) |
| `TRUNKCMS_BRANCH` | default `main` |
| `TRUNKCMS_GITHUB_APP_ID`, `TRUNKCMS_GITHUB_INSTALLATION_ID` | installation ID auto-discovered if omitted |
| `TRUNKCMS_GITHUB_PRIVATE_KEY` or `_FILE` | PEM; `_FILE` variant for k8s/ECS secrets mounts |
| `TRUNKCMS_GITHUB_CLIENT_ID`, `TRUNKCMS_GITHUB_CLIENT_SECRET` | App OAuth |
| `TRUNKCMS_WEBHOOK_SECRET` | |
| `TRUNKCMS_SESSION_KEY` | 32-byte base64, comma-list for rotation |
| `TRUNKCMS_SITE_URL` | the site's URL; required. Used for the OAuth callback, secure cookies, and the initial `site.yaml base_url` |
| `TRUNKCMS_POLL_INTERVAL` | default `30s`; `0` disables |
| `TRUNKCMS_CONTENT_DIR` | local mode: serve and edit a directory; saves write files, and dev logins come from `TRUNKCMS_DEV_USERS` |
| `PORT` | default `8080` (Cloud Run convention) |

Every variable that holds a secret also accepts a `_FILE` form.

---

## 8. Deployment targets

The engine is one static binary (`CGO_ENABLED=0`) in a `distroless/static` image and listens on `$PORT`.

| Platform | Notes |
|---|---|
| **Kubernetes** | Deployment + Service + Ingress. Readiness probe `/readyz` passes after the first build succeeds; liveness `/healthz`. Any number of replicas. |
| **Cloud Run** | Same image. `min-instances=1` is recommended to avoid cold-start fetches. Works with CPU throttling and scale to zero. Cloud Run's filesystem is in memory, so the data dir counts against the memory limit there. |
| **ECS/Fargate** | Same image; secrets come from the task definition. ALB health check at `/readyz`. |
| **Lambda** | Same image with the **AWS Lambda Web Adapter** layer/extension, so there's no separate Lambda code path. The data dir lives in `/tmp` (512 MB by default, configurable up to 10 GB). Use a Function URL or API Gateway. |

Examples ship in `deploy/` (k8s manifests, Cloud Run YAML, ECS task def, Lambda Dockerfile/SAM snippet).

**Cold start:** an instance can't serve until its first snapshot has been downloaded and built.
`/readyz` stays unready until then. A persistent snapshot cache was considered and dropped: on these
platforms new instances start with an empty disk, so it would rarely have anything to reuse.

**Disk:** the data dir (`TRUNKCMS_DATA_DIR`) holds up to two snapshots plus the rendered objects.
Budget roughly 2× the repo size plus the rendered site. Kubernetes: `emptyDir`. ECS Fargate: task ephemeral storage.

---

## 9. Go project layout

```
cmd/trunkcms/main.go            # flags/env → wire everything → http.Server w/ graceful shutdown
internal/config/               # env parsing, _FILE secrets, validation
internal/github/               # thin client: app JWT, installation tokens, user OAuth,
                               #   tarball, commits(ETag), git data API, permission, webhook verify
internal/source/               # Source interface {Snapshot(ctx) (fs.FS, sha)}; GitHubSource,
                               #   DirSource; Syncer (poll/webhook, singleflight, swap)
internal/content/              # front matter parse/serialize, site.yaml schema, load & validate → Model
internal/render/               # goldmark setup, templates, feeds, sitemap, pagination → Output
internal/search/               # tokenizer, stopwords, JSON search index (§4.2)
internal/theme/default/        # embedded default theme (templates + CSS)
internal/server/               # public handler: lookup in Output, ETag/304, gzip; draft overlay for editors
internal/policy/               # capabilities, role → capability sets, Can(user, cap, resource)
internal/admin/                # handlers, sessions, CSRF, commit composition, conflict handling
internal/admin/ui/             # embedded admin templates + vendored htmx
deploy/                        # platform examples
testdata/site/                 # fixture content repo
```

**Dependencies (kept small):** only `github.com/yuin/goldmark` (+ `goldmark-highlighting`, `alecthomas/chroma`)
`gopkg.in/yaml.v3`, and `golang.org/x/text` (Unicode normalization for search). The GitHub client (App JWT, OAuth code exchange, about 12 endpoints), sessions, and
sync coordination are hand-written against the standard library.

---

## 10. Milestones

1. **Renderer, local only.** `content` + `render` + `server` with `DirSource`.
   `trunkcms serve --dir ./testdata/site` and `trunkcms build --dir … --out ./public`
   (static export is almost free and useful for testing). Golden-file tests.
2. **GitHub source.** App JWT → installation token, tarball → snapshot on disk, Syncer with
   polling (timer and per-request), webhook endpoint, atomic swap, `/readyz`.
3. **Auth.** App OAuth login (identity only), encrypted cookie sessions, hybrid roles from `users.yaml`, CSRF.
4. **Editor.** Post/page list, edit/create/delete, live preview, Git Data API commits,
   conflict detection with a single automatic rebase, read-your-writes sync.
5. **Settings + uploads.** Schema-driven `site.yaml` form, validate-before-commit,
   image upload into `assets/` or a post bundle.
6. **Ship.** Dockerfile, `deploy/` examples for all four platforms, README with GitHub App
   setup steps (a manifest-flow `/admin/setup` page that creates the App in one click is a stretch goal).

Testing: unit tests per package; an `httptest` fake GitHub server that implements the
endpoints above (tarball, refs, git data, ETag behavior) for integration tests of
sync and commit/conflict flows without the network.

---

## 11. Non-goals (v1)

Comments, server-side search (search is client-side, §4.2), multi-repo
or multi-tenant, editing themes in the UI, PR-based review workflows, incremental builds,
and non-GitHub forges.

## 12. Decisions

0. **Identity & roles:** hybrid model (§3.2). The App commits on the user's behalf, and author profiles
   are kept separate from permissions (§2).
1. **Drafts:** front matter `draft: true` on the main branch. No draft branches or PRs.
   Editors see drafts at the real URL, and anonymous visitors get a 404 (§4).
2. **Uploads:** staged into the next save (one atomic commit).
3. **Content repo:** always its own repo, chosen with `TRUNKCMS_REPO`. The engine never reads its own repo.
   `TRUNKCMS_REPO_PATH` can point at a subdirectory so one repo can hold several sites.
4. **Editor:** plain `<textarea>` + live preview for v1.

---

## 13. Getting started (user journey)

Example: blog content in `nullism/myblog`, served at `https://myblog.com`.

**1. Create the content repo.** `nullism/myblog` can be public or private, and it can be **empty**.
If there's no `site.yaml`, trunkcms serves a placeholder page, and the admin dashboard
offers an **"Initialize site"** button. That button commits a starter `site.yaml`,
`posts/hello-world.md`, and `pages/about.md`.

**2. Create a GitHub App (one time, about 5 minutes).** In GitHub → Settings → Developer settings → GitHub Apps:
- Redirect (callback) URL: `https://myblog.com/admin/callback`
- Webhook URL: `https://myblog.com/admin/webhook`, plus a random webhook secret
- Permissions: Contents **read & write**. Events: **push**
- Generate a private key and a client secret, then **install** the App on `nullism/myblog` only.
- Make the App **public** (Advanced → Make public). GitHub shows a 404 on its sign-in page to anyone outside
  the owning account of a private App. Public only lets other accounts sign in. Roles still come from
  repo permission and `users.yaml`.

Stretch goal: `/admin/setup` uses GitHub's App *manifest flow* to do this with one click, then
shows the resulting credentials to paste into the deployment's secrets. trunkcms is stateless and
can't store them itself.

**3. Run the published image.** There is no Dockerfile per blog: one image, configured with env vars.

```sh
docker run -p 8080:8080 \
  -e TRUNKCMS_REPO=nullism/myblog \
  -e TRUNKCMS_SITE_URL=https://myblog.com \
  -e TRUNKCMS_GITHUB_APP_ID=123456 \
  -e TRUNKCMS_GITHUB_PRIVATE_KEY_FILE=/secrets/app.pem \
  -e TRUNKCMS_GITHUB_CLIENT_ID=Iv1.abc... \
  -e TRUNKCMS_GITHUB_CLIENT_SECRET=... \
  -e TRUNKCMS_WEBHOOK_SECRET=... \
  -e TRUNKCMS_SESSION_KEY=$(openssl rand -base64 32) \
  -v ./secrets:/secrets:ro \
  nullism/trunkcms:latest
```

On Kubernetes, Cloud Run, ECS, or Lambda, these same values go in the platform's env and secret config.

**4. Startup:** wipe the data dir → get an installation token → stream the tarball of `main` to disk →
render the site to disk → `/readyz` goes green → start serving.

**5. Edit:** open `https://myblog.com/admin` → "Sign in with GitHub" → the engine checks that
you have write access to `nullism/myblog` → edit or create a post → **Save** creates a commit
authored by you → this instance rebuilds right away. Other replicas get the change from the webhook
or the poll within seconds.

**Editing outside the UI works the same way.** A `git push` from a laptop, a merged PR, or
GitHub's web editor all fire the push webhook, and the site rebuilds. The admin UI is
just one convenient way to make commits.

---

## 14. Prototype status (2026-09-30)

**Implemented:** the rendering pipeline (posts, bundles, pages, tags, author pages, RSS/Atom, sitemap,
theme overrides, drafts and scheduled posts); the GitHub client (App auth, ETag head polling, tarball snapshots,
Git Data API commits with a single automatic rebase and conflict detection, empty-repo bootstrap, OAuth, webhooks);
the syncer (polling, webhook, read-your-writes, last-good-build fallback); encrypted cookie
sessions; the capability policy; and the admin UI (dashboard, posts/pages CRUD, live preview, staged uploads,
settings, users, profiles, site initialization). Local mode (`TRUNKCMS_CONTENT_DIR`) runs everything without GitHub.
Tests cover content, policy, sessions, the syncer, the GitHub client against a fake API, and end-to-end HTTP flows.
The Dockerfile builds a static distroless image. Snapshots and rendered files live on disk (§4, §4.1).

**Not yet:** deploy examples for Cloud Run, ECS and Lambda (Kubernetes only so far),
the `/admin/setup` manifest flow, `trunkcms theme eject`, a site time zone setting (dates are UTC), and
keeping comments in `site.yaml` when it is saved from the UI (unknown keys are kept; comments are not).
The GitHub path has only been tested against the fake API, not against a real GitHub App.
