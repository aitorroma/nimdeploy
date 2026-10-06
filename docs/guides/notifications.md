# Notifications and commit statuses

## Chat notifications

```toml
[notify]
format = "slack"            # slack | discord | telegram | json
on = "failure"              # failure | always | never
url_env = "NOTIFY_WEBHOOK_URL"
log_lines = 20
```

`on = "failure"` sends every failure **and the first success after one**
("recovered"), so a broken deploy is never silent and a fixed one tells you.
Failure messages include the last `log_lines` lines of the log.

=== "Slack / Discord"

    Create an incoming webhook in the channel and put its URL in
    `secrets.env`:

    ```bash
    NOTIFY_WEBHOOK_URL=https://hooks.slack.com/services/...
    ```

=== "Telegram"

    ```toml
    [notify]
    format = "telegram"
    on = "failure"
    telegram_token_env = "TELEGRAM_BOT_TOKEN"
    telegram_chat_id = "-1001234567890"
    ```

=== "Any URL (JSON)"

    `format = "json"` POSTs to `url_env`:

    ```json
    {"event": "deploy.finished", "state": { ... }, "log_tail": ["...", "..."]}
    ```

    `state` has the same fields as [`/status`](../reference/endpoints.md).

## GitHub commit statuses

```toml
[github]
token_env = "GITHUB_TOKEN"
```

Marks each pushed commit as pending → success / failure under the context
`nimdeploy/<deploy>`, visible next to the commit and in pull requests. Use a
fine-grained token limited to the repositories, with **Commit statuses: Read
and write**. Pushes that were coalesced and never ran get no status. Set
`commit_status = false` to use the token only for
[wait for CI](wait-for-ci.md).

!!! tip
    Start deploy scripts with `set -euxo pipefail`: each step appears in the
    log (`+ npm ci`) and in the failure message, and the script stops at the
    first error.
