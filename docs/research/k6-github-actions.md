# Running a Go server, Postgres and k6 in GitHub Actions

Research for Linear DWX-279 (map DWX-276 "Event store load test via Connect and
k6"). Scope: one `workflow_dispatch` job that starts Postgres, applies the
schema, starts a Go load-test server, drives it with k6 and reports results.
It does not gate anything.

Sources were checked on 2026-10-18. Versions and SHAs are as of that date.

## Answer

Use one job on `ubuntu-latest`:

1. Run Postgres as a `services:` container. The image and server flags can come
   from `workflow_dispatch` inputs.
2. Apply `docs/postgres/schema.sql` with `psql` after `actions/checkout`.
3. Build and start the Go server in the background, then wait until it is ready.
4. Install k6 with `grafana/setup-k6-action` and pin the k6 version. Then run
   `k6 run` directly. `grafana/run-k6-action` adds little without Grafana Cloud.
5. Write the result to `$GITHUB_STEP_SUMMARY` from `handleSummary()`. Upload the
   web dashboard HTML report, the summary JSON and (optionally) the raw metrics
   as artifacts.

Treat the numbers as indicative only. A public repo gets 4 vCPU and 16 GB on
`ubuntu-latest`, and the server, Postgres and k6 all share them. Hosted runners
are shared VMs, so repeated runs vary noticeably. To compare two versions, run
both in the same job.

## Findings

### 1. Official Grafana k6 actions

| Action | Latest | Commit SHA (tag) | Runtime |
|---|---|---|---|
| `grafana/setup-k6-action` | v1.2.2 (2026-09-22) | `43b9fc21641a76002687994433dd586f56e791b1` | `node24` |
| `grafana/run-k6-action` | v1.5.0 (2026-09-22) | `4082a08c7e40cf6d65863911077dfb60ae0a4f04` | `node24` |

The latest k6 is **v2.3.0** (2026-09-21). v1.8.2 (2026-10-12) is a maintenance
release on the 1.x line. Sources: GitHub releases API for
`grafana/setup-k6-action`, `grafana/run-k6-action` and `grafana/k6`.

**`setup-k6-action`** installs k6 and can also install Chrome for browser tests.
Its inputs (from `action.yaml` at v1.2.2) are:

- `k6-version`: the version to install. If it is unset, the action installs the
  latest k6. Pin it, because otherwise a k6 major release can silently change
  results.
- `browser`: also installs Chrome.
- `github-token`: defaults to `github.token` and is used for GitHub API lookups.

It has one output, `k6-version`.

**`run-k6-action`** wraps `k6 run`. It accepts globs, can run several scripts in
parallel and can stop on the first failure. It does not install k6. Its inputs
(from `action.yaml` at v1.5.0) are:

| Input | Default | Purpose |
|---|---|---|
| `path` (required) | | Glob(s) of test scripts. Separate several with newlines. |
| `flags` | `""` | Extra `k6 run` flags. |
| `inspect-flags` | `""` | Flags for the `k6 inspect` validation step. |
| `parallel` | `false` | Run several scripts concurrently. |
| `fail-fast` | `false` | Stop after the first failing script. |
| `cloud-run-locally` | `true` | With a cloud token, run locally and stream results to Grafana Cloud k6. |
| `cloud-comment-on-pr` | `true` | Post the cloud run URL and metrics as a PR comment. |
| `only-verify-scripts` | `false` | Only run `k6 inspect`. |
| `debug` | `false` | Show the full k6 output. Otherwise only the summary is shown. |
| `disable-analytics` | `false` | Turn off anonymous usage analytics. |
| `github-token` | `github.token` | Used for PR comments. |

Its one output, `testRunIds`, only exists for Grafana Cloud runs.

From the v1.5.0 source (`src/index.ts`, `src/k6helper.ts`, `src/githubHelper.ts`):

- The action first runs `k6 inspect --execution-requirements` on each script.
  It then spawns `k6 run --address= <flags> <script>`. If `K6_CLOUD_TOKEN` is
  set, it spawns `k6 cloud run [--local-execution]` instead.
- The step fails when any k6 process exits with a non-zero code, for example
  because a threshold was crossed.
- The action does **not** write to `$GITHUB_STEP_SUMMARY`. Its Markdown metrics
  rendering is only used for the Grafana Cloud PR comment.
- k6 v2 needs a cloud stack (`K6_CLOUD_STACK_ID`) for every `k6 cloud` command.
  The README says the action leaves this check to k6.

**Conclusion:** without Grafana Cloud, `run-k6-action` adds a script check and
glob handling around a single `k6 run`. Running `k6 run` in a plain `run:` step
is just as short and keeps all flags visible. `setup-k6-action` is still worth
using because it pins and caches the binary.

### 2. Postgres: service container, testcontainers or `docker run`

The runner image (Ubuntu 24.04, image version 20261012) ships Docker 28.0.4 and
a PostgreSQL 16.15 client and server. The server is disabled by default. Source:
`actions/runner-images` `images/ubuntu/Ubuntu2404-Readme.md`. So `psql` is
available without any install step.

| Option | How it works | Pros | Cons |
|---|---|---|---|
| `services:` container | The runner starts the container before the first step and creates a Docker network for it. Port `5432:5432` is reachable at `localhost`. A `--health-cmd pg_isready` in `options` makes the runner wait until Postgres is ready. | Declarative, and the runner handles start and teardown. `image` can use `inputs` and `matrix` contexts. `command` passes server flags such as `-c shared_buffers=...`. | The service starts before checkout, so a bind mount from the repo into `/docker-entrypoint-initdb.d` would be empty. The schema has to be applied in a step. `--network` is not allowed in `options`. Linux runners only. |
| testcontainers-go (as in `internal/pgtest`) | The Go process starts Postgres itself through Docker. | Same setup locally and in CI. `pgtest` already applies `schema.sql`. | `internal/pgtest` is built for `testing.M` and template databases, not for a long-running server. The load-test binary would carry a testcontainers dependency (Ryuk, Docker socket). The container lifetime is tied to the server process. |
| `docker run` in a step | A shell step starts the container, waits for `pg_isready` and runs `psql`. | Full control over flags, `--tmpfs`, `--cpus` and `--shm-size`. Runs exactly the same way locally. | Readiness polling and cleanup have to be written by hand (cleanup matters less on ephemeral runners). |

Sources: docs.github.com "Creating PostgreSQL service containers" (the
`localhost` hostname, the health-check example and the Linux-only rule).
Workflow syntax: `jobs.<job_id>.services`, `services.<id>.options` (the
`--network` warning) and `services.<id>.command`, which overrides `CMD` and
passes arguments to `docker create`. The contexts reference: `services` accepts
`github, needs, strategy, matrix, vars, inputs`.

**Schema migration.** The library ships `docs/postgres/schema.sql` as
documentation and never runs DDL itself (ADR-0003). In CI, apply it with
`psql -v ON_ERROR_STOP=1 -f docs/postgres/schema.sql` after checkout. The file
already rejects PostgreSQL versions below 15. The preinstalled psql 16 client
can run a plain SQL file against a PostgreSQL 18 server. Alternatively, the
load-test server can apply the file on start-up, like `pgtest.applySQLFile`.
That keeps one code path but puts DDL into a binary. A `psql` step keeps the
"library never runs DDL" rule visible.

**Recommendation:** use a `services:` container plus a `psql` step. Switch to
`docker run` only if we need flags that `services` cannot express, such as
`--tmpfs` for the data directory. `docker create` options in `options` may
already cover most of those.

### 3. Runner resources and run-to-run variance

Standard GitHub-hosted runners, from docs.github.com "GitHub-hosted runners":

| Repository | CPU | RAM | SSD |
|---|---|---|---|
| Public (deepworx/go-utils is public) | 4 | 16 GB | 14 GB |
| Private | 2 | 8 GB | 14 GB |

`ubuntu-latest` is listed together with `ubuntu-24.04`. "Each GitHub-hosted
runner is a new virtual machine (VM) hosted by GitHub." The docs make no claim
about consistent performance.

**Larger runners** (docs.github.com "Larger runners") range from 2 vCPU / 8 GB up
to 96 vCPU / 384 GB on x64 Linux, for example 8 vCPU / 32 GB / 300 GB. They need
a GitHub Team or Enterprise Cloud plan and a spending limit above zero. Larger
runner pools are smaller, so jobs can wait in the queue while a VM starts.

**Variance.** GitHub publishes nothing about this, so the evidence is secondary:

- Quansight Labs (2021) benchmarked two identical commits every 6 hours for 16
  days. The ratios ranged from 0.51 to 1.36 (standard deviation 0.05). They
  recommend running the baseline and the candidate in the same job. The cause
  they name is other workloads sharing the virtualized host.
- CodSpeed reports a coefficient of variation of about 2.7% across 100 runs on
  hosted runners and blames, among other things, the mix of CPU types. CodSpeed
  sells benchmark runners, so this source has a commercial interest.

Implication for us: k6, the Go server and Postgres compete for the same 4 vCPU.
The absolute throughput figures describe this co-located setup, not a
production deployment. For trend tracking, either compare A and B in one job
(for example `main` and the branch, run one after the other) or keep the
raw numbers only as a sanity check. Record `nproc`, `lscpu` model name and the
runner image version in the summary so outliers can be explained.

### 4. Results in the job summary and as artifacts

**Job summary** (docs.github.com "Workflow commands", Adding a job summary):
append GitHub-flavoured Markdown to the file in `$GITHUB_STEP_SUMMARY`. Each step
may write at most **1 MiB**. A larger upload fails with an error annotation but
does not fail the step. A job shows at most **20** step summaries.

**`handleSummary(data)`** (grafana.com/docs/k6 "Custom summary"): if the script
exports `handleSummary`, k6 calls it at the end and does **not** print the
default summary. It returns a map from `stdout`, `stderr` or any file path to
the content, and existing files are overwritten. `data` contains `metrics` (each
with `type`, `contains`, `values` and optionally `thresholds`), `root_group`,
`options` and `state`. `textSummary` is not built in. Import it from
`https://jslib.k6.io/k6-summary/0.0.2/index.js`. Since k6 v2.2.0 the callback
time budget (previously fixed at 120 s) can be set with the
`handleSummaryTimeout` option or `K6_HANDLE_SUMMARY_TIMEOUT`.

A k6 script cannot read environment variables of the runner unless they are
passed through `__ENV`. The usual pattern is to return a Markdown file path from
`handleSummary` (for example `summary.md`) and add a step that runs
`cat summary.md >> "$GITHUB_STEP_SUMMARY"`. Writing directly to
`__ENV.GITHUB_STEP_SUMMARY` also works, because a returned path overwrites the
file. Since the file is step-scoped and starts empty, overwriting is harmless.

**Summary export / modes** (k6 docs "End of test" and k6 v2.0.0 release notes):
`--summary-mode` takes `compact` (the default), `full` or `disabled`. k6 v2.0.0
removed `--no-summary` (use `--summary-mode=disabled`) and the `legacy` mode.
`--summary-export=<file>` still exists in v2.3.0
(`internal/cmd/runtime_options.go`). Together with
`--new-machine-readable-summary`, both the export and the `handleSummary` data
use the newer structured format introduced in k6 v1.5.0. That flag is still
opt-in in v2.3.0.

**Web dashboard HTML report** (k6 docs "Web dashboard"): the dashboard is built
into k6. In v2.0.0 xk6-dashboard was moved into `internal/dashboard`.

- `K6_WEB_DASHBOARD=true` turns it on.
- `K6_WEB_DASHBOARD_EXPORT=report.html` writes a self-contained HTML report at
  the end of the run.
- `K6_WEB_DASHBOARD_PERIOD` sets the update interval (default `10s`). The report
  only contains graphs if the test runs longer than 3× that period.
- In CI, set `K6_WEB_DASHBOARD_PORT=-1` so k6 does not wait for an open browser
  and exits.

**Raw metrics** (k6 docs "JSON" and "CSV"): `--out json=results.json` writes one
JSON object per line (`Metric` or `Point`). `--out csv=results.csv` writes
columns such as `metric_name`, `timestamp`, `metric_value`, `status` and
`scenario`. A `.gz` file name enables gzip for both. Raw output grows with the
request rate, so gzip it.

**Artifacts** (`actions/upload-artifact` README; the repo pins v7.0.2 at
`cf430e030ddbb5b0abf93d22962f4752f3646cd9`): `archive: false` uploads a single
file without zipping it, and the artifact is named after the file. Use this for
`report.html` so the report can be downloaded and opened directly. Bundle the
JSON summary and the raw metrics as a normal zipped artifact. Artifact storage
counts against the plan quota (500 MB on Free), so set `retention-days`.

### 5. Job time limits

- A job on a GitHub-hosted runner may run for at most **6 hours**. Support
  cannot raise this. Source: docs.github.com "Actions limits".
- `jobs.<id>.timeout-minutes` defaults to **360**. Step-level `timeout-minutes`
  is at most 360. Source: workflow syntax.
- A workflow run may last at most 35 days, including waiting time.
- `workflow_dispatch` accepts at most 25 inputs of type `boolean`, `choice`,
  `number`, `environment` or `string`.

Set an explicit `timeout-minutes` (for example test duration + 15) so a hung
server cannot use up 6 hours of runner time.

## Sketch

Untested and for orientation only. The SHAs are the tags listed above.

```yaml
name: Load test
on:
  workflow_dispatch:
    inputs:
      pg-image: { type: choice, options: ["postgres:18", "postgres:15"], default: "postgres:18" }
      duration: { type: string, default: "5m" }

jobs:
  k6:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    services:
      postgres:
        image: ${{ inputs.pg-image }}
        env: { POSTGRES_PASSWORD: postgres }
        ports: ["5432:5432"]
        options: >-
          --health-cmd pg_isready --health-interval 5s
          --health-timeout 5s --health-retries 10
    env:
      DATABASE_URL: postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with: { go-version: "1.27" }
      - name: Apply schema
        run: psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f docs/postgres/schema.sql
      - name: Start server
        run: |
          go build -o loadtest ./cmd/loadtest   # path to be decided in DWX-276
          ./loadtest > server.log 2>&1 &
          timeout 30 bash -c 'until curl -fs localhost:8080/healthz; do sleep 1; done'
      - uses: grafana/setup-k6-action@43b9fc21641a76002687994433dd586f56e791b1 # v1.2.2
        with: { k6-version: "2.3.0" }
      - name: Run k6
        env:
          K6_WEB_DASHBOARD: "true"
          K6_WEB_DASHBOARD_PORT: "-1"
          K6_WEB_DASHBOARD_EXPORT: report.html
        run: k6 run -e DURATION=${{ inputs.duration }} --summary-export=summary.json --out json=metrics.json.gz loadtest/script.js
      - name: Job summary  # summary.md is returned by handleSummary() in script.js
        if: always()
        run: |
          { echo "Runner: $(nproc) vCPU, $(lscpu | sed -n 's/^Model name: *//p'), image $ImageVersion"; cat summary.md; } >> "$GITHUB_STEP_SUMMARY"
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        if: always()
        with: { path: report.html, archive: false, retention-days: 14 }
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        if: always()
        with:
          name: k6-raw
          path: |
            summary.json
            metrics.json.gz
            server.log
          retention-days: 14
```

## Open points for DWX-276

- Where the load-test server lives (`cmd/`, `internal/`, or a separate module) and
  whether it should depend on testcontainers. This research suggests it should
  not.
- Whether to record a same-job A/B baseline or only absolute numbers.
- Whether to tune Postgres flags through `services.<id>.command`, for example
  `-c max_connections` and `-c synchronous_commit`, and record them in the summary.

## Sources

- https://github.com/grafana/setup-k6-action (`action.yaml`, README, release v1.2.2)
- https://github.com/grafana/run-k6-action (`action.yaml`, README, `src/`, release v1.5.0)
- https://github.com/grafana/k6/releases/tag/v2.0.0, v2.2.0 and v2.3.0; `internal/cmd/runtime_options.go`
- https://grafana.com/docs/k6/latest/results-output/end-of-test/custom-summary/
- https://grafana.com/docs/k6/latest/results-output/end-of-test/
- https://grafana.com/docs/k6/latest/results-output/web-dashboard/
- https://grafana.com/docs/k6/latest/results-output/real-time/json/
- https://grafana.com/docs/k6/latest/results-output/real-time/csv/
- https://docs.github.com/en/actions/reference/runners/github-hosted-runners
- https://docs.github.com/en/actions/reference/runners/larger-runners
- https://docs.github.com/en/actions/reference/limits
- https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax
- https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-commands
- https://docs.github.com/en/actions/reference/workflows-and-actions/contexts
- https://docs.github.com/en/actions/tutorials/use-containerized-services/create-postgresql-service-containers
- https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2404-Readme.md
- https://github.com/actions/upload-artifact (README, `archive` input)
- Secondary: https://dev.to/quansightlabs/is-github-actions-suitable-for-running-benchmarks-111h (Quansight Labs, 2021); https://codspeed.io/blog/benchmarks-in-ci-without-noise (vendor)
