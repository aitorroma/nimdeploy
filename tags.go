package main

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

// tagNameRe is a tag a manual run accepts (it reaches the script's environment).
var tagNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,127}$`)

// Deploys on tags. A git deploy with tags = "v*" runs when a tag matching
// the pattern is pushed (or created with a release on the git host) and
// ignores pushes to its branch: commits can land on main without reaching
// production, and every production deploy is a version. The script gets
// DEPLOY_TAG and DEPLOY_COMMIT (the commit the tag points to).

// matchPush picks the push this deploy runs: one to its branch, or with tags
// a new tag matching the pattern. Without a match, why says what was pushed.
func (d *DeployConfig) matchPush(pushes []pushInfo) (match *pushInfo, tag, why string) {
	var refs []string
	for i, p := range pushes {
		refs = append(refs, p.Ref)
		if d.Tags != "" {
			if name, ok := strings.CutPrefix(p.Ref, "refs/tags/"); ok && d.tagMatches(name) {
				match, tag = &pushes[i], name
			}
		} else if branch, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok && branch == d.Branch {
			match = &pushes[i]
		}
	}
	if match != nil {
		return match, tag, ""
	}
	if d.Tags != "" {
		return nil, "", "ref " + strings.Join(refs, ", ") + " is not a tag matching " + d.Tags
	}
	return nil, "", "ref " + strings.Join(refs, ", ") + " is not branch " + d.Branch
}

func (d *DeployConfig) tagMatches(name string) bool {
	ok, _ := path.Match(d.Tags, name)
	return ok
}

// setRef fills a git trigger's ref: the tag when there is one, else the branch.
func (d *DeployConfig) setRef(t *Trigger, tag string) {
	t.Branch, t.Tag = d.Branch, tag
	if tag != "" {
		t.Ref = "refs/tags/" + tag
	} else {
		t.Ref = "refs/heads/" + d.Branch
	}
}

// checkManualTag validates the tag of a manual run, rollback or hub command.
// A deploy with tags needs one (or a commit): its branch head is not a version.
func (d *DeployConfig) checkManualTag(tag, commit string) error {
	if tag == "" {
		if d.Tags != "" && commit == "" {
			return errors.New("deploy " + d.Name + " deploys tags (" + d.Tags + "): give the tag to deploy")
		}
		return nil
	}
	if _, git := providers[d.Provider]; !git {
		return errors.New("tag needs a git deploy")
	}
	if !tagNameRe.MatchString(tag) || strings.Contains(tag, "..") {
		return errors.New("tag: letters, digits and . _ - / +")
	}
	if d.Tags != "" && !d.tagMatches(tag) {
		return errors.New("tag " + tag + " does not match " + d.Tags)
	}
	return nil
}

// lastTagged is the most recent run of a deploy that had a tag.
func (r *Runner) lastTagged(name string) (State, bool) {
	h, err := r.History(name, 0)
	if err != nil {
		return State{}, false
	}
	for _, st := range h {
		if st.Tag != "" && st.Commit != "" {
			return st, true
		}
	}
	return State{}, false
}

// refLabel is how a deployed version reads: "v1.2.0 · a3f9c21", or the commit.
func refLabel(tag, commit string) string {
	if tag == "" {
		return shortSHA(commit)
	}
	if commit == "" {
		return tag
	}
	return tag + " · " + shortSHA(commit)
}
