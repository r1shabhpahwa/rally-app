# Deployment

The image is built by GitHub Actions and pulled from GHCR. Nothing is built on
the server.

```
push to main
   -> test        gofmt, go vet, go test
   -> build       docker build, push ghcr.io/r1shabhpahwa/rally-app:<sha> and :latest
   -> deploy      ssh to the box, pull the <sha> tag, compose up, wait for /healthz
```

## The box

`192.210.196.249` also runs an unrelated app (jobpilot) which owns the Caddy
reverse proxy on :80/:443. This app deliberately publishes **no ports**: it
joins the `jobpilot_default` compose network under the alias `badminton`, and
Caddy is the only route in.

That coupling is the price of sharing one proxy. If jobpilot is ever fully
`docker compose down`ed, Docker refuses to remove a network that still has this
container attached, so it degrades to a warning rather than an outage.

The `badminton.bitcrate.cc` site block lives in the **jobpilot repo's**
Caddyfile, because that repo's deploy copies `Caddyfile` to the box and would
otherwise overwrite anything added here. The deploy job re-adds the block if it
ever goes missing, validates the config, and only then reloads Caddy — and rolls
the file back if validation fails, so a mistake here cannot take the other app
down.

## Required GitHub secrets

Settings → Secrets and variables → Actions.

Until `DEPLOY_HOST` is set, the deploy job skips itself with a warning and the
build still publishes the image.

| Secret | Value |
| --- | --- |
| `DEPLOY_HOST` | `192.210.196.249` |
| `DEPLOY_USER` | `deploy` |
| `DEPLOY_SSH_KEY` | Private half of the `github-actions@badminton` key |
| `DEPLOY_SSH_KNOWN_HOSTS` | `ssh-keyscan -t ed25519 192.210.196.249` output |
| `APP_BASE_URL` | `https://badminton.bitcrate.cc` |
| `APP_TIMEZONE` | e.g. `America/Vancouver` |
| `ORGANIZER_NAME` | Organizer's display name |
| `ORGANIZER_EMAIL` | Organizer's email |
| `ORGANIZER_PASSWORD` | First-boot login password |
| `SMTP_HOST` `SMTP_PORT` `SMTP_USER` `SMTP_PASS` | Mail server |
| `SMTP_FROM` `SMTP_FROM_NAME` | Sender identity |
| `SMTP_TLS` | `starttls`, `tls`, or `none` |
| `SMTP_RATE_PER_SEC` | e.g. `1` |

`TRUST_PROXY=true` and `LOG_LEVEL=info` are set by the workflow, not by secrets.

## Operating it

```bash
ssh deploy@192.210.196.249
cd /srv/badminton
docker compose --project-name badminton ps
docker compose --project-name badminton logs -f
```

Roll back by re-running the deploy workflow on an older commit, or by hand:

```bash
IMAGE_TAG=<older-sha> docker compose --project-name badminton up -d
```

Data lives in the `badminton_data` volume at `/data`, with nightly `VACUUM INTO`
snapshots in `/data/backups` kept for seven days.
