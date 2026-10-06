# Emails and secret links

A deploy can send an email when it finishes. The typical case: a sale comes in,
your script or Ansible playbook **creates something for the customer**, and the
customer gets a welcome email with what was created: their URL, their user,
and their password **as a link that expires** (Password Pusher), never in the
email itself.

```text
order paid ──▶ provision.sh / Ansible ──▶ $DEPLOY_OUTPUT ──▶ welcome email
                 creates the account        SITE_URL=…          to the buyer, with
                 random password            USERNAME=…          PASSWORD as a pwpush
                                            PASSWORD=…          link (expires after 3 views)
```

## 1. Configure SMTP (and pwpush)

```toml
[smtp]
host = "email-smtp.eu-west-3.amazonaws.com"   # Amazon SES, Postmark, Mailgun, your own server…
port = 587                                    # STARTTLS; 465 = TLS
user_env = "SMTP_USER"                        # values in secrets.env
password_env = "SMTP_PASSWORD"
from = "Nimbox360 <hola@nimbox360.com>"

[pwpush]
url = "https://pw.nimbox360.com"              # your instance, or https://eu.pwpush.com
token_env = "PWPUSH_TOKEN"                    # optional: links then show in your dashboard
expire_after_views = 3
expire_after_days = 7                         # open source instances
```

Check it before relying on it:

```bash
nimdeploy mail test -to you@example.com
```

!!! warning "Deliverability"
    Send through a provider with your domain verified (SPF and DKIM), e.g.
    Amazon SES on EC2. Mail sent straight from a server without them ends up
    in spam, and that is not where you want a welcome email.

## 2. Have the script report what it created

Every run gets `$DEPLOY_OUTPUT`, an empty file (`600`, deleted after the run).
Write the results there, as `KEY=VALUE` lines or a JSON object:

=== "bash"

    ```bash
    password=$(openssl rand -base64 18)
    v-add-user "c$ORDER" "$password" "$EMAIL"          # HestiaCP, for example
    {
      echo "SITE_URL=https://c$ORDER.nimbox360.com"
      echo "PANEL_URL=https://panel.nimbox360.com:8083"
      echo "USERNAME=c$ORDER"
      echo "PASSWORD=$password"
    } >> "$DEPLOY_OUTPUT"
    ```

=== "Ansible"

    ```yaml
    - name: Results for the welcome email
      ansible.builtin.copy:
        content: "{{ {'SITE_URL': site_url, 'USERNAME': account, 'PASSWORD': account_password} | to_json }}"
        dest: "{{ lookup('env', 'DEPLOY_OUTPUT') }}"
        mode: "0600"
      delegate_to: localhost
      no_log: true
    ```

The file is passed to the command's children, so a script that calls Ansible,
which calls another script, can all write to it.

## 3. Declare the email

```toml
[deploy.orders.email]
on = "success"                       # success (default) | failure | always
to_from = "billing.email"            # from the order / event (JSON path), and/or:
# to = ["ops@nimbox360.com"]
bcc = ["altas@nimbox360.com"]        # internal copy
subject = "Tu servicio está listo — pedido #{{ResourceID}}"
template = "/home/deploy/templates/welcome.html.hbs"
secrets = ["PASSWORD"]               # $DEPLOY_OUTPUT keys sent as pwpush links
```

- **`secrets`**: before rendering, each of these values is stored in Password
  Pusher; the template gets the link in `Links.PASSWORD` and the value is
  removed from `Output`. It never reaches the email, the log or the outbox.
- **`once`** (default `true`): one email per order/event (`DEPLOY_RESOURCE_ID`),
  so replays, retries and repeated `order.updated` events don't email the
  customer twice.
- Nothing personal is logged: the deploy log says
  `email: sent to a***@example.com (welcome.html.hbs, 1 secret links)`.

## Templates

Files ending in **`.hbs`** use **Handlebars**; `welcome.html.hbs` is sent as
HTML (with a text version generated from it), `welcome.txt.hbs` as plain text.
A complete example is in
[`deploy/examples/templates/welcome.html.hbs`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/templates/welcome.html.hbs).

```handlebars
<p>Hola {{Payload.billing.first_name}},</p>
<ul>{{#each Payload.line_items}}<li>{{name}} × {{quantity}}</li>{{/each}}</ul>
<p>Tu web: <a href="{{Output.SITE_URL}}">{{Output.SITE_URL}}</a></p>
{{#if Links.PASSWORD}}<p><a href="{{Links.PASSWORD}}">Ver tu contraseña</a></p>{{/if}}
```

| In the template | |
|---|---|
| `Output.KEY` | what the script wrote to `$DEPLOY_OUTPUT` (secrets removed) |
| `Links.KEY` | the pwpush link of each secret |
| `Payload.…` | the webhook body: the WooCommerce order, the Stripe event… |
| `Params.NAME` | the deploy's validated params |
| `ResourceID`, `Event`, `Deploy`, `Commit`, `Status`, `Host` | about the run |

Supported Handlebars: `{{path.to.value}}`, `{{list.0.name}}`, `{{{raw html}}}`,
`{{#if}}…{{else}}…{{/if}}`, `{{#unless}}`, `{{#each}}` with `{{this}}`,
`{{@index}}`, `{{@key}}`, `{{../x}}`/`{{@root.x}}`, `{{#with}}`, comments
`{{! }}`/`{{!-- --}}` and `{{~ ~}}`. Helpers with arguments are not supported.
A missing value renders as nothing. HTML is escaped according to where the
value goes (text, attribute, link), which is stricter than Handlebars itself.

Templates without `.hbs` (`welcome.html`, `welcome.txt`) are Go templates:
`{{ .Output.SITE_URL }}`, `{{ range }}`… with the same data.

## When sending fails

The deploy still counts as done (the service was created). The message is
kept in `/var/log/nimdeploy/<deploy>/outbox/` (`600`), the team gets a
notification (Slack, Telegram… from `[notify]`), and:

```bash
nimdeploy mail retry -n     # what is waiting
nimdeploy mail retry        # send it now
```

## From a script, any time

For other emails, or more than one per run:

```bash
"$NIMDEPLOY" mail send -to "$EMAIL" -subject "Acceso a tu VPN" \
  -template /home/deploy/templates/vpn.html.hbs -secrets VPN_KEY
# -data defaults to $DEPLOY_OUTPUT and -payload to $DEPLOY_PAYLOAD_FILE
```

And just a secret link (for an order note, a ticket, Slack):

```bash
url=$(printf '%s' "$password" | "$NIMDEPLOY" pwpush -views 1 -note "order #$DEPLOY_RESOURCE_ID")
"$NIMDEPLOY" woocommerce note "$DEPLOY_NAME" -customer "$DEPLOY_RESOURCE_ID" "Tu contraseña: $url"
```

The secret goes through stdin, never as an argument (`ps`, shell history).

## Password Pusher

- Works with [pwpush.com](https://pwpush.com) and its EU instance, Pro, and
  self-hosted open source instances from 2.4.2 (API v2). nimdeploy asks the
  instance its edition: open source expires links by `expire_after_days`;
  pwpush.com and Pro by `expire_after_duration`, their own index of durations
  (leave it unset to use the instance's default).
- **`retrieval_step` is on by default**, and it matters: Outlook Safe Links,
  Gmail and corporate antivirus open the links in emails to scan them. Without
  the extra click, a scanner can use up the views before the customer arrives.
- With `token_env`, links appear in your pwpush dashboard, where you can see
  whether they were opened and expire them by hand.
