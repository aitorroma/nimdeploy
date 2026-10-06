# Wait for CI

Deploy only commits whose GitHub Actions workflows passed:

```toml
[github]
token_env = "GITHUB_TOKEN"

[deploy.shop]
# ...
wait_for_ci = ["linter", "tests"]   # workflow names, as shown in the Actions tab
ci_timeout = "30m"
```

A push no longer deploys right away: the deploy shows as `waiting` and
nimdeploy asks GitHub every 15 s about those workflows for the pushed commit.

| Outcome | What happens |
|---|---|
| all passed (or `skipped` / `neutral`) | it deploys |
| one failed, was cancelled or timed out | nothing runs; the deploy ends as `skipped` with the reason (`CI failed: linter=success tests=failure`), the commit gets a failure status and `[notify]` sends a "NOT deployed" message |
| not finished within `ci_timeout` | `skipped`. Also when a listed workflow never starts (e.g. `paths:` filters): list only workflows that run on every push to the branch |
| another push arrives while waiting | the waiting one ends as `skipped` (superseded, no status or notification) and the newest push waits for its own CI |
| `nimdeploy run` (manual) | doesn't wait |

The log shows each change: `ci: linter=success tests=in_progress`, then
`ci: passed after 2m10s, deploying`.

## Token

The token needs **Actions: read** on the repository, plus **Commit statuses:
write** if you also want [commit statuses](notifications.md#github-commit-statuses)
(otherwise set `commit_status = false`). Fine-grained tokens only reach
repositories owned by you or your organizations: for a repository of another
personal account, its owner creates the token, or use a classic token with
`repo` scope. For GitHub Enterprise Server set `api_url` in `[github]`.

`wait_for_ci` uses the GitHub API, so it is only available with
`provider = "github"`.
