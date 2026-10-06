# Traefik

With nimdeploy on the host and Traefik in Docker, requests come from the
Docker network, so trust it and listen where the container can reach you:

```toml
[server]
listen = "172.17.0.1:9000"                 # docker0 bridge
trusted_proxies = ["172.16.0.0/12"]
api_token_env = "NIMDEPLOY_API_TOKEN"      # listen is not local: protect /status
```

```yaml
# dynamic configuration (file provider)
http:
  routers:
    nimdeploy:
      rule: "Host(`deploy.example.com`) && PathPrefix(`/hooks/`)"
      service: nimdeploy
      tls:
        certResolver: letsencrypt
  services:
    nimdeploy:
      loadBalancer:
        servers:
          - url: "http://172.17.0.1:9000"
```

Make sure the host firewall doesn't expose port 9000 beyond the Docker bridge.
