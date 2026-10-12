# corruption-tracker

A review-gated tracker for New Zealand fraud, bribery and corruption
enforcement matters. It polls agency media releases (SFO, IRD, Commerce
Commission, MSD, Police, ACC, MBIE, DIA), classifies news coverage and
licensed alert emails, and routes everything into a human review queue.
Only reviewer-verified cases reach the case register, the explorer and the
periodic reports, and only under fixed naming and suppression rules.

> **Status: pre-release.** The code is being split out of an existing
> service, where it runs today as an embedded module. The standalone
> server, setup wizard and `docker compose` stack work, but no image is
> published yet, so `docker compose up` builds one from this checkout.
> Treat it as a preview, not as a way to run a production tracker.

## Run it

New to servers? The [operator guide](docs/operator-guide.md) goes from a
blank VPS to a working tracker step by step, with screenshots and a
[sample dataset](docs/sample/sample-bundle.tar.gz) to try it on. It also
covers backups, restores, upgrades, handoffs and troubleshooting.

With Docker (Compose v2.23 or later):

```sh
echo "POSTGRES_PASSWORD=$(openssl rand -hex 24)" > .env
docker compose up -d
```

Then open <http://localhost:8080> and create the first admin account. The
site name, LLM key, alert mailbox and news feeds are all set on the
admin Settings page; nothing else goes in `.env`. On a server with a
domain name, add `TRACKER_DOMAIN=…` and `COMPOSE_PROFILES=https` to `.env`
for HTTPS through Caddy with an automatic Let's Encrypt certificate.
`.env.example` lists the options.

Data lives in the `db` and `data` volumes. `docker compose down` keeps
them; `down -v` deletes them. The `data` volume holds the key that
encrypts stored secrets. `scripts/compose-smoke.sh` brings a throwaway
copy of the stack up and checks setup, the tracker page and a restart.

### Backups, upgrades and restore

A backup is taken every night at 03:00, and before any upgrade that
changes the database. Admins can also take one, and download any of them,
on the Backups page. Each backup holds a full database dump and an export
bundle. Backups are kept on the `data` volume (the newest per day for 14
days, per week for 8 weeks), so download copies to keep elsewhere.

To upgrade: `git pull && docker compose up -d --build`. No image is
published yet, so compose builds one from your checkout. Once images are
published, the upgrade becomes
`docker compose pull && docker compose up -d`.

To restore a backup into an empty stack:

```sh
docker compose up -d db
docker compose run --rm -v "$PWD/20261012T030000Z-nightly:/restore:ro" tracker restore /restore
docker compose up -d
```

Encrypted settings (the LLM key, the mailbox password) come back only
if the restored server has the original data volume's `secret.key`, or
the same `TRACKER_SECRET`. Otherwise, enter them again on the Settings
page. `scripts/compose-upgrade-test.sh` checks the upgrade and restore
path end to end.

## What it is, and what it is not

- **It is** a research and diligence tool for organisations that have
  agreed to handle the data responsibly. Every surface sits behind a login,
  and nothing is published automatically.
- **It is not** a public "name and shame" site. A new tracker shows
  nobody's name until an admin turns naming on, and confirms that everyone
  with access has agreed to check suppression before publishing. Even
  then, pre-verdict matters are always labelled **ALLEGED** and shown
  without names, unless the prosecuting agency itself published the names.
  Any suppression signal quarantines a claim before a case can exist, and
  a single flagged participant withholds names for the whole case.
- **It never** scrapes judgment databases (Judicial Decisions Online,
  NZLII). Reviewers check those by hand, in their own browser.

Read [DATA-ETHICS.md](DATA-ETHICS.md) before you run this against real
data. It sets out the rules the code enforces and the responsibilities the
code cannot take on for you.

## Layout

| Path | What it holds |
|---|---|
| `domain/` | Case, claim, participant and review types |
| `store/` | Postgres storage; `store/storetest` gives each test a private schema |
| `migrations/` | The tracker's own migration track (`tracker_schema_migrations`) |
| `ingest/fraudagency/` | Agency feed poller, news classifier, alert-mail ingest |
| `ingest/fraudsheet/` | One-off importer for a normalised research spreadsheet |
| `fraudtracker/`, `fraudrecode/` | Status derivation, matching, taxonomy coding backfill |
| `fraudreport/`, `readmodel/` | Report snapshots and the naming/suppression read model |
| `web/` | Admin, review queue, explorer and reader handlers + templates |
| `standalone/` | Local accounts, roles and access log for the standalone server |
| `settings/` | Standalone settings (DB-stored, secrets encrypted) behind the admin Settings page |
| `ingest/newsfeed/` | RSS/Atom feeds as the standalone news corpus for classify-news |
| `llm/` | LLM clients: Anthropic and OpenAI-compatible |
| `Dockerfile`, `docker-compose.yml`, `scripts/` | Image, compose stack and its smoke test |
| `bundle/` | Versioned export/import bundles for handing a dataset to another operator |
| `backup/` | Nightly and pre-upgrade backups (pg_dump + bundle), retention and restore |
| `scheduler/`, `jobs/`, `cli/` | In-process job scheduler and command dispatch |
| `tools/` | Developer tools (local dev seeding, mailbox peek, spreadsheet normaliser) |

## Development

Requirements: Go (see `go.mod`) and a throwaway PostgreSQL 16 for the
database-backed tests.

```sh
go vet ./...
go build ./...
go test ./...            # database tests skip when DATABASE_URL_TEST is unset
```

To run the database-backed tests, point `DATABASE_URL_TEST` at a database
that is **only** for tests and mark it as such. The harness refuses any
database whose comment doesn't end in `test-db`:

```sh
docker run -d --rm --name tracker-test-pg -p 127.0.0.1:55432:5432 \
  -e POSTGRES_USER=tracker -e POSTGRES_PASSWORD=tracker -e POSTGRES_DB=tracker \
  --tmpfs /var/lib/postgresql/data postgres:16-alpine
psql postgres://tracker:tracker@localhost:55432/tracker \
  -c "COMMENT ON DATABASE tracker IS 'tracker-test-db';"
DATABASE_URL_TEST=postgres://tracker:tracker@localhost:55432/tracker?sslmode=disable go test ./...
```

Each test gets its own schema, so packages can run in parallel.

Code comments cite the design notes by section ("design §10.4"). Those notes
are kept with the project's first embedding host and aren't published here
yet; the code and tests are the reference for behaviour.

## Licence

GNU Affero General Public License v3.0. See [LICENSE](LICENSE). If you run a
modified version as a network service, the AGPL requires you to offer its
source to the people who use it.

Contributions: see [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: see
[SECURITY.md](SECURITY.md).
