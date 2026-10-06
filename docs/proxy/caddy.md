# Caddy

```caddy
deploy.example.com {
    handle /hooks/* {
        request_body {
            max_size 25MB
        }
        reverse_proxy 127.0.0.1:9000   # or unix//run/nimdeploy/nimdeploy.sock
    }
    respond 404
}
```

Caddy sets `X-Forwarded-For` and gets its certificates by itself.
