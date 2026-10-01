package main

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The templates a user install writes; the binary is all that's needed.
//
//go:embed config.example.toml deploy/secrets.env.example deploy/nimdeploy-user.service
var assets embed.FS

// userPaths are where a user install puts things. Real paths are what the
// config and messages refer to; files are written under destdir+real.
type userPaths struct {
	destdir                                      string
	home, bin, conf, config, secrets, logs, unit string
}

func newUserPaths(destdir string) (userPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return userPaths{}, err
	}
	confHome := os.Getenv("XDG_CONFIG_HOME")
	if confHome == "" {
		confHome = filepath.Join(home, ".config")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	conf := filepath.Join(confHome, "nimdeploy")
	return userPaths{
		destdir: destdir,
		home:    home,
		bin:     filepath.Join(home, ".local", "bin", "nimdeploy"),
		conf:    conf,
		config:  filepath.Join(conf, "config.toml"),
		secrets: filepath.Join(conf, "secrets.env"),
		logs:    filepath.Join(stateHome, "nimdeploy"),
		unit:    filepath.Join(confHome, "systemd", "user", "nimdeploy.service"),
	}, nil
}

// at returns where to write a real path.
func (p userPaths) at(real string) string { return filepath.Join(p.destdir, real) }

// short shows a path with ~ for the home directory.
func (p userPaths) short(path string) string {
	if rest, ok := strings.CutPrefix(path, p.home+"/"); ok {
		return "~/" + rest
	}
	return path
}

func step(format string, args ...any) { fmt.Printf("\033[1;32m==>\033[0m "+format+"\n", args...) }
func caution(format string, args ...any) {
	fmt.Printf("\033[1;33m==>\033[0m "+format+"\n", args...)
}

func cliInstall(args []string) int {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	userMode := fset.Bool("user", false, "install for the current user (the default when not root)")
	destdir := fset.String("destdir", "", "write files under this directory and skip systemctl (packaging, tests)")
	var q quickDeploy
	fset.StringVar(&q.repo, "repo", "", "configure a deploy for this repository (owner/repo); without it an example config is written")
	fset.StringVar(&q.provider, "provider", "github", "git host: github, gitea, forgejo, gitlab, bitbucket")
	fset.StringVar(&q.branch, "branch", "main", "branch that deploys")
	fset.StringVar(&q.dir, "dir", "", "working directory of the deploy (the checkout); default: current directory")
	fset.StringVar(&q.command, "command", "", "what to run, e.g. ./deploy.sh or \"git pull && npm ci && npm run build\" (bash, stops at the first error)")
	fset.StringVar(&q.name, "name", "", "deploy name (default: the repository name)")
	fset.StringVar(&q.listen, "listen", "127.0.0.1:9000", "address nimdeploy listens on, for the reverse proxy")
	fset.Usage = func() {
		fmt.Fprint(fset.Output(), `Usage: nimdeploy install [flags]

Installs nimdeploy for the current user, without root: binary in ~/.local/bin,
config in ~/.config/nimdeploy, logs in ~/.local/state/nimdeploy, systemd user service.

With --repo and --command it also configures the deploy, generates its webhook
secret, starts the service and prints what to set in the git host and nginx:

  nimdeploy install --repo acme/shop --dir /srv/shop --command ./deploy.sh

Flags:
`)
		fset.PrintDefaults()
	}
	fset.Parse(args)
	if os.Geteuid() == 0 && !*userMode && *destdir == "" {
		fmt.Fprintln(os.Stderr, "as root, install the system service with install.sh from the release archive\n(service user, /usr/local/bin, /etc/nimdeploy, /var/log/nimdeploy).\nTo install into root's own home anyway: nimdeploy install --user")
		return 2
	}
	p, err := newUserPaths(*destdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var quick *quickDeploy
	if q.repo != "" || q.command != "" {
		if err := q.complete(); err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 2
		}
		quick = &q
	}
	live := *destdir == ""
	if live {
		if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "no systemd user manager (systemctl --user: %s).\nLog in with SSH or a desktop session, not su/sudo, and retry.\n", strings.TrimSpace(string(out)))
			return 1
		}
	}
	if err := writeUserInstall(p, quick); err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		return 1
	}
	if !live {
		step("staged under %s", *destdir)
		return 0
	}
	return startAndReport(p, quick)
}

