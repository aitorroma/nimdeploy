package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ciPollInterval is how often GitHub is asked about a commit's workflows.
var ciPollInterval = 15 * time.Second

type workflowRun struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	RunAttempt int       `json:"run_attempt"`
	CreatedAt  time.Time `json:"created_at"`
}

// ciState reports the latest run of each required workflow for sha. done is
// true once any of them failed or all of them passed; summary reads like
// "linter=success tests=in_progress".
func (n *Notifier) ciState(ctx context.Context, repo, sha string, workflows []string) (done, ok bool, summary string, err error) {
	endpoint := fmt.Sprintf("%s/repos/%s/actions/runs?head_sha=%s&per_page=100",
		strings.TrimRight(n.github.APIURL, "/"), repo, url.QueryEscape(sha))
	var body struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := n.getJSON(ctx, endpoint, &body); err != nil {
		return false, false, "", err
	}

	latest := map[string]workflowRun{}
	for _, run := range body.WorkflowRuns {
		prev, seen := latest[run.Name]
		if !seen || run.CreatedAt.After(prev.CreatedAt) ||
			(run.CreatedAt.Equal(prev.CreatedAt) && run.RunAttempt > prev.RunAttempt) {
			latest[run.Name] = run
		}
	}

	var parts []string
	passed, failed := 0, false
	for _, name := range workflows {
		run, found := latest[name]
		state := "not_started"
		switch {
		case !found:
		case run.Status != "completed":
			state = run.Status
		default:
			state = run.Conclusion
			switch run.Conclusion {
			case "success", "skipped", "neutral":
				passed++
			default:
				failed = true
			}
		}
		parts = append(parts, name+"="+state)
	}
	summary = strings.Join(parts, " ")
	if failed {
		return true, false, summary, nil
	}
	return passed == len(workflows), passed == len(workflows), summary, nil
}

func (n *Notifier) getJSON(ctx context.Context, endpoint string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.github.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "nimdeploy")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("GitHub API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// needsCI reports whether a deploy has to wait for CI before running.
// Manual runs don't: whoever starts them decides.
func needsCI(d *DeployConfig, t Trigger) bool {
	return len(d.WaitForCI) > 0 && t.Source == TriggerWebhook && t.Commit != ""
}

// waitForCI polls GitHub until the required workflows pass or fail, the
// timeout expires, a newer push is queued (superseded) or nimdeploy stops.
func (r *Runner) waitForCI(d *DeployConfig, t Trigger, n *Notifier, logf func(string, ...any)) (ok bool, reason string, superseded bool) {
	deadline := time.Now().Add(d.CITimeout.Duration)
	last := ""
	for {
		if r.ctx.Err() != nil {
			return false, errShutdownCanceled, false
		}
		r.mu.Lock()
		p := r.pending[t.lane]
		r.mu.Unlock()
		if len(p) > 0 {
			return false, "superseded by a newer push (" + shortSHA(p[len(p)-1].trigger.Commit) + ")", true
		}

		done, passed, summary, err := n.ciState(r.ctx, t.Repository, t.Commit, d.WaitForCI)
		if err != nil {
			summary = "error: " + err.Error()
		}
		if summary != last {
			logf("ci: %s", summary)
			last = summary
		}
		if err == nil && done {
			if passed {
				return true, "", false
			}
			return false, "CI failed: " + summary, false
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("CI not finished after %s: %s", d.CITimeout, summary), false
		}
		select {
		case <-r.ctx.Done():
		case <-time.After(ciPollInterval):
		}
	}
}
