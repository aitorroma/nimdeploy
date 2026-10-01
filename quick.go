package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// quickDeploy is the deploy `nimdeploy install --repo ... --command ...`
// writes, so whoever installs gets a working setup in one command.
type quickDeploy struct {
	repo, provider, branch, dir, command, name, listen string

	secretEnv, hookPath string
	secret              string  // the webhook secret to give the git host
	cfg                 *Config // the validated config
	cfgDeploys          []string
}

var nonEnvChars = regexp.MustCompile(`[^A-Z0-9_]+`)

func (q *quickDeploy) complete() error {
	if q.repo == "" || q.command == "" {
		return fmt.Errorf("--repo and --command go together")
	}
	if _, ok := providers[q.provider]; !ok {
		return fmt.Errorf("--provider must be one of: %s", strings.Join(providerNames(), ", "))
	}
	if q.name == "" {
		q.name = strings.ToLower(path.Base(strings.TrimSuffix(q.repo, ".git")))
	}
	if !deployNameRe.MatchString(q.name) {
		return fmt.Errorf("deploy name %q: use letters, digits, _ . - (set it with --name)", q.name)
	}
	if q.dir == "" {
		q.dir, _ = os.Getwd()
	}
	dir, err := filepath.Abs(q.dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("--dir %s is not a directory", dir)
	}
	q.dir = dir
	if _, _, err := net.SplitHostPort(q.listen); err != nil && !strings.HasPrefix(q.listen, "unix:") {
		return fmt.Errorf("--listen must be host:port or unix:/path")
	}
	q.secretEnv = strings.Trim(nonEnvChars.ReplaceAllString(strings.ToUpper(q.name), "_"), "_") + "_WEBHOOK_SECRET"
	q.hookPath = "/hooks/" + q.name
	return nil
}

func (q *quickDeploy) section() string {
	return fmt.Sprintf(`
[deploy.%s]
provider = %q
path = %q
repository = %q
branch = %q
secret_env = %q
working_directory = %q
# Runs with bash; -e and pipefail stop at the first failing command.
command = "/bin/bash"
args = ["-eo", "pipefail", "-c", %q]
timeout = "30m"
`, q.name, q.provider, q.hookPath, q.repo, q.branch, q.secretEnv, q.dir, q.command)
}

// writeConfig creates the config with this deploy, or adds the deploy to an
// existing config that doesn't have it yet.
func (q *quickDeploy) writeConfig(p userPaths) error {
	file := p.at(p.config)
	existing, err := os.ReadFile(file)
	switch {
	case os.IsNotExist(err):
		content := fmt.Sprintf(`# nimdeploy, created by "nimdeploy install". Every option is described in
# https://github.com/aitorroma/nimdeploy/blob/main/config.example.toml

[server]
listen = %q
api_token_env = "NIMDEPLOY_API_TOKEN"

[logging]
directory = %q
retain = 30
%s`, q.listen, p.logs, q.section())
		if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
			return err
		}
		step("config   %s (deploy.%s)", p.short(p.config), q.name)
	case err != nil:
		return err
	case regexp.MustCompile(`(?m)^\[deploy\.` + regexp.QuoteMeta(q.name) + `\]`).Match(existing):
		step("config   %s (deploy.%s already there, kept)", p.short(p.config), q.name)
	default:
		f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteString(q.section())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		step("config   %s (deploy.%s added)", p.short(p.config), q.name)
	}
	return nil
}

// writeSecret makes sure the deploy's webhook secret exists and remembers it.
func (q *quickDeploy) writeSecret(p userPaths) error {
	file := p.at(p.secrets)
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		b = []byte("# nimdeploy secrets (chmod 600). Restart after changing: systemctl --user restart nimdeploy\n")
	} else if err != nil {
		return err
	}
	line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(q.secretEnv) + `=(.*)$`)
	if m := line.FindSubmatch(b); m != nil {
		if v := strings.Trim(strings.TrimSpace(string(m[1])), `"'`); v != "" && v != placeholderSecret {
			q.secret = v
			step("secrets  %s (%s kept)", p.short(p.secrets), q.secretEnv)
			return os.WriteFile(file, b, 0o600)
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	q.secret = hex.EncodeToString(buf)
	if line.Match(b) {
		b = line.ReplaceAll(b, []byte(q.secretEnv+"="+q.secret))
	} else {
		b = append(b, []byte(q.secretEnv+"="+q.secret+"\n")...)
	}
	step("secrets  %s (%s generated)", p.short(p.secrets), q.secretEnv)
	return os.WriteFile(file, b, 0o600)
}

// validate checks the resulting config exactly like the service will.
func (q *quickDeploy) validate(p userPaths) error {
	q.cfgDeploys = deploySections(p.at(p.config))
	cfg, err := LoadConfig(p.at(p.config))
	if err != nil {
		return fmt.Errorf("%s does not validate: %w", p.short(p.config), err)
	}
	b, err := os.ReadFile(p.at(p.secrets))
	if err != nil {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if k, v, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(l, "#") {
			os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	if err := cfg.ResolveSecrets(); err != nil {
		return fmt.Errorf("%s: %w", p.short(p.secrets), err)
	}
	q.cfg = cfg
	return nil
}

var webhookWhere = map[string]string{
	"github":    "Settings → Webhooks → Add webhook; content type application/json; Just the push event",
	"gitea":     "Settings → Webhooks → Add webhook → Gitea; POST, application/json; Push events",
	"forgejo":   "Settings → Webhooks → Add webhook → Forgejo; POST, application/json; Push events",
	"gitlab":    "Settings → Webhooks → Add new webhook; the secret goes in \"Secret token\"; Push events",
	"bitbucket": "Repository settings → Webhooks → Add webhook; Secret; trigger Repository push",
}

// report prints what the git host and the reverse proxy need.
func (q *quickDeploy) report(p userPaths, cmd string) {
	d := q.cfg.Deploy[q.name]
	hookPath := q.cfg.Server.BasePath + d.Path
	fmt.Printf(`
Webhook for %s (%s, branch %s):
  URL      https://<the domain nginx serves>%s
  Secret   %s
  Where    %s

nginx: add this inside the site's server { } block (it is also printed by "%s nginx"):

%s`, d.Repository, d.Provider, d.Branch, hookPath, q.secret, webhookWhere[d.Provider], cmd,
		indent(nginxSnippet(q.cfg, false), "    "))
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

var deploySectionRe = regexp.MustCompile(`(?m)^\[deploy\.([^\]]+)\]`)

func deploySections(file string) []string {
	b, _ := os.ReadFile(file)
	var names []string
	for _, m := range deploySectionRe.FindAllSubmatch(b, -1) {
		names = append(names, string(m[1]))
	}
	return names
}