// writeUserInstall puts the binary, config, secrets, log dir and unit in
// place. Existing config and secrets are kept.
func writeUserInstall(p userPaths, quick *quickDeploy) (err error) {
	if err := installSelf(p.at(p.bin)); err != nil {
		return fmt.Errorf("binary: %w", err)
	}
	step("binary   %s", p.short(p.bin))

	if err := os.MkdirAll(p.at(p.conf), 0o750); err != nil {
		return err
	}
	if quick != nil {
		// If the result doesn't validate, leave config and secrets as they were.
		restore := snapshot(p.at(p.config), p.at(p.secrets))
		defer func() {
			if err != nil {
				restore()
				caution("nothing changed in %s", p.short(p.conf))
			}
		}()
	}
	switch {
	case quick != nil:
		if err := quick.writeConfig(p); err != nil {
			return err
		}
	case exists(p.at(p.config)):
		step("config   %s (kept)", p.short(p.config))
	default:
		example, _ := assets.ReadFile("config.example.toml")
		cfg := strings.Replace(string(example), `directory = "/var/log/nimdeploy"`, fmt.Sprintf("directory = %q", p.logs), 1)
		if err := os.WriteFile(p.at(p.config), []byte(cfg), 0o640); err != nil {
			return err
		}
		step("config   %s (example, edit it)", p.short(p.config))
	}

	switch {
	case quick != nil:
		if err := quick.writeSecret(p); err != nil {
			return err
		}
	case exists(p.at(p.secrets)):
		step("secrets  %s (kept)", p.short(p.secrets))
	default:
		tmpl, _ := assets.ReadFile("deploy/secrets.env.example")
		if err := os.WriteFile(p.at(p.secrets), tmpl, 0o600); err != nil {
			return err
		}
		step("secrets  %s (template, edit it)", p.short(p.secrets))
	}
	if generated, err := ensureAPIToken(p.at(p.secrets)); err != nil {
		return fmt.Errorf("api token: %w", err)
	} else if generated {
		step("api token generated in %s", p.short(p.secrets))
	}

	if err := os.MkdirAll(p.at(p.logs), 0o750); err != nil {
		return err
	}
	step("logs     %s/<deploy>/", p.short(p.logs))

	if quick != nil {
		if err := quick.validate(p); err != nil {
			if len(quick.cfgDeploys) > 1 {
				err = fmt.Errorf("%w\n\nThe config already had other deploys that are not ready. Fix them, or start from\nscratch (deletes config, secrets and deploy logs):  nimdeploy uninstall --purge", err)
			}
			return err
		}
	}

	unit, _ := assets.ReadFile("deploy/nimdeploy-user.service")
	if err := os.MkdirAll(filepath.Dir(p.at(p.unit)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.at(p.unit), unit, 0o644); err != nil {
		return err
	}
	step("service  %s", p.short(p.unit))
	return nil
}

// installSelf copies the running binary to dst (atomically, so a running
// nimdeploy keeps working until it restarts).
func installSelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(dst); err == nil && real == src {
		return nil // already running from there
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".nimdeploy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

var apiTokenLine = regexp.MustCompile(`(?m)^NIMDEPLOY_API_TOKEN=(.*)$`)

// ensureAPIToken fills in NIMDEPLOY_API_TOKEN when it's missing or empty.
func ensureAPIToken(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	m := apiTokenLine.FindSubmatch(b)
	if m != nil && len(strings.TrimSpace(string(m[1]))) > 0 {
		return false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return false, err
	}
	line := "NIMDEPLOY_API_TOKEN=" + hex.EncodeToString(buf)
	if m != nil {
		b = apiTokenLine.ReplaceAll(b, []byte(line))
	} else {
		b = append(b, []byte("\n# API token for /status, /deploy and nimdeploy run\n"+line+"\n")...)
	}
	return true, os.WriteFile(path, b, 0o600)
}

// hasPlaceholders reports secrets still set to the template's value.
func hasPlaceholders(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if _, v, ok := strings.Cut(line, "="); ok && strings.Trim(strings.TrimSpace(v), `"'`) == placeholderSecret {
			return true
		}
	}
	return false
}

func systemctlUser(args ...string) error {
	return exec.Command("systemctl", append([]string{"--user"}, args...)...).Run()
}

// startAndReport reloads systemd, starts the service when it is configured,
// sets up lingering and prints what's left to do.
func startAndReport(p userPaths, quick *quickDeploy) int {
	_ = systemctlUser("daemon-reload")
	configured := !hasPlaceholders(p.secrets)
	running := false
	if configured {
		wasActive := systemctlUser("is-active", "--quiet", "nimdeploy") == nil
		_ = systemctlUser("enable", "nimdeploy")
		action := "start"
		if wasActive {
			action = "restart"
		}
		if err := systemctlUser(action, "nimdeploy"); err == nil {
			time.Sleep(time.Second)
			running = systemctlUser("is-active", "--quiet", "nimdeploy") == nil
		}
		if running {
			step("nimdeploy is running (%sed)", action)
		} else {
			caution("nimdeploy did not start: journalctl --user -u nimdeploy -e")
		}
	} else {
		// An enabled unit would start on the next login or boot with the
		// template's secrets; nimdeploy refuses those anyway.
		_ = systemctlUser("disable", "nimdeploy")
	}

	who := currentUser()
	linger := lingerEnabled(who)
	lingerNow := false
	if !linger && exec.Command("loginctl", "enable-linger", who).Run() == nil {
		linger, lingerNow = true, true
	}
	pathOK := slices.Contains(filepath.SplitList(os.Getenv("PATH")), filepath.Dir(p.bin))
	cmd := "nimdeploy"
	if !pathOK {
		cmd = p.short(p.bin)
	}

	fmt.Printf("\nnimdeploy %s installed for %s, no root needed.\n", version, who)
	if lingerNow {
		fmt.Println("Lingering enabled: it keeps running after you log out and starts on boot.")
	}

	if quick != nil && configured {
		quick.report(p, cmd)
	}

	fmt.Println("\nNext steps:")
	n := 1
	item := func(format string, args ...any) {
		fmt.Printf("  %d. %s\n", n, fmt.Sprintf(format, args...))
		n++
	}
	if quick != nil && configured {
		item("Give the nginx block above to whoever manages nginx.")
		item("Create the webhook in %s with the URL and secret above.", quick.provider)
	}
	if !configured {
		item("Edit %s: one [deploy.<name>] per app\n     (provider, repository, branch, working_directory, command).", p.short(p.config))
		item("Put each deploy's webhook secret in %s\n     (openssl rand -hex 32) and the same secret in the git host's webhook.", p.short(p.secrets))
		item("Start it:     systemctl --user enable --now nimdeploy")
	} else if !running {
		item("Fix the config, then:  systemctl --user restart nimdeploy")
	}
	item("Check it:     %s status\n                   %s run -f <deploy>\n                   journalctl --user -u nimdeploy -f", cmd, cmd)
	if quick == nil {
		item("Reverse proxy: %s nginx   prints the nginx blocks for the hooks;\n     hand them to whoever manages nginx.", cmd)
	}
	item("Config changes: systemctl --user reload nimdeploy (secrets: restart).")

	if !linger {
		fmt.Printf("\n\033[1;33mImportant:\033[0m lingering is off and %s may not enable it, so nimdeploy stops\n"+
			"when your last session ends and does not start on boot. Ask an administrator to run once:\n\n"+
			"    sudo loginctl enable-linger %s\n", who, who)
	}
	if !pathOK {
		fmt.Printf("\n%s is not in your PATH. Add this to ~/.bashrc (or ~/.profile):\n\n"+
			"    export PATH=\"$HOME/.local/bin:$PATH\"\n", p.short(filepath.Dir(p.bin)))
	}
	return 0
}

func cliUninstall(args []string) int {
	fset := flag.NewFlagSet("uninstall", flag.ExitOnError)
	fset.Bool("user", false, "uninstall the current user's install (the default)")
	purge := fset.Bool("purge", false, "also delete the config, secrets and deploy logs")
	destdir := fset.String("destdir", "", "work under this directory and skip systemctl (packaging, tests)")
	fset.Parse(args)
	p, err := newUserPaths(*destdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *destdir == "" {
		_ = systemctlUser("disable", "--now", "nimdeploy")
	}
	for _, f := range []string{p.unit, p.bin} {
		if err := os.Remove(p.at(f)); err == nil {
			step("removed %s", p.short(f))
		} else if !errors.Is(err, fs.ErrNotExist) {
			caution("cannot remove %s: %v", p.short(f), err)
		}
	}
	if *destdir == "" {
		_ = systemctlUser("daemon-reload")
	}
	if *purge {
		for _, dir := range []string{p.conf, p.logs} {
			if err := os.RemoveAll(p.at(dir)); err == nil {
				step("removed %s", p.short(dir))
			}
		}
	} else {
		step("kept %s and %s (--purge removes them)", p.short(p.conf), p.short(p.logs))
	}
	step("nimdeploy uninstalled (lingering, if on, was left as is: loginctl disable-linger)")
	return 0
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func lingerEnabled(name string) bool {
	out, err := exec.Command("loginctl", "show-user", name, "-p", "Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// snapshot remembers files and returns a func that puts them back (or
// removes them if they didn't exist).
func snapshot(paths ...string) func() {
	saved := map[string][]byte{}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			saved[p] = b
		}
	}
	return func() {
		for _, p := range paths {
			if b, ok := saved[p]; ok {
				_ = os.WriteFile(p, b, 0o600)
			} else {
				_ = os.Remove(p)
			}
		}
	}
}
