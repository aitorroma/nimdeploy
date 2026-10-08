#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright: (c) 2026, Aitor Roma
# MIT License (see https://opensource.org/licenses/MIT)

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r'''
---
module: nimdeploy_run
short_description: Run a nimdeploy deploy and wait for its result
version_added: "0.9.0"
description:
  - Starts a deploy through nimdeploy's API (C(POST /deploy/<name>), or C(/rollback/<name>)), as C(nimdeploy run) does.
  - Waits until that run finishes and fails the task when the deploy fails.
  - Sends a I(delivery_id), so a retried task doesn't deploy twice.
options:
  url:
    description: Base URL of nimdeploy, including its C(base_path) if any, e.g. C(https://deploy.example.com).
    type: str
    required: true
  api_token:
    description: The C(server.api_token_env) token. Defaults to C($NIMDEPLOY_API_TOKEN).
    type: str
  deploy:
    description: The deploy's name.
    type: str
    required: true
  commit:
    description: Commit to deploy (git deploys); the latest of the branch by default.
    type: str
  params:
    description: Params of the deploy, validated by nimdeploy like a webhook's.
    type: dict
    default: {}
  rollback:
    description: Redeploy the last good commit (or I(commit)) instead.
    type: bool
    default: false
  user:
    description: Shown as who triggered it.
    type: str
    default: ansible
  delivery_id:
    description:
      - Identifies this request; the same id twice runs once. A random one by default.
      - Set it (e.g. to the job id) to make retries of the whole playbook safe too.
    type: str
  wait:
    description: Wait for the run to finish.
    type: bool
    default: true
  wait_timeout:
    description: Seconds to wait.
    type: int
    default: 1800
  poll_interval:
    description: Seconds between checks.
    type: int
    default: 3
  fail_on_error:
    description: Fail the task when the deploy fails.
    type: bool
    default: true
  validate_certs:
    description: Verify nimdeploy's TLS certificate.
    type: bool
    default: true
  ca_path:
    description: CA bundle for nimdeploy's certificate.
    type: path
  client_cert:
    description: Client certificate, when nimdeploy requires one (mTLS).
    type: path
  client_key:
    description: Its private key.
    type: path
  timeout:
    description: Seconds for each HTTP request.
    type: int
    default: 30
author:
  - Aitor Roma (@aitorroma)
'''

EXAMPLES = r'''
- name: Deploy the API and wait for it
  aitorroma.nimdeploy.nimdeploy_run:
    url: https://deploy.example.com
    api_token: "{{ vault_nimdeploy_token }}"
    deploy: api
    params:
      SERVICE: api
      VERSION: "{{ version }}"
    delivery_id: "release-{{ version }}"

- name: Roll back the frontend
  aitorroma.nimdeploy.nimdeploy_run:
    url: https://deploy.example.com
    deploy: frontend
    rollback: true
'''

RETURN = r'''
result:
  description: C(started), C(queued) or C(duplicate) (this delivery_id already ran).
  type: str
  returned: always
state:
  description: The run's state as nimdeploy reports it (status, commit, duration, error, log, ansible summary...).
  type: dict
  returned: when wait is true
delivery_id:
  description: The id sent.
  type: str
  returned: always
'''

import os
import time
import uuid

from ansible.module_utils.basic import AnsibleModule
from ansible_collections.aitorroma.nimdeploy.plugins.module_utils.nimdeploy import (
    CONNECTION_ARGS, NimdeployError, api_url, error_text, request)

ACTIVE = ('running', 'waiting')


def find_run(module, base, headers, name, delivery):
    """Returns ('active'|'queued'|'done'|'unknown', state) for our delivery."""
    status, st = request(module, 'GET', api_url(base, '/status/' + name), headers=headers)
    if status != 200 or not isinstance(st, dict):
        raise NimdeployError('status: HTTP %s: %s' % (status, error_text(st)), status, st)
    if st.get('delivery') == delivery:
        return ('active' if st.get('status') in ACTIVE else 'done'), st
    queued = st.get('queued') or {}
    if queued.get('delivery') == delivery:
        return 'queued', st
    status, hist = request(module, 'GET', api_url(base, '/history/' + name + '?limit=50'), headers=headers)
    if status == 200 and isinstance(hist, list):
        for h in hist:
            if h.get('delivery') == delivery and h.get('status') not in ACTIVE:
                return 'done', h
    return 'unknown', st


