# Logs and history

```text
/var/log/nimdeploy/
├── agency/
│   ├── 20260929-125433-51af02.log      # <date>-<time>-<first 6 chars of the delivery ID>
│   ├── 20260929-131002-c93a11.log
│   ├── latest.log -> 20260929-131002-c93a11.log
│   ├── status.json                     # last state, survives restarts
│   └── queue.json                      # queue_mode = "all": waiting and interrupted runs (600)
└── frontend/
    └── ...
```

Each run writes its own file. `latest.log` always points at the newest, and
only the last `logging.retain` files (30 by default) are kept per deploy.

## Format

```text
2026-09-29T12:54:33+02:00 deploy=agency status=started
2026-09-29T12:54:33+02:00 trigger=webhook
2026-09-29T12:54:33+02:00 repository=acme/agency
2026-09-29T12:54:33+02:00 branch=main
2026-09-29T12:54:33+02:00 commit=9f1c2e...
2026-09-29T12:54:33+02:00 pusher=dev
2026-09-29T12:54:33+02:00 delivery=51af02d4-...
2026-09-29T12:54:33+02:00 command=/usr/local/bin/deploy-agency.sh
2026-09-29T12:54:33+02:00 working_directory=/var/www/agency
2026-09-29T12:54:33+02:00 timeout=20m0s

...command output...

2026-09-29T12:56:01+02:00 status=success
2026-09-29T12:56:01+02:00 duration=1m28s
2026-09-29T12:56:01+02:00 exit_code=0
```

A failure ends with `status=failed`, the exit code and
`error="exit status 1"` (or `error="timeout after 20m0s"`).

## Where to look

```bash
tail -f /var/log/nimdeploy/agency/latest.log     # the deploy's own output
nimdeploy history agency                          # every kept run, with its file
journalctl -u nimdeploy -f                        # one line per webhook and per finished deploy
```

The journal answers "did the webhook arrive, from where, and what did
nimdeploy answer?"; the log file answers "what did my script do?".
