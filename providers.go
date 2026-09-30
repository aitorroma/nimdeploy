package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// A provider knows how one git host authenticates and formats its webhooks.
type provider struct {
	verify func(r *http.Request, body, secret []byte) bool
	parse  func(r *http.Request, body []byte) (hookEvent, error)
}

var providers = map[string]provider{
	"github":    {verify: verifyHeaderHMAC("X-Hub-Signature-256"), parse: parseGitHub},
	"gitea":     {verify: verifyGitea, parse: parseGitea},
	"forgejo":   {verify: verifyGitea, parse: parseGitea},
	"gitlab":    {verify: verifyGitLab, parse: parseGitLab},
	"bitbucket": {verify: verifyHeaderHMAC("X-Hub-Signature"), parse: parseBitbucket},
}

func providerNames() []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// hookEvent is a webhook delivery in provider-neutral form.
type hookEvent struct {
	Name       string // the provider's event name, for logs
	Ping       bool
	Push       bool
	Delivery   string
	Repository string
	Pusher     string
	// Pushes has one entry per updated ref; Bitbucket can send several.
	Pushes []pushInfo
}

type pushInfo struct {
	Ref     string
	Commit  string
	Deleted bool
}

// --- authentication -----------------------------------------------------------

// verifyHeaderHMAC checks a "sha256=<hex HMAC of the body>" header.
func verifyHeaderHMAC(header string) func(*http.Request, []byte, []byte) bool {
	return func(r *http.Request, body, secret []byte) bool {
		return validSignature(secret, body, r.Header.Get(header))
	}
}

// verifyGitea accepts Forgejo's and Gitea's plain hex signatures, or the
// GitHub-style header both also send.
func verifyGitea(r *http.Request, body, secret []byte) bool {
	if sig := firstHeader(r, "X-Forgejo-Signature", "X-Gitea-Signature"); sig != "" {
		return validSignature(secret, body, "sha256="+sig)
	}
	return validSignature(secret, body, r.Header.Get("X-Hub-Signature-256"))
}

// verifyGitLab compares the secret token GitLab sends as is.
func verifyGitLab(r *http.Request, _, secret []byte) bool {
	token := r.Header.Get("X-Gitlab-Token")
	return token != "" && subtle.ConstantTimeCompare([]byte(token), secret) == 1
}

func validSignature(secret, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// --- payloads -----------------------------------------------------------------

func parseGitHub(r *http.Request, body []byte) (hookEvent, error) {
	ev := hookEvent{Name: r.Header.Get("X-GitHub-Event"), Delivery: r.Header.Get("X-GitHub-Delivery")}
	ev.Ping, ev.Push = ev.Name == "ping", ev.Name == "push"
	if !ev.Push {
		return ev, nil
	}
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pusher struct {
			Name string `json:"name"`
		} `json:"pusher"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ev, err
	}
	ev.Repository, ev.Pusher = p.Repository.FullName, p.Pusher.Name
	ev.Pushes = []pushInfo{{Ref: p.Ref, Commit: p.After, Deleted: p.Deleted || zeroSHA(p.After)}}
	return ev, nil
}

// parseGitea handles Gitea and Forgejo, which share the payload format.
func parseGitea(r *http.Request, body []byte) (hookEvent, error) {
	ev := hookEvent{
		Name:     firstHeader(r, "X-Forgejo-Event", "X-Gitea-Event", "X-Gogs-Event", "X-GitHub-Event"),
		Delivery: firstHeader(r, "X-Forgejo-Delivery", "X-Gitea-Delivery", "X-Gogs-Delivery", "X-GitHub-Delivery"),
	}
	ev.Ping, ev.Push = ev.Name == "ping", ev.Name == "push"
	if !ev.Push {
		return ev, nil
	}
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pusher struct {
			Login    string `json:"login"`
			Username string `json:"username"`
		} `json:"pusher"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ev, err
	}
	ev.Repository = p.Repository.FullName
	ev.Pusher = firstNonEmpty(p.Pusher.Login, p.Pusher.Username)
	ev.Pushes = []pushInfo{{Ref: p.Ref, Commit: p.After, Deleted: zeroSHA(p.After)}}
	return ev, nil
}

