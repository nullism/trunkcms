# trunkcms

A small, stateless blog engine. Your content lives in a GitHub repository. Editors sign
in with GitHub, write Markdown in a web UI at `/admin`, and every save is a commit.
Each running container picks up new commits, rebuilds the site in memory, and swaps it
in. There's no database; GitHub is the only external service.

One image serves any blog. There's nothing to build per site: point the container at a
content repo with environment variables and run it.

- Source and full docs: https://github.com/nullism/trunkcms
- Single static Go binary on `distroless/static`, runs as non-root
- Listens on `$PORT` (default `8080`)
- Health checks: `/healthz` (liveness), `/readyz` (ready once the first build succeeds)

## Quick start (no GitHub needed)

Local mode serves and edits a directory on disk, with built-in dev logins
(`dev-admin`, `dev-editor`, `dev-author`). It's the fastest way to try it:

```sh
docker run -p 8080:8080 \
  --user "$(id -u):$(id -g)" \
  -e TRUNKCMS_CONTENT_DIR=/content \
  -v "$PWD/myblog:/content" \
  nullism/trunkcms
```

Open http://localhost:8080 for the site and http://localhost:8080/admin to edit. Saves
are written into `./myblog`. `--user` lets the container write to your mounted directory.

**Minimum:** `TRUNKCMS_CONTENT_DIR` only.

## Run against GitHub

1. Create a content repo (e.g. `you/myblog`). Keep it private so drafts stay hidden. It can be empty.
2. Create a GitHub App:
   - Callback URL: `https://myblog.com/admin/callback`
   - Webhook URL: `https://myblog.com/admin/webhook`, with a random secret
   - Repository permissions: **Contents: read & write**. Subscribe to **Push** events.
   - Generate a private key and a client secret, then install the App on the content repo only.
3. Run the container:

```sh
docker run -p 8080:8080 \
  -e TRUNKCMS_REPO=you/myblog \
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

4. Visit `/admin` and sign in. Anyone with push access to the repo is an admin. If the
   repo is empty, click **Initialize site**.

### Required variables

| Variable | Description |
|---|---|
| `TRUNKCMS_REPO` | Content repo as `owner/name` (a `github.com/...` URL also works) |
| `TRUNKCMS_SITE_URL` | Public URL of the site, e.g. `https://myblog.com`. Used for OAuth callbacks |
| `TRUNKCMS_GITHUB_APP_ID` | GitHub App ID |
| `TRUNKCMS_GITHUB_PRIVATE_KEY` | GitHub App private key (PEM) |
| `TRUNKCMS_GITHUB_CLIENT_ID` | GitHub App client ID |
| `TRUNKCMS_GITHUB_CLIENT_SECRET` | GitHub App client secret |
| `TRUNKCMS_SESSION_KEY` | Base64 key that decodes to 32 bytes. Generate with `openssl rand -base64 32` |

`TRUNKCMS_WEBHOOK_SECRET` is optional but recommended. Without it, changes arrive by
polling only (every 30s by default), not instantly.

Every secret can be read from a file instead by appending `_FILE`, e.g.
`TRUNKCMS_GITHUB_PRIVATE_KEY_FILE=/secrets/app.pem`. Use this with Docker or Kubernetes secrets.

### Optional variables

| Variable | Default | Notes |
|---|---|---|
| `TRUNKCMS_REPO_PATH` | | Subdirectory of the repo holding the site (default: the repo root) |
| `TRUNKCMS_BRANCH` | `main` | Branch to serve and commit to |
| `TRUNKCMS_GITHUB_INSTALLATION_ID` | auto | Looked up from the repo if unset |
| `TRUNKCMS_GITHUB_API_URL` | `https://api.github.com` | API base URL, for GitHub Enterprise Server |
| `TRUNKCMS_GITHUB_WEB_URL` | `https://github.com` | Web base URL (OAuth, commit links), for GitHub Enterprise Server |
| `TRUNKCMS_POLL_INTERVAL` | `30s` | `0` disables polling |
| `TRUNKCMS_DATA_DIR` | `/tmp/trunkcms-$PORT` | Scratch space for snapshots and rendered files. Set this with a writable volume if the root filesystem is read-only |
| `TRUNKCMS_CONTENT_DIR` | | Local mode: serve and edit this directory instead of GitHub |
| `TRUNKCMS_DEV_USERS` | `dev-admin:admin,dev-editor:editor,dev-author:author` | Local mode logins, as `login:role` pairs |
| `TRUNKCMS_LOG_JSON` | | Set to any value to log JSON |
| `PORT` | `8080` | Port to listen on |

To rotate the session key, set `TRUNKCMS_SESSION_KEY` to a comma-separated list with the new key first.

## Deploying

The same image runs on Kubernetes, Cloud Run, ECS/Fargate, and Lambda (with the AWS
Lambda Web Adapter). Instances are stateless, so you can run several replicas. See
[`deploy/kubernetes`](https://github.com/nullism/trunkcms/tree/main/deploy/kubernetes)
for a Kubernetes example.
