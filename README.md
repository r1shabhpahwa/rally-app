# Badminton RSVP

A small web app that replaces the weekly *"reply-all with your name"* email thread
with a single link. The weekly email stays — it is what reminds people to sign up.
What changes is that replies become a click, and the app's roster becomes the
source of truth instead of the thread.

The organizer never types a confirmed-player list by hand again.

Runs as **one Docker container** with a **SQLite file** and an **SMTP server**.
No external database, no Redis, no job runner. Idles at roughly 25 MB of memory,
which is comfortable on a single-core VM.

---

## The weekly workflow

**Once:** add everyone to the mailing list (type them in, or import a CSV).

**Every week:**

1. **New session** (or **Duplicate last session** — same slot, one week on).
2. **Send invitation.** Everyone on the mailing list gets an email with their
   own signup link.
3. People click **Sign Up**. The app handles capacity, guests and the waitlist.
4. Watch the dashboard. Promote from the waitlist or drop a court if needed.

---

## How the links work

When the invitation goes out, every person on the mailing list gets a row with a
unique 256-bit token. That one link is:

- the **Sign Up** button before they answer, with their name already filled in, and
- the **Manage my RSVP** page afterwards, where they add a guest or cancel.

It never changes, so the same link is reused in the confirmation, the reminder and
the promotion email. Because the link identifies one person, nobody can see or
change anyone else's RSVP, and nobody can sign up twice by accident.

If someone forwards the email to a friend, the friend uses the public link at the
bottom of the session dashboard. They enter a name and email, and get their own
personal link. They are **not** added to the weekly mailing list — the organizer
decides that.

---

## Rules worth knowing

- **Guests take spots and pay.** A player with one guest is two bodies for both
  capacity and the cost split.
- **Parties are all-or-nothing.** A player plus a guest is confirmed together or
  waitlisted together. Nobody is ever half-admitted.
- **Nothing is promoted automatically.** When someone cancels, the organizer gets
  an email and clicks *Promote next*. Promotion confirms the person outright and
  emails them — there is no spot to claim and nothing to time out.
- **Promotion skips a party that does not fit** and says so, rather than silently
  reordering the queue.
- **Signups close by themselves** at the deadline. No job has to run.
- **`Full` and `Completed` are derived**, not stored, so they can never drift out
  of date.
- **Per-player cost is rounded up** to the cent, so the organizer is never short
  at the desk.

---

## Quick start

```bash
cp .env.example .env
# edit .env — APP_BASE_URL, ORGANIZER_*, and SMTP_* at minimum
docker compose up -d --build
```

Then open `APP_BASE_URL` and sign in with `ORGANIZER_PASSWORD`.

The password is hashed into the database on first boot and ignored afterwards, so
it can be removed from the environment once you are in. Change it later under
**Settings**.

Put TLS in front of the container with whatever reverse proxy the VM already runs;
the app speaks plain HTTP on `PORT` and does not terminate TLS itself.

### Try it locally with a mail catcher

```bash
docker compose -f docker-compose.dev.yml up --build
```

App on <http://localhost:8080> (password `badminton123`), and every email it sends
lands in Mailpit at <http://localhost:8025> instead of a real inbox.

---

## Configuration

All configuration is environment variables — see `.env.example`.

| Variable | Notes |
| --- | --- |
| `APP_BASE_URL` | **Required.** Every email link is built from this. The app refuses to start without it. |
| `APP_TIMEZONE` | IANA name, e.g. `America/Vancouver`. All dates and times are entered and shown in this zone. |
| `ORGANIZER_NAME` / `ORGANIZER_EMAIL` | Shown as the sender, and where cancellation notices go. |
| `ORGANIZER_PASSWORD` | Used on first boot only, to create the account. |
| `SMTP_HOST` `SMTP_PORT` `SMTP_USER` `SMTP_PASS` `SMTP_FROM` `SMTP_FROM_NAME` | Outbound mail. |
| `SMTP_TLS` | `starttls` (587), `tls` (implicit, 465), or `none`. |
| `SMTP_RATE_PER_SEC` | Send pacing. Keep at or below your provider's limit. |
| `TRUST_PROXY` | Set only when behind a reverse proxy, so `X-Forwarded-For` can be trusted for rate limiting. |
| `DB_PATH` `BACKUP_DIR` `PORT` `LOG_LEVEL` | Defaults suit the container. |

Bad SMTP settings do **not** stop the app from booting. The dashboard still works
and the **Email log** shows exactly why nothing went out.

---

## Email

Nothing is sent inline from a web request. Messages are rendered, queued in an
outbox table, and delivered by a single background worker that paces sends,
retries transient failures with backoff (1 min → 5 min → 15 min → 1 hr, then
gives up), and never retries a permanently bad address.

The **Email log** page shows every message and its state, with a retry button —
useful the moment somebody says they never got the email.

Six messages exist: invitation, signup confirmation, reminder, promoted from
waitlist, session cancelled, and a cancellation notice to the organizer. Each has
an HTML and a plain-text part, and carries an unsubscribe link and a
`List-Unsubscribe` header.

---

## Backups

The container writes a nightly snapshot to `BACKUP_DIR` (`/data/backups`) using
SQLite's own `VACUUM INTO`, keeping seven days. To restore, stop the container and
copy a snapshot over `DB_PATH`.

```bash
docker compose cp app:/data/backups ./backups   # pull them off the VM
```

---

## Development

```bash
go test ./...        # unit, store, HTTP and email tests
go run ./cmd/server  # needs the same environment variables
```

```
cmd/server        wiring, graceful shutdown, nightly backup
internal/config   environment parsing and validation
internal/model    domain types, capacity, cost, status derivation
internal/store    SQLite, migrations, all queries and transactions
internal/mail     composition, SMTP delivery, outbox worker
internal/web      handlers, middleware, embedded templates and CSS
```

Templates, migrations and static files are embedded in the binary, so the running
container is one file plus a data volume. Every page is server-rendered and every
form is a plain POST-and-redirect: there is no build step, no npm, and the app
works with JavaScript turned off.

Three dependencies: a pure-Go SQLite driver, an SMTP client, and bcrypt.
