# trunkcms

[![CI](https://github.com/nullism/trunkcms/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/nullism/trunkcms/actions/workflows/ci.yml)

A small, stateless blog engine in Go. Content lives in a GitHub repository. Editors
sign in with GitHub, write Markdown in a web UI at `/admin`, and every save is a
commit. Each running instance picks up new commits, rebuilds the site in memory,
and swaps it in. There's no database; GitHub is the only external service.

Homepage: [trunkcms.nullism.com](https://trunkcms.nullism.com)

See [docs/DESIGN.md](docs/DESIGN.md) for the full design.

## Try it locally (no GitHub needed)

```sh
TRUNKCMS_CONTENT_DIR=./testdata/site go run ./cmd/trunkcms
```

Open http://localhost:8080 for the site and http://localhost:8080/admin to edit.
Local mode offers three dev logins (`dev-admin`, `dev-editor`, `dev-author`) so you
can try each role. Saves are written straight into the content directory, and edits
you make to those files by hand show up within about 2 seconds.

Note: this writes to `testdata/site`. Copy it somewhere first if you want to keep the fixture clean.

Render a content directory to static files:

```sh
go run ./cmd/trunkcms build -dir ./testdata/site -out ./public
```

## Run against GitHub

1. Create the content repo (e.g. `nullism/myblog`). Keep it **private** so drafts stay hidden. It can be empty.
2. Create a GitHub App:
   - Redirect (callback) URL: `https://myblog.com/admin/callback`
   - Webhook URL: `https://myblog.com/admin/webhook`, with a random secret
   - Repository permissions: **Contents: read & write**. Subscribe to **Push** events.
   - Generate a private key and a client secret, then install the App on the content repo only.
   - Under **Advanced**, make the App **public**. Otherwise GitHub shows a 404 to anyone but the App's owner when they try to sign in. Public only means other accounts can sign in through it. Access is still decided by repo permission and `users.yaml`.
3. Run the image from Docker Hub ([`nullism/trunkcms`](https://hub.docker.com/r/nullism/trunkcms)):

```sh
docker run -p 8080:8080 \
  -e TRUNKCMS_REPO=nullism/myblog \
  -e TRUNKCMS_SITE_URL=https://myblog.com \
  -e TRUNKCMS_GITHUB_APP_ID=123456 \
  -e TRUNKCMS_GITHUB_PRIVATE_KEY_FILE=/secrets/app.pem \
  -e TRUNKCMS_GITHUB_CLIENT_ID=Iv1.abc \
  -e TRUNKCMS_GITHUB_CLIENT_SECRET=... \
  -e TRUNKCMS_WEBHOOK_SECRET=... \
  -e TRUNKCMS_SESSION_KEY="$(openssl rand -base64 32)" \
  -v "$PWD/secrets:/secrets:ro" \
  nullism/trunkcms
```

4. Visit `/admin` and sign in. Anyone with push access to the repo is an admin. If
   the repo is empty, click **Initialize site**.

See [deploy/kubernetes/trunkcms.yaml](deploy/kubernetes/trunkcms.yaml) for a Kubernetes example.

## Configuration

trunkcms runs in **GitHub mode** unless `TRUNKCMS_CONTENT_DIR` is set, which switches it to
**local mode** and ignores the GitHub settings. Secrets marked † can also be read from a file:
set `NAME_FILE` to the file's path instead of setting `NAME`.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `TRUNKCMS_REPO` | GitHub mode | | Content repo: `owner/name`, `github.com/owner/name`, or a full URL |
| `TRUNKCMS_SITE_URL` | GitHub mode | `http://localhost:$PORT` (local) | The site's URL, e.g. `https://myblog.com`. Used for OAuth callbacks; `https://` makes session cookies Secure |
| `TRUNKCMS_GITHUB_APP_ID` | GitHub mode | | GitHub App ID |
| `TRUNKCMS_GITHUB_PRIVATE_KEY` † | GitHub mode | | App private key (RSA PEM) |
| `TRUNKCMS_GITHUB_CLIENT_ID` | GitHub mode | | App OAuth client ID, for sign-in |
| `TRUNKCMS_GITHUB_CLIENT_SECRET` † | GitHub mode | | App OAuth client secret |
| `TRUNKCMS_SESSION_KEY` † | GitHub mode | random (local) | Base64 key that decodes to 32 bytes; signs session cookies. Comma-separate to rotate (new key first) |
| `TRUNKCMS_WEBHOOK_SECRET` † | Recommended | | Verifies push webhooks. Without it, changes arrive by polling only |
| `TRUNKCMS_REPO_PATH` | | repo root | Subdirectory holding the site, e.g. `blog1` |
| `TRUNKCMS_BRANCH` | | `main` | Branch to serve and commit to |
| `TRUNKCMS_GITHUB_INSTALLATION_ID` | | auto | Looked up from the repo if unset |
| `TRUNKCMS_GITHUB_API_URL` | | `https://api.github.com` | API base URL, for GitHub Enterprise Server |
| `TRUNKCMS_GITHUB_WEB_URL` | | `https://github.com` | Web base URL (OAuth, commit links), for GitHub Enterprise Server |
| `TRUNKCMS_POLL_INTERVAL` | | `30s` (`2s` local) | How often to check for new commits. `0` disables polling |
| `TRUNKCMS_DATA_DIR` | | `$TMPDIR/trunkcms-$PORT` | Scratch space for snapshots and rendered files. trunkcms wipes its `trunkcms/` subdirectory at startup |
| `TRUNKCMS_CONTENT_DIR` | Local mode | | Serve and edit this directory instead of GitHub |
| `TRUNKCMS_DEV_USERS` | | `dev-admin:admin,dev-editor:editor,dev-author:author` | Local mode logins, as `login:role` pairs (`admin`, `editor`, or `author`) |
| `TRUNKCMS_LOG_JSON` | | | Set to any value to log JSON |
| `PORT` | | `8080` | Port to listen on |

## Content repo layout

```
site.yaml                  site settings (admin UI: Settings)
posts/2026-09-30-hello.md  → /posts/hello/
posts/2026-10-01-trip/     bundle: index.md plus images served next to the post
pages/about.md             → /about/
authors/<login>.md         public author profile → /authors/<login>/
assets/                    served as-is at /assets/ (uploads go to assets/uploads/YYYY/MM/)
.trunkcms/users.yaml        roles for people without repo access
themes/<name>/             optional selectable themes (theme.yaml, screenshot.png, templates/*.html, static/*); pick one in Settings
theme/                     optional site-specific overrides, applied on top of the selected theme
```

### Search

Add `search: { enabled: true }` to `site.yaml`, or tick **Search box on the site** in Settings.
New sites start with it on. Each build writes a JSON index of published posts (plus pages,
unless `pages: false`), and the theme's search box downloads it the first time someone
searches. Drafts and scheduled posts are never indexed. The dashboard shows the index size;
expect roughly 150–300 KB gzipped for 300 posts. See [docs/DESIGN.md](docs/DESIGN.md) §4.2
for stopwords, `keep`, `min_kw_length`, and `title_boost` (how strongly title matches
outrank body text).

### Several sites in one repo

Set `TRUNKCMS_REPO_PATH` to serve a subdirectory. For a repo with `blog1/` and `blog2/`,
run one instance per blog with `TRUNKCMS_REPO_PATH=blog1` and `TRUNKCMS_REPO_PATH=blog2`.
Each instance reads and commits only inside its own directory. Things to know:

- Permissions are per repo. Anyone with push access is an admin on every site in it; use
  separate repos if the sites have different editors.
- A push to any site rebuilds all of them (each instance tracks the branch head).
- A GitHub App has one webhook URL, so only one instance can receive pushes directly. The
  others pick up changes by polling unless you fan the webhook out.

## Development

```sh
go test -race ./...
```
