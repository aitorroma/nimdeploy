package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeployOnTags: with tags = "v*" a deploy runs on new version tags and
// ignores pushes to its branch; rollback and manual runs work with tags.
func TestDeployOnTags(t *testing.T) {
	e := newEnv(t, `echo "tag=$DEPLOY_TAG commit=$DEPLOY_COMMIT ref=$DEPLOY_REF branch=$DEPLOY_BRANCH"`, `[server]
api_token_env = "TEST_API_TOKEN"
`)
	e.cfg.Deploy["agency"].Tags = "v*"

	for ref, want := range map[string]string{
		"refs/heads/main":      "is not a tag matching v*",
		"refs/tags/release-1":  "is not a tag matching v*",
		"refs/heads/v1-branch": "is not a tag matching v*",
	} {
		rec := e.push(t, pushOpts{ref: ref, delivery: "ign-" + ref})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", ref, rec.Code, rec.Body)
		}
	}

	rec := e.push(t, pushOpts{ref: "refs/tags/v1.0.0", commit: sha1, delivery: "t1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("tag push: %d %s", rec.Code, rec.Body)
	}
	st := e.waitIdle(t)
	if st.Status != StatusSuccess || st.Tag != "v1.0.0" || st.Commit != sha1 || st.Branch != "main" {
		t.Fatalf("state %+v", st)
	}
	if out := readLatest(t, e, "agency"); !strings.Contains(out, "tag=v1.0.0 commit="+sha1+" ref=refs/tags/v1.0.0 branch=main") {
		t.Errorf("script environment:\n%s", out)
	}

	// An annotated tag: GitHub sends the tag object as "after" and the
	// commit as head_commit.id; the commit is what gets deployed.
	body := []byte(`{"ref":"refs/tags/v1.1.0","after":"` + sha3 + `","repository":{"full_name":"acme/agency"},"pusher":{"name":"dev"},"head_commit":{"id":"` + sha2 + `","message":"release"}}`)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, jsonRequest("/hooks/agency", body, map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "t2", "X-Hub-Signature-256": sign(body)}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("annotated tag: %d %s", rec.Code, rec.Body)
	}
	if st := e.waitIdle(t); st.Tag != "v1.1.0" || st.Commit != sha2 {
		t.Fatalf("annotated tag state %+v", st)
	}

	// Deleting a tag deploys nothing.
	if rec := e.push(t, pushOpts{ref: "refs/tags/v1.1.0", commit: "0000000000000000000000000000000000000000", delivery: "t3"}); !strings.Contains(rec.Body.String(), "tag deleted") {
		t.Errorf("deleted tag: %s", rec.Body)
	}

	// Rollback goes back to the previous version, tag included.
	rec = e.request(http.MethodPost, "/rollback/agency", testToken, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body)
	}
	if st := e.waitIdle(t); st.Trigger != TriggerRollback || st.Tag != "v1.0.0" || st.Commit != sha1 {
		t.Fatalf("rollback state %+v", st)
	}
	if out := readLatest(t, e, "agency"); !strings.Contains(out, "tag=v1.0.0 commit="+sha1+" ref=refs/tags/v1.0.0") {
		t.Errorf("rollback environment:\n%s", out)
	}

	// A manual run needs the version: the branch head is not one.
	for body, want := range map[string]int{
		`{}`:                        http.StatusBadRequest,
		`{"tag":"release-2"}`:       http.StatusBadRequest,
		`{"tag":"v1;rm -rf"}`:       http.StatusBadRequest,
		`{"tag":"v1.1.0"}`:          http.StatusAccepted,
		`{"commit":"` + sha2 + `"}`: http.StatusAccepted,
	} {
		rec := e.request(http.MethodPost, "/deploy/agency", testToken, body)
		if rec.Code != want {
			t.Errorf("run %s: %d %s", body, rec.Code, rec.Body)
		}
		e.waitIdle(t)
	}

	// Run again from the hub: the last version deployed.
	if last, ok := e.runner.lastTagged("agency"); !ok || last.Tag == "" {
		t.Errorf("last tagged %+v %v", last, ok)
	}
}

// TestTagPushProviders: each git host's tag push, with the commit the tag
// points to.
func TestTagPushProviders(t *testing.T) {
	for _, c := range []struct {
		name    string
		parse   func(*http.Request, []byte) (hookEvent, error)
		headers map[string]string
		body    string
		ref     string
		commit  string
	}{
		{"gitlab", parseGitLab, map[string]string{"X-Gitlab-Event": "Tag Push Hook"},
			`{"object_kind":"tag_push","ref":"refs/tags/v2.0.0","after":"` + sha3 + `","checkout_sha":"` + sha2 + `","project":{"path_with_namespace":"acme/agency"},"user_username":"dev"}`,
			"refs/tags/v2.0.0", sha2},
		{"gitea", parseGitea, map[string]string{"X-Gitea-Event": "push"},
			`{"ref":"refs/tags/v2.0.0","after":"` + sha3 + `","repository":{"full_name":"acme/agency"},"head_commit":{"id":"` + sha2 + `"}}`,
			"refs/tags/v2.0.0", sha2},
		{"gitea, no head commit", parseGitea, map[string]string{"X-Gitea-Event": "push"},
			`{"ref":"refs/tags/v2.0.0","after":"` + sha2 + `","repository":{"full_name":"acme/agency"},"head_commit":null}`,
			"refs/tags/v2.0.0", sha2},
		{"bitbucket", parseBitbucket, map[string]string{"X-Event-Key": "repo:push"},
			`{"repository":{"full_name":"acme/agency"},"push":{"changes":[{"new":{"type":"tag","name":"v2.0.0","target":{"hash":"` + sha2 + `"}}}]}}`,
			"refs/tags/v2.0.0", sha2},
	} {
		req := jsonRequest("/", []byte(c.body), c.headers)
		ev, err := c.parse(req, []byte(c.body))
		if err != nil || !ev.Push || len(ev.Pushes) != 1 || ev.Pushes[0].Ref != c.ref || ev.Pushes[0].Commit != c.commit || ev.Pushes[0].Deleted {
			t.Errorf("%s: %+v %v", c.name, ev, err)
		}
	}
}

func TestTagsConfig(t *testing.T) {
	for toml, wantErr := range map[string]string{
		"tags = \"v*\"\nprovider = \"generic\"\nbranch = \"\"": "tags is for git providers",
		"tags = \"refs/tags/v*\"":                              "a tag name pattern",
		"tags = \"v[\"":                                        "a tag name pattern",
	} {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte("[deploy.a]\npath = \"/a\"\nrepository = \"a/b\"\nsecret_env = \"X\"\ncommand = \"true\"\n"+toml+"\n"), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%q: err %v", toml, err)
		}
	}
}
