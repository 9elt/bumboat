# Bumboat

Push-to-deploy for one Linux server, managed from a vim-style TUI over SSH. Add as many services as you like, each a GitHub repo/branch or a Docker image with its own domains; every push deploys with no downtime, over HTTPS.

One Go binary, no web UI: the daemon is the reverse proxy, the webhook receiver and the build queue.

## Install

Needs Docker with buildx, git, and free ports 80 and 443.

```
curl -fsSL https://raw.githubusercontent.com/9elt/bumboat/main/install.sh | sudo sh
```

Log out and back in, then run `bumboat`.

## Use

- Connect GitHub once: `:settings`, `<space>g`, open the printed link, install the app on the repos you want.
- Add a service with `o`: a repo and branch (Dockerfile or Railpack) or an image.
- `?` shows the keys. Edits are drafts until `:w`; `<space>d` deploys.

```
bumboat ls | deploy <svc> | logs <svc> [-f] [--build] | backup | upgrade
```

State: `/var/lib/bumboat` and `/etc/bumboat/master.key`.

## Develop

```
buf generate        # after editing proto/
go test ./...
scripts/e2e.sh      # end to end against the local Docker
scripts/release.sh v0.1.0   # tag + push; GitHub Actions publishes the release
```
