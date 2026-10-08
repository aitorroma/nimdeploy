# -*- coding: utf-8 -*-
# Copyright: (c) 2026, Aitor Roma
# MIT License (see https://opensource.org/licenses/MIT)

"""HTTP helpers shared by the nimdeploy modules."""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

import json

from ansible.module_utils.six.moves.urllib.error import HTTPError, URLError
from ansible.module_utils.urls import open_url

CONNECTION_ARGS = dict(
    validate_certs=dict(type='bool', default=True),
    ca_path=dict(type='path'),
    client_cert=dict(type='path'),
    client_key=dict(type='path', no_log=False),
    timeout=dict(type='int', default=30),
)


class NimdeployError(Exception):
    def __init__(self, msg, status=None, body=None):
        super(NimdeployError, self).__init__(msg)
        self.status = status
        self.body = body


def request(module, method, url, body=None, headers=None, raw=None):
    """Sends a request and returns (status, decoded JSON or text)."""
    p = module.params
    hdrs = {'Accept': 'application/json', 'User-Agent': 'ansible-nimdeploy'}
    hdrs.update(headers or {})
    data = raw
    if body is not None:
        data = json.dumps(body)
        hdrs['Content-Type'] = 'application/json'
    try:
        resp = open_url(url, data=data, method=method, headers=hdrs, timeout=p['timeout'],
                        validate_certs=p['validate_certs'], ca_path=p.get('ca_path'),
                        client_cert=p.get('client_cert'), client_key=p.get('client_key'),
                        use_netrc=False)  # our Authorization header, never ~/.netrc's
        status, text = resp.getcode(), resp.read()
    except HTTPError as e:
        status, text = e.code, e.read() if hasattr(e, 'read') else b''
    except URLError as e:
        raise NimdeployError('cannot reach %s: %s' % (url, e.reason))
    if isinstance(text, bytes):
        text = text.decode('utf-8', 'replace')
    try:
        return status, json.loads(text) if text else None
    except ValueError:
        return status, text


def api_url(base, path):
    return base.rstrip('/') + path


def error_text(body):
    if isinstance(body, dict):
        return body.get('error') or body.get('reason') or json.dumps(body)
    return str(body)
