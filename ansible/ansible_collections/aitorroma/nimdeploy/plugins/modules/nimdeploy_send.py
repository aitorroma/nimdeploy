#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright: (c) 2026, Aitor Roma
# MIT License (see https://opensource.org/licenses/MIT)

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r'''
---
module: nimdeploy_send
short_description: Send a signed webhook to a nimdeploy generic deploy
version_added: "0.9.0"
description:
  - POSTs a JSON body to a deploy with C(provider = "generic"), signed with its secret (HMAC-SHA256) or carrying its token,
    as C(nimdeploy send) does.
  - The same I(delivery_id) twice runs once.
options:
  url:
    description: The hook's full URL, e.g. C(https://deploy.example.com/hooks/services).
    type: str
    required: true
  secret:
    description: The deploy's secret (its C(secret_env) value). Defaults to C($NIMDEPLOY_SECRET).
    type: str
  body:
    description: The JSON body.
    type: dict
    default: {}
  auth:
    description: C(hmac) signs the body; C(token) sends the secret as a token.
    type: str
    choices: [hmac, token]
    default: hmac
  signature_header:
    description: Header of the HMAC signature (C(signature_header) in the deploy).
    type: str
    default: X-Signature
  timestamp_header:
    description: When the deploy has C(timestamp_header), the same header; the signature then covers it.
    type: str
  token_header:
    description: Header of the token with I(auth=token); C(Authorization) sends C(Bearer <token>).
    type: str
    default: Authorization
  delivery_header:
    description: Header of the delivery id.
    type: str
    default: X-Delivery-ID
  delivery_id:
    description: Identifies this request; a random one by default.
    type: str
  validate_certs:
    description: Verify nimdeploy's TLS certificate.
    type: bool
    default: true
  ca_path:
    description: CA bundle for nimdeploy's certificate.
    type: path
  client_cert:
    description: Client certificate, for deploys with C(client_names) (mTLS).
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
- name: Restart a service through nimdeploy
  aitorroma.nimdeploy.nimdeploy_send:
    url: https://deploy.example.com/hooks/services
    secret: "{{ vault_services_secret }}"
    body:
      service: api
      action: restart
    delivery_id: "restart-api-{{ ansible_date_time.epoch }}"
'''

RETURN = r'''
status:
  description: The HTTP status (202 started or queued, 200 ignored).
  type: int
  returned: always
response:
  description: nimdeploy's answer.
  type: raw
  returned: always
delivery_id:
  description: The id sent.
  type: str
  returned: always
'''

import hashlib
import hmac
import json
import os
import time
import uuid

from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.common.text.converters import to_bytes
from ansible_collections.aitorroma.nimdeploy.plugins.module_utils.nimdeploy import (
    CONNECTION_ARGS, NimdeployError, error_text, request)


def main():
    args = dict(
        url=dict(type='str', required=True),
        secret=dict(type='str', no_log=True),
        body=dict(type='dict', default={}),
        auth=dict(type='str', default='hmac', choices=['hmac', 'token']),
        signature_header=dict(type='str', default='X-Signature'),
        timestamp_header=dict(type='str'),
        token_header=dict(type='str', default='Authorization', no_log=False),
        delivery_header=dict(type='str', default='X-Delivery-ID'),
        delivery_id=dict(type='str'),
    )
    args.update(CONNECTION_ARGS)
    module = AnsibleModule(argument_spec=args, supports_check_mode=True)
    p = module.params
    secret = p['secret'] or os.environ.get('NIMDEPLOY_SECRET')
    if not secret:
        module.fail_json(msg='secret (or $NIMDEPLOY_SECRET) is required')
    delivery = p['delivery_id'] or 'ansible-' + uuid.uuid4().hex
    result = dict(changed=False, delivery_id=delivery)
    if module.check_mode:
        module.exit_json(changed=True, status=0, response=None, delivery_id=delivery)

    raw = json.dumps(p['body'], separators=(',', ':'))
    headers = {'Content-Type': 'application/json', p['delivery_header']: delivery}
    if p['auth'] == 'hmac':
        signed = to_bytes(raw)
        if p['timestamp_header']:
            ts = str(int(time.time()))
            headers[p['timestamp_header']] = ts
            signed = to_bytes(ts + '.') + signed
        headers[p['signature_header']] = 'sha256=' + hmac.new(to_bytes(secret), signed, hashlib.sha256).hexdigest()
    elif p['token_header'].lower() == 'authorization':
        headers['Authorization'] = 'Bearer ' + secret
    else:
        headers[p['token_header']] = secret
    try:
        status, resp = request(module, 'POST', p['url'], raw=raw, headers=headers)
    except NimdeployError as e:
        module.fail_json(msg=str(e), **result)
    result.update(status=status, response=resp)
    if status == 202:
        result['changed'] = True
    elif status != 200:
        module.fail_json(msg='nimdeploy refused the webhook: HTTP %s: %s' % (status, error_text(resp)), **result)
    module.exit_json(**result)


if __name__ == '__main__':
    main()
