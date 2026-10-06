package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Password Pusher (pwpush.com, its EU instance, Pro, or a self-hosted open
// source one) turns a secret into a link that expires after some views or
// days, so emails carry links instead of passwords. API v2:
//   GET  /api/v2/version  → {"edition": "oss"|..., "features": {"anonymous_access": ...}}
//   POST /api/v2/pushes   {"push": {...}} → {"url_token", "html_url"}
// The open source edition expires by expire_after_days; pwpush.com and Pro
// by expire_after_duration (an index into their list of durations).

const defaultPwPushURL = "https://eu.pwpush.com"

type PwPushConfig struct {
	URL                 string `toml:"url"`
	TokenEnv            string `toml:"token_env"`
	ExpireAfterViews    int    `toml:"expire_after_views"`
	ExpireAfterDays     int    `toml:"expire_after_days"`     // open source edition
	ExpireAfterDuration *int   `toml:"expire_after_duration"` // pwpush.com / Pro: their duration index
	RetrievalStep       *bool  `toml:"retrieval_step"`
	DeletableByViewer   *bool  `toml:"deletable_by_viewer"`

	token string
}

func (p *PwPushConfig) validate() error {
	if p.URL == "" {
		p.URL = defaultPwPushURL
	}
	p.URL = strings.TrimRight(p.URL, "/")
	if !strings.HasPrefix(p.URL, "https://") && !strings.HasPrefix(p.URL, "http://127.0.0.1") && !strings.HasPrefix(p.URL, "http://localhost") {
		return fmt.Errorf("pwpush.url must be https://")
	}
	if p.ExpireAfterViews == 0 {
		p.ExpireAfterViews = 3
	}
	if p.ExpireAfterViews < 1 || p.ExpireAfterViews > 100 {
		return fmt.Errorf("pwpush.expire_after_views must be 1-100")
	}
	if p.ExpireAfterDays < 0 || p.ExpireAfterDays > 90 {
		return fmt.Errorf("pwpush.expire_after_days must be 1-90")
	}
	if p.ExpireAfterDuration != nil && (*p.ExpireAfterDuration < 0 || *p.ExpireAfterDuration > 17) {
		return fmt.Errorf("pwpush.expire_after_duration is pwpush.com's index 0-17")
	}
	return nil
}

type pwPush struct {
	cfg  PwPushConfig
	http *http.Client

	once    sync.Once
	edition string
	verErr  error
}

func newPwPush(cfg PwPushConfig) *pwPush {
	return &pwPush{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}}
}

func (p *pwPush) request(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.cfg.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nimdeploy/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.token)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		msg := truncate(strings.TrimSpace(string(b)), 200)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += " (set [pwpush] token_env: this instance needs an API token)"
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// editionOf asks the instance once which edition it runs.
func (p *pwPush) editionOf() (string, error) {
	p.once.Do(func() {
		var v struct {
			Edition string `json:"edition"`
		}
		p.verErr = p.request(http.MethodGet, "/api/v2/version", nil, &v)
		if p.verErr != nil {
			p.verErr = fmt.Errorf("%w (API v2 needs Password Pusher 2.4.2 or newer)", p.verErr)
		}
		p.edition = v.Edition
	})
	return p.edition, p.verErr
}

// push stores a secret and returns its link.
func (p *pwPush) push(secret, note string, views, days int) (string, error) {
	if secret == "" {
		return "", errors.New("empty secret")
	}
	edition, err := p.editionOf()
	if err != nil {
		return "", err
	}
	if views == 0 {
		views = p.cfg.ExpireAfterViews
	}
	if days == 0 {
		days = p.cfg.ExpireAfterDays
	}
	push := map[string]any{
		"payload":             secret,
		"expire_after_views":  views,
		"retrieval_step":      p.cfg.RetrievalStep == nil || *p.cfg.RetrievalStep,
		"deletable_by_viewer": p.cfg.DeletableByViewer == nil || *p.cfg.DeletableByViewer,
	}
	if note != "" {
		push["note"] = note
	}
	if edition == "oss" {
		if days > 0 {
			push["expire_after_days"] = days
		}
	} else if p.cfg.ExpireAfterDuration != nil {
		push["expire_after_duration"] = *p.cfg.ExpireAfterDuration
	}
	var res struct {
		URLToken string `json:"url_token"`
		HTMLURL  string `json:"html_url"`
	}
	if err := p.request(http.MethodPost, "/api/v2/pushes", map[string]any{"push": push}, &res); err != nil {
		return "", err
	}
	if res.HTMLURL != "" {
		return res.HTMLURL, nil
	}
	if res.URLToken == "" {
		return "", errors.New("the response has no url_token")
	}
	link := p.cfg.URL + "/p/" + res.URLToken
	if push["retrieval_step"] == true {
		link += "/r"
	}
	return link, nil
}

// cliPwPush reads a secret from stdin and prints a pwpush link. The secret
// never appears in arguments (ps, shell history).
func cliPwPush(cfg *Config, envFile string, args []string) int {
	fset := flag.NewFlagSet("pwpush", flag.ExitOnError)
	views := fset.Int("views", 0, "expire after this many views (default: [pwpush] expire_after_views)")
	days := fset.Int("days", 0, "expire after this many days (open source instances; default: [pwpush] expire_after_days)")
	note := fset.String("note", "", "note shown in your pwpush dashboard (not to the viewer)")
	fset.Usage = func() {
		_, _ = io.WriteString(fset.Output(), "usage: printf '%s' \"$secret\" | nimdeploy pwpush [-views 1] [-days 7] [-note \"order #1234\"]\n\nStores the secret read from stdin in Password Pusher and prints its link.\n\n")
		fset.PrintDefaults()
	}
	fset.Parse(args)
	if isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "nimdeploy pwpush: pipe the secret on stdin")
		return 2
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	secret := strings.TrimRight(string(b), "\r\n")
	if err := loadSecretsInto(envFile); err == nil {
		_ = cfg.ResolveSecrets()
	}
	link, err := newPwPush(cfg.PwPush).push(secret, *note, *views, *days)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy pwpush:", err)
		return 1
	}
	fmt.Println(link)
	return 0
}
