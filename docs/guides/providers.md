# Git providers

Each deploy sets `provider`; the webhook in the git host must send **push**
events, as **JSON**, with the deploy's secret.

| `provider` | `repository` | Where | Secret |
|---|---|---|---|
| `github` (default) | `owner/repo` | Settings → Webhooks → Add webhook, content type `application/json`, "Just the push event" | *Secret* (HMAC, `X-Hub-Signature-256`) |
| `gitea` | `owner/repo` | Settings → Webhooks → Add webhook → Gitea, POST, `application/json`, trigger *Push events* | *Secret* (HMAC, `X-Gitea-Signature`) |
| `forgejo` | `owner/repo` | Settings → Webhooks → Add webhook → Forgejo, same as Gitea | *Secret* (HMAC, `X-Forgejo-Signature`) |
| `gitlab` | `group/subgroup/project` | Settings → Webhooks → Add new webhook, trigger *Push events* | *Secret token* (compared as is, `X-Gitlab-Token`) |
| `bitbucket` (Cloud) | `workspace/repo` | Repository settings → Webhooks → Add webhook, trigger *Repository: Push* | *Secret* (HMAC, `X-Hub-Signature`) |
| `bitbucket` (Data Center) | `PROJECT/repo` (project **key**) | Repository settings → Webhooks → Create webhook, event *Repository: Push* | *Secret* (HMAC, `X-Hub-Signature`) |
| `generic` | optional | anything that can POST JSON | HMAC (`X-Signature`, optional signed timestamp) or token. See [Generic webhooks](generic.md) |

Everything else works the same for all of them: branch filter, deleted
branches ignored, duplicate deliveries dropped, queue, logs, history,
notifications. The script gets `DEPLOY_PROVIDER`. [Wait for CI](wait-for-ci.md)
and [commit statuses](notifications.md#github-commit-statuses) use the GitHub
API, so they are only available with `github`.

## GitHub

Repository → **Settings → Webhooks → Add webhook**:

- Payload URL: `https://your-host/hooks/<deploy>`
- Content type: `application/json`
- Secret: the value of the deploy's `secret_env`
- Events: *Just the push event*

GitHub sends a `ping` when you save it (`200`). **Recent Deliveries** shows
every request with nimdeploy's answer, and *Redeliver* resends one (nimdeploy
ignores duplicates; use `nimdeploy run` to deploy again on purpose).

For private repositories, give the server a **deploy key** (read-only, one per
repository) in Settings → Deploy keys.

## Notes per provider

- **Gitea / Forgejo:** by default they refuse to send webhooks to private or
  loopback addresses. If nimdeploy runs on the same server or network as the
  forge, allow it in the forge's `app.ini`: `[webhook] ALLOWED_HOST_LIST =
  loopback` (or `private`, or the host name). *Test delivery* sends a real
  push of the default branch, so it deploys.
- **GitLab:** the secret token travels as is (over HTTPS), not as a
  signature. Tag pushes are ignored. *Test → Push events* sends a real push
  and deploys.
- **Bitbucket:** set the secret (optional in Bitbucket, required here). One
  push can update several branches; only the deploy's branch counts. Data
  Center's *Test connection* gets `pong`.

Tested against real Gitea 28 and Forgejo 13; GitLab and Bitbucket against
their documented payloads.
