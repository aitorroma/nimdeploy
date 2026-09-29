package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	notifyTimeout = 10 * time.Second
	// Discord rejects messages over 2000 chars, Telegram over 4096.
	maxMessageChars = 1900
)

var fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Notifier sends chat notifications and GitHub commit statuses. A nil
// *Notifier or an unconfigured one does nothing.
type Notifier struct {
	notify      NotifyConfig
	github      GitHubConfig
	telegramAPI string
	host        string
	client      *http.Client
}

func NewNotifier(cfg *Config) *Notifier {
	host, _ := os.Hostname()
	return &Notifier{
		notify:      cfg.Notify,
		github:      cfg.GitHub,
		telegramAPI: "https://api.telegram.org",
		host:        host,
		client:      &http.Client{Timeout: notifyTimeout},
	}
}

// CommitStatus sets a GitHub commit status ("pending", "success", "failure").
func (n *Notifier) CommitStatus(d *DeployConfig, t Trigger, state, description string) {
	if n == nil || n.github.token == "" || !fullSHARe.MatchString(t.Commit) || t.Repository == "" {
		return
	}
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	endpoint := fmt.Sprintf("%s/repos/%s/statuses/%s", strings.TrimRight(n.github.APIURL, "/"), t.Repository, t.Commit)
	body := map[string]string{
		"state":       state,
		"description": description,
		"context":     "nimdeploy/" + d.Name,
	}
	headers := map[string]string{
		"Authorization":        "Bearer " + n.github.token,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if err := n.post(endpoint, body, headers); err != nil {
		log.Printf("deploy=%s github commit status %s failed: %v", d.Name, state, err)
	}
}

// Finished reports the end of a deploy: commit status and chat notification.
func (n *Notifier) Finished(d *DeployConfig, t Trigger, st State, recovered bool, logPath string) {
	if n == nil {
		return
	}
	if st.Status == StatusSuccess {
		n.CommitStatus(d, t, "success", "Deployed in "+st.Duration)
	} else {
		n.CommitStatus(d, t, "failure", "Deploy failed: "+st.Error)
	}

	if !n.shouldNotify(st.Status, recovered) {
		return
	}
	var tail []string
	if st.Status != StatusSuccess && n.notify.LogLines > 0 {
		tail = readTail(logPath, n.notify.LogLines)
	}
	if err := n.send(st, recovered, logPath, tail); err != nil {
		log.Printf("deploy=%s notification failed: %v", d.Name, err)
	}
}

func (n *Notifier) shouldNotify(status string, recovered bool) bool {
	if n.notify.Format == "" {
		return false
	}
	switch n.notify.On {
	case "always":
		return true
	case "failure":
		return status != StatusSuccess || recovered
	}
	return false
}

func (n *Notifier) send(st State, recovered bool, logPath string, tail []string) error {
	switch n.notify.Format {
	case "slack":
		return n.post(n.notify.url, map[string]string{"text": n.message(st, recovered, logPath, tail, true)}, nil)
	case "discord":
		return n.post(n.notify.url, map[string]string{"content": n.message(st, recovered, logPath, tail, true)}, nil)
	case "telegram":
		endpoint := fmt.Sprintf("%s/bot%s/sendMessage", n.telegramAPI, n.notify.telegramToken)
		return n.post(endpoint, map[string]any{
			"chat_id":                  n.notify.TelegramChatID,
			"text":                     n.message(st, recovered, logPath, tail, false),
			"disable_web_page_preview": true,
		}, nil)
	case "json":
		return n.post(n.notify.url, map[string]any{
			"event":     "deploy.finished",
			"host":      n.host,
			"recovered": recovered,
			"state":     st,
			"log_path":  logPath,
			"log_tail":  tail,
		}, nil)
	}
	return nil
}

func (n *Notifier) message(st State, recovered bool, logPath string, tail []string, fences bool) string {
	var b strings.Builder
	switch {
	case recovered:
		fmt.Fprintf(&b, "✅ %s deploy recovered on %s\n", st.Deploy, n.host)
	case st.Status == StatusSuccess:
		fmt.Fprintf(&b, "✅ %s deployed on %s\n", st.Deploy, n.host)
	default:
		fmt.Fprintf(&b, "❌ %s deploy FAILED on %s\n", st.Deploy, n.host)
	}
	fmt.Fprintf(&b, "%s@%s", st.Repository, st.Branch)
	if st.Commit != "" {
		fmt.Fprintf(&b, " · commit %s", shortSHA(st.Commit))
	}
	if st.Pusher != "" {
		fmt.Fprintf(&b, " · by %s", st.Pusher)
	}
	if st.Trigger == TriggerManual {
		b.WriteString(" (manual)")
	}
	fmt.Fprintf(&b, "\nduration %s", st.Duration)
	if st.Status != StatusSuccess {
		if st.ExitCode != nil {
			fmt.Fprintf(&b, " · exit %d", *st.ExitCode)
		}
		fmt.Fprintf(&b, " · %s", st.Error)
	}
	fmt.Fprintf(&b, "\nlog: %s", logPath)

	if len(tail) > 0 {
		text := strings.Join(tail, "\n")
		if room := maxMessageChars - b.Len() - 10; len(text) > room {
			text = "…" + text[max(0, len(text)-room):]
		}
		if fences {
			fmt.Fprintf(&b, "\n```\n%s\n```", text)
		} else {
			fmt.Fprintf(&b, "\n\n%s", text)
		}
	}
	return b.String()
}

func (n *Notifier) post(endpoint string, body any, headers map[string]string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nimdeploy")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// Don't log the URL: Slack/Discord/Telegram URLs contain the secret.
		if uerr, ok := err.(*url.Error); ok {
			return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// readTail returns the last n lines of a file, reading at most 64 KB.
func readTail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const chunk = 64 << 10
	if info, err := f.Stat(); err == nil && info.Size() > chunk {
		_, _ = f.Seek(info.Size()-chunk, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return lines[max(0, len(lines)-n):]
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