func parseGitLab(r *http.Request, body []byte) (hookEvent, error) {
	ev := hookEvent{
		Name: r.Header.Get("X-Gitlab-Event"),
		// Idempotency-Key stays the same when GitLab retries a delivery.
		Delivery: firstHeader(r, "Idempotency-Key", "X-Gitlab-Event-UUID"),
	}
	var p struct {
		ObjectKind   string  `json:"object_kind"`
		Ref          string  `json:"ref"`
		After        string  `json:"after"`
		CheckoutSHA  *string `json:"checkout_sha"`
		UserUsername string  `json:"user_username"`
		UserName     string  `json:"user_name"`
		Project      struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		if ev.Name == "Push Hook" {
			return ev, err
		}
		return ev, nil // some other event we don't handle anyway
	}
	// "Push Hook", or a system hook with object_kind push; tag pushes are
	// object_kind tag_push.
	ev.Push = p.ObjectKind == "push"
	if !ev.Push {
		if ev.Name == "" {
			ev.Name = p.ObjectKind
		}
		return ev, nil
	}
	ev.Repository = p.Project.PathWithNamespace
	ev.Pusher = firstNonEmpty(p.UserUsername, p.UserName)
	ev.Pushes = []pushInfo{{Ref: p.Ref, Commit: p.After, Deleted: zeroSHA(p.After) || p.CheckoutSHA == nil}}
	return ev, nil
}

// parseBitbucket handles Bitbucket Cloud (repo:push) and Bitbucket Data
// Center / Server (repo:refs_changed).
func parseBitbucket(r *http.Request, body []byte) (hookEvent, error) {
	ev := hookEvent{Name: r.Header.Get("X-Event-Key"), Delivery: firstHeader(r, "X-Request-UUID", "X-Request-Id")}
	switch ev.Name {
	case "diagnostics:ping":
		ev.Ping = true
	case "repo:push":
		type ref struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			Target struct {
				Hash string `json:"hash"`
			} `json:"target"`
		}
		var p struct {
			Actor struct {
				Nickname    string `json:"nickname"`
				DisplayName string `json:"display_name"`
			} `json:"actor"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			Push struct {
				Changes []struct {
					New *ref `json:"new"`
					Old *ref `json:"old"`
				} `json:"changes"`
			} `json:"push"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return ev, err
		}
		ev.Push = true
		ev.Repository = p.Repository.FullName
		ev.Pusher = firstNonEmpty(p.Actor.Nickname, p.Actor.DisplayName)
		for _, c := range p.Push.Changes {
			switch {
			case c.New != nil && c.New.Type == "branch":
				ev.Pushes = append(ev.Pushes, pushInfo{Ref: "refs/heads/" + c.New.Name, Commit: c.New.Target.Hash})
			case c.New == nil && c.Old != nil && c.Old.Type == "branch":
				ev.Pushes = append(ev.Pushes, pushInfo{Ref: "refs/heads/" + c.Old.Name, Deleted: true})
			}
		}
	case "repo:refs_changed":
		var p struct {
			Actor struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"actor"`
			Repository struct {
				Slug    string `json:"slug"`
				Project struct {
					Key string `json:"key"`
				} `json:"project"`
			} `json:"repository"`
			Changes []struct {
				Ref struct {
					ID string `json:"id"`
				} `json:"ref"`
				RefID  string `json:"refId"`
				ToHash string `json:"toHash"`
				Type   string `json:"type"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return ev, err
		}
		ev.Push = true
		ev.Repository = p.Repository.Project.Key + "/" + p.Repository.Slug
		ev.Pusher = firstNonEmpty(p.Actor.Name, p.Actor.DisplayName)
		for _, c := range p.Changes {
			ev.Pushes = append(ev.Pushes, pushInfo{
				Ref:     firstNonEmpty(c.Ref.ID, c.RefID),
				Commit:  c.ToHash,
				Deleted: c.Type == "DELETE" || zeroSHA(c.ToHash),
			})
		}
	}
	return ev, nil
}

// zeroSHA reports an all-zero commit id, which git hosts use for a deleted ref.
func zeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
