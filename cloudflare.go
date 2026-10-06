package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cloudflareRanges are Cloudflare's edge networks, from
// https://www.cloudflare.com/ips-v4 and /ips-v6 (checked 2026-09-29).
// `trusted_proxies = ["cloudflare"]` expands to this list.
var cloudflareRanges = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
	"2400:cb00::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2405:b500::/32",
	"2405:8100::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

// purgeCloudflare clears the zone's cache after a deploy: everything, or the
// given URLs (in batches of 30, Cloudflare's limit per call).
func purgeCloudflare(ctx context.Context, cf CloudflareConfig, zone string, purge []string) error {
	if cf.token == "" {
		return errors.New("[cloudflare] api_token_env is not set")
	}
	var bodies []map[string]any
	if len(purge) == 1 && purge[0] == "everything" {
		bodies = append(bodies, map[string]any{"purge_everything": true})
	} else {
		for i := 0; i < len(purge); i += 30 {
			bodies = append(bodies, map[string]any{"files": purge[i:min(i+30, len(purge))]})
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	endpoint := strings.TrimRight(cf.APIURL, "/") + "/zones/" + url.PathEscape(zone) + "/purge_cache"
	for _, body := range bodies {
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cf.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		var res struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 || !res.Success {
			msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			if len(res.Errors) > 0 {
				msg += ": " + res.Errors[0].Message
			}
			return errors.New(msg)
		}
	}
	return nil
}