def main():
    args = dict(
        url=dict(type='str', required=True),
        api_token=dict(type='str', no_log=True),
        deploy=dict(type='str', required=True),
        commit=dict(type='str'),
        params=dict(type='dict', default={}),
        rollback=dict(type='bool', default=False),
        user=dict(type='str', default='ansible'),
        delivery_id=dict(type='str'),
        wait=dict(type='bool', default=True),
        wait_timeout=dict(type='int', default=1800),
        poll_interval=dict(type='int', default=3),
        fail_on_error=dict(type='bool', default=True),
    )
    args.update(CONNECTION_ARGS)
    module = AnsibleModule(argument_spec=args, supports_check_mode=True)
    p = module.params
    token = p['api_token'] or os.environ.get('NIMDEPLOY_API_TOKEN')
    if not token:
        module.fail_json(msg='api_token (or $NIMDEPLOY_API_TOKEN) is required')
    headers = {'Authorization': 'Bearer ' + token}
    delivery = p['delivery_id'] or 'ansible-' + uuid.uuid4().hex
    name = p['deploy']
    result = dict(changed=False, delivery_id=delivery)

    if module.check_mode:
        result.update(changed=True, result='would_run', msg='would %s %s' % ('roll back' if p['rollback'] else 'deploy', name))
        module.exit_json(**result)

    body = dict(user=p['user'])
    if p['commit']:
        body['commit'] = p['commit']
    path = '/rollback/' if p['rollback'] else '/deploy/'
    if not p['rollback']:
        body['delivery'] = delivery
        if p['params']:
            body['params'] = dict((k, str(v)) for k, v in p['params'].items())
    try:
        status, resp = request(module, 'POST', api_url(p['url'], path + name), body=body, headers=headers)
        if status == 202:
            result.update(changed=True, result=resp.get('result', 'started'))
            if p['rollback']:
                if result['result'] != 'started':
                    result['msg'] = 'rollback queued behind the running deploy; not waiting for it'
                    module.exit_json(**result)
                delivery = None  # rollbacks have no delivery: follow the run it started
                result['log'] = resp.get('state', {}).get('log')
        elif status == 200 and isinstance(resp, dict) and resp.get('status') == 'ignored':
            result['result'] = 'duplicate' if 'duplicate' in resp.get('reason', '') else 'ignored'
            if result['result'] == 'ignored':
                result['msg'] = resp.get('reason')
                module.exit_json(**result)
        else:
            module.fail_json(msg='nimdeploy refused the deploy: HTTP %s: %s' % (status, error_text(resp)), status=status, **result)

        if not p['wait']:
            module.exit_json(**result)
        deadline = time.time() + p['wait_timeout']
        while True:
            if delivery is None:
                s, st = request(module, 'GET', api_url(p['url'], '/status/' + name), headers=headers)
                where = 'done' if s == 200 and st.get('log') == result.get('log') and st.get('status') not in ACTIVE else 'active'
            else:
                where, st = find_run(module, p['url'], headers, name, delivery)
            if where == 'done':
                break
            if time.time() > deadline:
                module.fail_json(msg='deploy %s still %s after %ss' % (name, where, p['wait_timeout']), state=st, **result)
            time.sleep(p['poll_interval'])
    except NimdeployError as e:
        module.fail_json(msg=str(e), **result)

    result['state'] = st
    if st.get('status') != 'success' and p['fail_on_error'] and st.get('status') != 'skipped':
        module.fail_json(msg='deploy %s %s: %s' % (name, st.get('status'), st.get('error', '')), **result)
    module.exit_json(**result)


if __name__ == '__main__':
    main()
