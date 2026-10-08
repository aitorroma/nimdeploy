# aitorroma.nimdeploy

Ansible modules for [nimdeploy](https://nimdeploy.nimbox360.com):

| Module | |
|---|---|
| `nimdeploy_run` | start a deploy (or a rollback) through the API and wait for its result; a `delivery_id` makes retries safe |
| `nimdeploy_send` | send a signed webhook to a `provider = "generic"` deploy |
| `nimdeploy_status` | read the state of the deploys, filtered by labels |

All of them support a private CA and client certificates (mTLS).

```yaml
- hosts: localhost
  tasks:
    - aitorroma.nimdeploy.nimdeploy_run:
        url: https://deploy.example.com
        api_token: "{{ vault_nimdeploy_token }}"
        deploy: api
        params: { VERSION: "1.4.0" }
        delivery_id: "release-1.4.0"
      register: deploy

    - debug:
        msg: "{{ deploy.state.status }} in {{ deploy.state.duration }}"
```

Install from the release tarball:

```bash
ansible-galaxy collection install https://github.com/aitorroma/nimdeploy/releases/download/v0.9.0/aitorroma-nimdeploy-0.9.0.tar.gz
```

Documentation: https://nimdeploy.nimbox360.com/guides/ansible/
