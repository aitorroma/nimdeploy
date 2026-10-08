#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright: (c) 2026, Aitor Roma
# MIT License (see https://opensource.org/licenses/MIT)

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r'''
---
module: nimdeploy_status
short_description: Read the state of nimdeploy deploys
version_added: "0.9.0"
description:
  - Reads C(/status) (every deploy, optionally filtered by labels) or C(/status/<name>).
options:
  url:
    description: Base URL of nimdeploy.
    type: str
    required: true
  api_token:
    description: The API token. Defaults to C($NIMDEPLOY_API_TOKEN). Needed when the server has one.
    type: str
  deploy:
    description: One deploy; all by default.
    type: str
  labels:
    description: 'Only deploys with these labels, e.g. C({environment: production}).'
    type: dict
    default: {}
  validate_certs:
    description: Verify nimdeploy's TLS certificate.
    type: bool
    default: true
  ca_path:
    description: CA bundle for nimdeploy's certificate.
    type: path
  client_cert:
    description: Client certificate (mTLS).
    type: path
  client_key:
    description: Its private key.
    type: path
  timeout:
    description: Seconds for the request.
    type: int
    default: 30
author:
  - Aitor Roma (@aitorroma)
'''

EXAMPLES = r'''
- name: Production deploys
  aitorroma.nimdeploy.nimdeploy_status:
    url: https://deploy.example.com
    labels:
      environment: production
  register: prod

- name: Stop if one is failing
  ansible.builtin.assert:
    that: prod.deploys | dict2items | selectattr('value.status', 'equalto', 'failed') | list | length == 0
'''

RETURN = r'''
deploys:
  description: State of each deploy by name.
  type: dict
  returned: always
'''

import os

from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.six.moves.urllib.parse import urlencode
from ansible_collections.aitorroma.nimdeploy.plugins.module_utils.nimdeploy import (
    CONNECTION_ARGS, NimdeployError, api_url, error_text, request)


def main():
    args = dict(
        url=dict(type='str', required=True),
        api_token=dict(type='str', no_log=True),
        deploy=dict(type='str'),
        labels=dict(type='dict', default={}),
    )
    args.update(CONNECTION_ARGS)
    module = AnsibleModule(argument_spec=args, supports_check_mode=True)
    p = module.params
    token = p['api_token'] or os.environ.get('NIMDEPLOY_API_TOKEN')
    headers = {'Authorization': 'Bearer ' + token} if token else {}
    if p['deploy']:
        path = '/status/' + p['deploy']
    else:
        path = '/status'
        if p['labels']:
            path += '?' + urlencode([('label', '%s=%s' % (k, v)) for k, v in sorted(p['labels'].items())])
    try:
        status, resp = request(module, 'GET', api_url(p['url'], path), headers=headers)
    except NimdeployError as e:
        module.fail_json(msg=str(e))
    if status != 200:
        module.fail_json(msg='HTTP %s: %s' % (status, error_text(resp)))
    deploys = {p['deploy']: resp} if p['deploy'] else resp
    module.exit_json(changed=False, deploys=deploys)


if __name__ == '__main__':
    main()
