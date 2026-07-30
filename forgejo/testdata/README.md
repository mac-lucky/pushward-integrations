# Forgejo API fixtures

Captured from a live Forgejo **16.0.1+gitea-1.22.0** instance, then scrubbed:
hostnames, owner/repo names and commit SHAs are replaced, the multi-KB
`event_payload` string is dropped, and the embedded `repository` object is cut
down to the fields the bridge reads. Everything structural is untouched, because
the structure is the point - several of these shapes are traps.

| File | What it pins down |
|---|---|
| `runs_active_empty.json` | The idle probe's response. 37 bytes on the wire, and `workflow_runs` is **null**, not `[]` |
| `run_success.json` | A terminal run: `id` != `index_in_repo`, no `conclusion` key, `duration` in ns, `workflow_id` a filename |
| `run_failure.json` | The same for `status: "failure"` |
| `run_dispatch_empty_event.json` | `event` is `""` while `trigger_event` says `workflow_dispatch` |
| `jobs_matrix.json` | A bare array: `needs` both null and populated, three matrix legs of `tofu`, mixed `success`/`running`/`waiting` |
| `tasks_page.json` | The tasks envelope, whose rows are per-**job**; includes one epoch-stamped unstarted task |
| `repos_page.json` | `/user/repos` with an archived and an empty repo to filter out |
| `orgs_repos_403.json` | The scope error `/orgs/{org}/repos` returns, which discovery must tolerate |
| `user.json`, `version.json` | Token owner and instance version |

## What these exist to prevent

- **`html_url` is built from `index_in_repo`, not `id`.** Run `id: 39` lives at
  `.../actions/runs/33`. Constructing the URL from `id` points at a different
  run. Always use the API's own `html_url`.
- **There is no `conclusion` field.** One `status` enum carries everything:
  `unknown, waiting, running, success, failure, cancelled, skipped, blocked`.
  GitHub's status/conclusion split is synthesised at the client boundary.
- **`workflow_id` is a filename string** (`"tofu.yml"`), not an int, and there is
  no workflow display name anywhere in the API.
- **Jobs carry no timestamps and no conclusion.** Timing is only reachable by
  joining `job.task_id` to a task's `id`.
- **The tasks envelope is keyed `workflow_runs` but holds per-job rows.** Decoding
  it as a runs page silently yields garbage.
- **Unset times serialise as the unix epoch**, not null. Treated literally, an
  unstarted task yields a 55-year duration; `tasks_page.json` carries one so the
  floor is exercised.
- **`prettyref` is the bare branch** (`master`) while the `ref` query filter needs
  the full ref (`refs/heads/master`). The bare form returns zero rows.
- **On `/actions/tasks`, `limit` alone is ignored** and the endpoint returns every
  row; `page` and `limit` must be sent together.
