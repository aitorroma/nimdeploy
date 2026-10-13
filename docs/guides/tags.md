# Production from tags (releases)

By default a git deploy runs on every push to its branch. For production you
may want every deploy to be a version instead: commits land on `main`
without reaching the servers, and production changes only when someone
creates a tag such as `v1.2.0`, or publishes a release with its notes. Give
the deploy a `tags` pattern:

```toml
[deploy.shop-stage]                 # stage: every push to the branch
path = "/hooks/shop-stage"
repository = "acme/shop"
branch = "stage"
secret_env = "SHOP_WEBHOOK_SECRET"
command = "/home/deploy/bin/deploy-shop-stage.sh"

[deploy.shop-prod]                  # production: only version tags
path = "/hooks/shop-prod"
repository = "acme/shop"
branch = "main"
tags = "v*"
secret_env = "SHOP_WEBHOOK_SECRET"
command = "/home/deploy/bin/deploy-shop-prod.sh"
```

With `tags` set:

- Pushes to branches are ignored (`ref refs/heads/main is not a tag matching v*`).
- A new tag matching the pattern deploys the commit it points to. Annotated
  tags work too: nimdeploy deploys the tagged commit, not the tag object.
- Deleting a tag deploys nothing.
- `branch` is still the branch the script fetches (`DEPLOY_BRANCH`), so tag
  commits on it.

The pattern uses `*`, `?` and `[...]`: `v*`, `v[0-9]*`, `release-*`. It is
matched against the tag's name, without `refs/tags/`.

## Releasing a version

Either way triggers the deploy:

```bash
git tag -a v1.2.0 -m "v1.2.0"
git push origin v1.2.0
```

or, on GitHub, **Releases → Draft a new release**, choose a new tag
(`v1.2.0`) on `main`, write the notes and publish: publishing creates the tag,
and the tag push reaches nimdeploy. GitLab (**Deploy → Releases**, or
**Code → Tags**), Bitbucket and Gitea/Forgejo work the same way.

## The webhook

Nothing changes on the git host: keep the **push** event, which covers tags
as well as branches; nimdeploy picks what applies to each deploy. The one
exception is GitLab, which sends tags in a separate event: tick **Tag push
events** as well as **Push events**.

Both deploys above can share one webhook URL and secret if they use the same
`path`: nimdeploy runs the one whose branch or tags match (see
[several deploys on one path](generic.md#several-deploys-on-one-path)).

## What the script gets

| Variable | Example |
|---|---|
| `DEPLOY_TAG` | `v1.2.0` (empty for branch deploys) |
| `DEPLOY_REF` | `refs/tags/v1.2.0` |
| `DEPLOY_COMMIT` | the commit the tag points to |
| `DEPLOY_BRANCH` | `main` (from the config) |

The example scripts ([Nuxt](nuxt.md), [Laravel](laravel.md), [pm2](pm2.md))
handle it: with `DEPLOY_TAG` they also fetch the tag and deploy it. In your
own script:

```bash
git fetch origin "+refs/heads/$DEPLOY_BRANCH:refs/remotes/origin/$DEPLOY_BRANCH"
TARGET=${DEPLOY_COMMIT:-origin/$DEPLOY_BRANCH}
if [[ -n "${DEPLOY_TAG:-}" ]]; then
	git fetch origin "+refs/tags/$DEPLOY_TAG:refs/tags/$DEPLOY_TAG"
	TARGET=${DEPLOY_COMMIT:-refs/tags/$DEPLOY_TAG}
fi
git reset --hard "$TARGET^{commit}"
```

`^{commit}` resolves an annotated tag to its commit.

## Manual runs, rollback and the hub

```bash
nimdeploy run -tag v1.2.0 shop-prod      # deploy a version by hand
nimdeploy rollback shop-prod             # back to the previous good version
nimdeploy rollback -to v1.1.0 shop-prod  # back to a given version
nimdeploy status shop-prod               # COMMIT shows "v1.2.0 · 9f1c2e7"
```

- `nimdeploy run` on a deploy with tags needs `-tag` (or `-commit`): the head
  of `main` is not a version. The tag must match the pattern.
- Rollback, by hand or with `rollback_on_failure`, goes back to the last
  version that deployed successfully, with its tag.
- In the [hub](hub.md), runs show the version, and **Run again** deploys the
  last version again, not the head of the branch.
- Notifications and commit statuses carry the version and its commit.
