# gha-status-exporter

A Prometheus exporter for GitHub Actions. It reports whether the latest run of each workflow failed on the default
branch. It watches every repo of one GitHub user or org, so a Grafana alert can push a message when `main` breaks.

## Why

Existing exporters list org repos only, or report in-progress runs as failures. This one works for a user account and
counts only completed runs.

- A workflow counts as failed while its newest decisive run failed. The next passing run resolves it.
- A decisive run is the newest completed run on the default branch with the conclusion `success`, `failure`,
  `timed_out` or `startup_failure`.
- Runs that end `cancelled`, `neutral`, `skipped`, `stale` or `action_required` are skipped, and the run before
  decides. Many repos set `cancel-in-progress`, so cancelled runs on `main` are routine.
- Queued and in-progress runs never come back from the query, so they never count.
- Forks, archived repos and disabled repos are left out.
- Workflows whose path starts with `dynamic/` are left out. GitHub runs them, not the repo.
- Runs from a fork PR whose branch is named like the default branch are ignored.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gha_workflow_last_run_failed` | gauge | `owner`, `repo`, `branch`, `workflow`, `workflow_name` | 1 if the latest decisive run failed, else 0 |
| `gha_workflow_last_run_timestamp_seconds` | gauge | same | `updated_at` of that run |
| `gha_exporter_build_info` | gauge | `version` | always 1 |
| `gha_exporter_scan_success` | gauge | none | 1 if the last scan read every repo, else 0. Starts at 0 |
| `gha_exporter_last_success_timestamp_seconds` | gauge | none | end of the last scan that read every repo. Absent until the first one |
| `gha_exporter_scan_errors_total` | counter | `reason` | `unauthorized`, `forbidden`, `not_found`, `rate_limited`, `server`, `transport`, `decode` |
| `gha_exporter_repos` | gauge | none | repos in the last scan, after filtering |
| `gha_exporter_rate_limit_remaining` | gauge | none | last `x-ratelimit-remaining` |
| `gha_exporter_scan_duration_seconds` | gauge | none | length of the last scan |

- `workflow` is the file name, such as `build-push.yaml`. `workflow_name` is the display name.
- A workflow with no decisive run in its last 20 completed runs has no series.
- A repo that fails to read keeps its previous series and sets `gha_exporter_scan_success` to 0.
  A repo that leaves the list loses its series.

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `GHA_STATUS_OWNER` | required | user or org whose repos are watched |
| `GHA_STATUS_TOKEN` | required | fine-grained personal access token |
| `GHA_STATUS_SCAN_INTERVAL` | `5m` | time between scan starts |
| `GHA_STATUS_RATE_LIMIT_RESERVE` | `1000` | skip a scan while fewer API requests than this remain |
| `GHA_STATUS_MAX_RUN_AGE` | `0` | drop a workflow whose deciding run is older than this. A Go duration such as `192h`, or whole days such as `8d`. `0` is off |
| `GHA_STATUS_LISTEN` | `:8080` | probes and `/metrics` |
| `GHA_STATUS_LOG_LEVEL` | `info` | zap level |

A workflow that has not run for a long time, for example in a repo whose Actions are disabled, stops alerting once
its deciding run is older than the max age. The exporter then drops both series and does not fall back to an older run.

The process exits at start when the owner or the token is empty, or when a value does not parse.

## Rate limit

- GitHub allows 5000 requests per hour for each user, shared by every token of that user.
- A full scan of about 40 repos costs about 175 requests. The exporter sends `If-None-Match` with the cached ETag
  of each URL. A `304` reply costs nothing.
- The exporter makes at most 4 requests at a time. At 0 requests left it waits for `x-ratelimit-reset`.

## Install

```bash
helm install gha-status-exporter oci://ghcr.io/yama6a/charts/gha-status-exporter \
  --namespace gha-status --create-namespace \
  --set owner=yama6a
```

The pod needs a Secret in its namespace. The pod stays in `CreateContainerConfigError` until the Secret exists.

```bash
kubectl -n gha-status create secret generic gha-status-exporter-token \
  --from-literal=GITHUB_TOKEN="$GITHUB_TOKEN"
```

An empty `owner` renders nothing. `values.yaml` documents every other value.

`maxRunAge` sets `GHA_STATUS_MAX_RUN_AGE`. It defaults to `"0"`, which is off.

### Token

A fine-grained personal access token with these permissions on all repositories of the owner:

- Actions: read
- Metadata: read

The resource owner of the token must be the watched owner. A token for a user lists private repos too.

### NetworkPolicy

The chart ships none. A policy for the pod needs:

- ingress from the metrics scraper on 8080
- egress to DNS
- egress to `api.github.com` on 443

## Release

- Every PR carries exactly one of the labels `patch`, `minor`, `major` or `skip-release`. CI checks this.
- Renovate labels its PRs `patch`. Relabel by hand when a bump changes a flag, env or behaviour.
- A merge to `main` computes the bump from every PR merged since the last release. The strongest label wins.
- The release builds `linux/amd64` and `linux/arm64`, then pushes the image to `ghcr.io/yama6a/gha-status-exporter`.
- It packages the chart at the same version and pushes it to `oci://ghcr.io/yama6a/charts`.
- It creates the git tag last, so every tag has an image and a chart.

## Development

```bash
make ci      # tidy and generate drift, fmt, lint, vet, race tests, govulncheck, chart
make build
make chart   # helm lint, unit tests, render
make image   # multi-arch, no push
```

The tests run against an `httptest` fake of the GitHub API.

## License

MIT.
