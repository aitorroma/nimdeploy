package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration lets TOML values like "20m" decode into a time.Duration.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

type Config struct {
	Server  ServerConfig             `toml:"server"`
	Logging LoggingConfig            `toml:"logging"`
	Notify  NotifyConfig             `toml:"notify"`
	GitHub  GitHubConfig             `toml:"github"`
	Deploy  map[string]*DeployConfig `toml:"deploy"`
}

type ServerConfig struct {
	Listen          string   `toml:"listen"`
	MaxBodyBytes    int64    `toml:"max_body_bytes"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
	// APITokenEnv names the env var holding the bearer token for /status and
	// /deploy. Without it /status is open and manual deploys are disabled.
	APITokenEnv string `toml:"api_token_env"`

	apiToken string
}

type LoggingConfig struct {
	Directory string `toml:"directory"`
	// Retain is the number of log files kept per deploy; 0 keeps everything.
	Retain int `toml:"retain"`
}

type NotifyConfig struct {
	// Format enables notifications: slack, discord, telegram or json.
	Format string `toml:"format"`
	// On is "failure" (failures and recoveries), "always" or "never".
	On               string `toml:"on"`
	URLEnv           string `toml:"url_env"`
	TelegramTokenEnv string `toml:"telegram_token_env"`
	TelegramChatID   string `toml:"telegram_chat_id"`
	LogLines         int    `toml:"log_lines"`

	url           string
	telegramToken string
}

type GitHubConfig struct {
	// TokenEnv enables commit statuses; the token needs "Commit statuses: write".
	TokenEnv string `toml:"token_env"`
	APIURL   string `toml:"api_url"`

	token string
}

type DeployConfig struct {
	Name string `toml:"-"`

	Path       string `toml:"path"`
	Repository string `toml:"repository"`
	Branch     string `toml:"branch"`
	SecretEnv  string `toml:"secret_env"`

	WorkingDirectory string   `toml:"working_directory"`
	Command          string   `toml:"command"`
	Args             []string `toml:"args"`
	Env              []string `toml:"env"`

	Timeout   Duration `toml:"timeout"`
	Lock      *bool    `toml:"lock"`
	Queue     *bool    `toml:"queue"`
	LogOutput *bool    `toml:"log_output"`

	secret    []byte
	lock      bool
	queue     bool
	logOutput bool
}

var (
	deployNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	hookPathRe   = regexp.MustCompile(`^/[A-Za-z0-9/_.-]+$`)
	envKeyRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

const defaultDeployTimeout = 30 * time.Minute

// LoadConfig parses and validates the config file. Secrets are read from the
// environment separately by ResolveSecrets, so CLI commands work without them.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Listen:          "127.0.0.1:9000",
			MaxBodyBytes:    25 << 20, // GitHub caps webhook payloads at 25 MB
			ShutdownTimeout: Duration{5 * time.Minute},
		},
		Logging: LoggingConfig{
			Directory: "/var/log/nimdeploy",
			Retain:    30,
		},
		Notify: NotifyConfig{
			On:       "failure",
			LogLines: 20,
		},
		GitHub: GitHubConfig{
			APIURL: "https://api.github.com",
		},
	}

	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	if c.Server.MaxBodyBytes <= 0 {
		return fmt.Errorf("server.max_body_bytes must be positive")
	}
	if !filepath.IsAbs(c.Logging.Directory) {
		return fmt.Errorf("logging.directory must be an absolute path")
	}
	if c.Logging.Retain < 0 {
		return fmt.Errorf("logging.retain must be >= 0")
	}
	if err := c.Notify.validate(); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	if len(c.Deploy) == 0 {
		return fmt.Errorf("no [deploy.<name>] sections defined")
	}

	paths := map[string]string{}
	for name, d := range c.Deploy {
		d.Name = name
		if err := d.validate(); err != nil {
			return fmt.Errorf("deploy.%s: %w", name, err)
		}
		if other, dup := paths[d.Path]; dup {
			return fmt.Errorf("deploy.%s: path %s already used by deploy.%s", name, d.Path, other)
		}
		paths[d.Path] = name
	}
	return nil
}

func (n *NotifyConfig) validate() error {
	switch n.On {
	case "failure", "always", "never":
	default:
		return fmt.Errorf("on must be failure, always or never")
	}
	switch n.Format {
	case "":
	case "slack", "discord", "json":
		if n.URLEnv == "" {
			return fmt.Errorf("url_env is required for format %s", n.Format)
		}
	case "telegram":
		if n.TelegramTokenEnv == "" || n.TelegramChatID == "" {
			return fmt.Errorf("telegram_token_env and telegram_chat_id are required for format telegram")
		}
	default:
		return fmt.Errorf("format must be slack, discord, telegram or json")
	}
	if n.LogLines < 0 {
		return fmt.Errorf("log_lines must be >= 0")
	}
	return nil
}

func (d *DeployConfig) validate() error {
	if !deployNameRe.MatchString(d.Name) {
		return fmt.Errorf("invalid deploy name (allowed: letters, digits, _ . -)")
	}
	if !hookPathRe.MatchString(d.Path) {
		return fmt.Errorf("path must start with / and contain only letters, digits, / _ . -")
	}
	for _, reserved := range []string{"/status", "/deploy", "/healthz"} {
		if d.Path == reserved || strings.HasPrefix(d.Path, reserved+"/") {
			return fmt.Errorf("path %s is reserved", d.Path)
		}
	}
	if d.Repository == "" {
		return fmt.Errorf("repository is required")
	}
	if d.Branch == "" {
		d.Branch = "main"
	}
	if d.SecretEnv == "" {
		return fmt.Errorf("secret_env is required")
	}
	if d.Command == "" {
		return fmt.Errorf("command is required")
	}
	if d.WorkingDirectory != "" && !filepath.IsAbs(d.WorkingDirectory) {
		return fmt.Errorf("working_directory must be an absolute path")
	}
	for _, kv := range d.Env {
		if !envKeyRe.MatchString(kv) {
			return fmt.Errorf("env entry %q must be KEY=VALUE", kv)
		}
	}
	if d.Timeout.Duration == 0 {
		d.Timeout.Duration = defaultDeployTimeout
	}
	if d.Timeout.Duration < 0 {
		return fmt.Errorf("timeout must be positive")
	}
	d.lock = d.Lock == nil || *d.Lock
	d.queue = d.Queue == nil || *d.Queue
	d.logOutput = d.LogOutput == nil || *d.LogOutput
	return nil
}

// ResolveSecrets reads every *_env setting from the environment.
func (c *Config) ResolveSecrets() error {
	get := func(key, name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("%s: environment variable %s is empty or not set", key, name)
		}
		return v, nil
	}
	var err error
	for _, name := range c.DeployNames() {
		d := c.Deploy[name]
		s, e := get("deploy."+name+".secret_env", d.SecretEnv)
		if e != nil {
			return e
		}
		d.secret = []byte(s)
	}
	if c.Server.APITokenEnv != "" {
		if c.Server.apiToken, err = get("server.api_token_env", c.Server.APITokenEnv); err != nil {
			return err
		}
	}
	if c.Notify.URLEnv != "" {
		if c.Notify.url, err = get("notify.url_env", c.Notify.URLEnv); err != nil {
			return err
		}
	}
	if c.Notify.TelegramTokenEnv != "" {
		if c.Notify.telegramToken, err = get("notify.telegram_token_env", c.Notify.TelegramTokenEnv); err != nil {
			return err
		}
	}
	if c.GitHub.TokenEnv != "" {
		if c.GitHub.token, err = get("github.token_env", c.GitHub.TokenEnv); err != nil {
			return err
		}
	}
	return nil
}

// DeployNames returns deploy names in a stable order.
func (c *Config) DeployNames() []string {
	names := make([]string, 0, len(c.Deploy))
	for name := range c.Deploy {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// secretEnvNames lists every env var holding a secret, so they can be kept
// out of deploy commands.
func (c *Config) secretEnvNames() []string {
	names := []string{c.Server.APITokenEnv, c.Notify.URLEnv, c.Notify.TelegramTokenEnv, c.GitHub.TokenEnv}
	for _, d := range c.Deploy {
		names = append(names, d.SecretEnv)
	}
	return names
}
