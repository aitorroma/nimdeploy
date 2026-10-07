package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Labels describe a deploy for people and tools: client, environment,
// project, url... Global ones ([labels]) apply to every deploy; a deploy's
// own ([deploy.<name>.labels]) are added on top and win on conflicts. They
// show up in notifications, status/history, /metrics, scripts
// (DEPLOY_LABEL_<KEY>) and email templates, and travel to the hub.

var labelKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

const (
	maxLabels        = 32
	maxLabelValueLen = 200
)

func validateLabels(where string, labels map[string]string) error {
	if len(labels) > maxLabels {
		return fmt.Errorf("%s: at most %d labels", where, maxLabels)
	}
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("%s: label %q: keys are lowercase letters, digits and _ (max 32)", where, k)
		}
		if k == "deploy" {
			return fmt.Errorf("%s: label key \"deploy\" is reserved", where)
		}
		if len(v) > maxLabelValueLen {
			return fmt.Errorf("%s: label %s: value longer than %d", where, k, maxLabelValueLen)
		}
		for _, r := range v {
			if unicode.IsControl(r) {
				return fmt.Errorf("%s: label %s: control characters are not allowed", where, k)
			}
		}
	}
	return nil
}

func mergeLabels(global, own map[string]string) map[string]string {
	if len(global) == 0 && len(own) == 0 {
		return nil
	}
	out := make(map[string]string, len(global)+len(own))
	for k, v := range global {
		out[k] = v
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}

func copyLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// labelTitle is how a deploy is named in messages: the well-known labels
// that are set (client, environment, project), then the deploy name.
func labelTitle(labels map[string]string, deploy string) string {
	var parts []string
	for _, k := range []string{"client", "environment", "project"} {
		if v := labels[k]; v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(append(parts, deploy), " · ")
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, " ")
}

// labelFilter is "key=value" conditions; all must match.
type labelFilter map[string]string

func parseLabelFilter(specs []string) (labelFilter, error) {
	f := labelFilter{}
	for _, s := range specs {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("label filter %q: want key=value", s)
		}
		f[k] = v
	}
	return f, nil
}

func (f labelFilter) match(labels map[string]string) bool {
	for k, v := range f {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func labelEnv(labels map[string]string) []string {
	env := make([]string, 0, len(labels))
	for k, v := range labels {
		env = append(env, "DEPLOY_LABEL_"+strings.ToUpper(k)+"="+v)
	}
	sort.Strings(env)
	return env
}
