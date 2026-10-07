package main

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	Server     ServerConfig             `toml:"server"`
	Logging    LoggingConfig            `toml:"logging"`
	Notify     NotifyConfig             `toml:"notify"`
	GitHub     GitHubConfig             `toml:"github"`
	Cloudflare CloudflareConfig         `toml:"cloudflare"`
	SMTP       SMTPConfig               `toml:"smtp"`
	Hub        HubConfig                `toml:"hub"`
	PwPush     PwPushConfig             `toml:"pwpush"`
	Deploy     map[string]*DeployConfig `toml:"deploy"`

	// Labels apply to every deploy (client, environment...); see labels.go.
	Labels map[string]string `toml:"labels"`

	path string // where it was loaded from, for scripts that call nimdeploy
}

type ServerConfig struct {
	// Listen is "host:port" or "unix:/path/to.sock".
	Listen          string   `toml:"listen"`
	MaxBodyBytes    int64    `toml:"max_body_bytes"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
	// APITokenEnv names the env var holding the bearer token for /status and
	// /deploy. Without it /status is open and manual deploys are disabled.
	APITokenEnv string `toml:"api_token_env"`
	// BasePath is a prefix every route lives under, for reverse proxies that
	// forward "/nimdeploy/hooks/x" without stripping "/nimdeploy".
	BasePath string `toml:"base_path"`
	// TrustedProxies are the addresses/CIDRs whose X-Forwarded-For and
	// X-Real-IP headers are believed. Unix socket peers are always trusted.
	// "cloudflare" expands to Cloudflare's edge ranges.
	TrustedProxies []string `toml:"trusted_proxies"`
	// ClientIPHeader, e.g. "CF-Connecting-IP", takes precedence over
	// X-Forwarded-For when the request comes from a trusted proxy.
	ClientIPHeader string `toml:"client_ip_header"`
	// SocketMode is the permission of the unix socket.
	SocketMode string `toml:"socket_mode"`

	apiToken   string
	socketPath string
	socketMode os.FileMode
	trusted    []netip.Prefix
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
	// TokenEnv is the GitHub token used for commit statuses ("Commit
	// statuses: write") and wait_for_ci ("Actions: read").
	TokenEnv string `toml:"token_env"`
	APIURL   string `toml:"api_url"`
	// CommitStatus posts deploy results as commit statuses (default true
	// when a token is set). Turn off for a read-only token.
	CommitStatus *bool `toml:"commit_status"`

	token        string
	commitStatus bool
}

// CloudflareConfig is the API access for cache purges after a deploy.
type CloudflareConfig struct {
	APITokenEnv string `toml:"api_token_env"` // token with Zone → Cache Purge
	APIURL      string `toml:"api_url"`

	token string
}

type DeployConfig struct {
	Name string `toml:"-"`

	// Provider is the git host sending the webhooks: github (default),
	// gitea, forgejo, gitlab or bitbucket (Cloud and Data Center); or
	// generic, for any JSON webhook (see generic.go).
	Provider   string `toml:"provider"`
	Path       string `toml:"path"`
	Repository string `toml:"repository"`
	Branch     string `toml:"branch"`
	SecretEnv  string `toml:"secret_env"`

	WorkingDirectory string   `toml:"working_directory"`
	Command          string   `toml:"command"`
	Args             []string `toml:"args"`
	Env              []string `toml:"env"`

	Timeout Duration `toml:"timeout"`
	// WaitForCI lists GitHub Actions workflow names that must succeed for the
	// pushed commit before a webhook deploy runs.
	WaitForCI []string `toml:"wait_for_ci"`
	CITimeout Duration `toml:"ci_timeout"`
	Lock      *bool    `toml:"lock"`
	Queue     *bool    `toml:"queue"`
	LogOutput *bool    `toml:"log_output"`

	// Generic webhooks: how they authenticate and identify themselves.
	Auth            string   `toml:"auth"`             // hmac (default) or token
	SignatureHeader string   `toml:"signature_header"` // hmac: "sha256=<hex>" or "<hex>"
	TokenHeader     string   `toml:"token_header"`     // token: "Authorization" means "Bearer <token>"
	TimestampHeader string   `toml:"timestamp_header"` // hmac: signs "<timestamp>.<body>", refuses old requests
	MaxSkew         Duration `toml:"max_skew"`
	DeliveryHeader  string   `toml:"delivery_header"` // unique ID per request, to drop duplicates
	PusherFrom      string   `toml:"pusher_from"`     // JSON path of who triggered it, for logs

	// WooCommerce (see woocommerce.go).
	StoreURL     string   `toml:"store_url"`      // the shop; also checked against X-WC-Webhook-Source
	WebhookURL   string   `toml:"webhook_url"`    // public URL of this hook, as the shop calls it (register)
	APIKeyEnv    string   `toml:"api_key_env"`    // REST API consumer key, for the CLI (register, replay, note)
	APISecretEnv string   `toml:"api_secret_env"` // REST API consumer secret
	Topics       []string `toml:"topics"`         // e.g. order.created, order.updated
	Statuses     []string `toml:"statuses"`       // order statuses that run, e.g. processing, completed

	// Any provider: run only when the JSON matches, and pass declared values on.
	When     map[string]any          `toml:"when"`
	Params   map[string]*ParamConfig `toml:"params"`
	QueueKey string                  `toml:"queue_key"` // param that gives each value its own lock and queue
	// QueueMode "latest" (default for git) keeps only the newest waiting run;
	// "all" (default for woocommerce) keeps every one, in order, on disk.
	QueueMode   string `toml:"queue_mode"`
	QueueMax    int    `toml:"queue_max"`
	PayloadFile *bool  `toml:"payload_file"` // pass the request body as DEPLOY_PAYLOAD_FILE

	// Labels of this deploy, added to the global [labels].
	Labels map[string]string `toml:"labels"`
	labels map[string]string // global + own

	// Email sent after the deploy (e.g. a welcome email with what it created).
	Email *EmailConfig `toml:"email"`

	// Payment providers (stripe, paddle, lemonsqueezy): event types that run.
	Events []string `toml:"events"`

	// Schedule runs the deploy on a cron schedule ("*/15 * * * *", "@daily",
	// "@every 10m"); a deploy with a schedule and no path has no webhook.
	Schedule string `toml:"schedule"`

	// Hooks around the command, run with bash in the same directory and log.
	Before       string `toml:"before"`        // fails → the deploy fails, the command doesn't run
	AfterSuccess string `toml:"after_success"` // e.g. artisan up, warm caches
	AfterFailure string `toml:"after_failure"` // e.g. artisan up, page someone
	// HealthURL is checked after the command; not answering 2xx/3xx within
	// HealthTimeout makes the deploy fail (and roll back, if enabled).
	HealthURL     string   `toml:"health_url"`
	HealthTimeout Duration `toml:"health_timeout"`
	// RollbackOnFailure deploys the last successful commit when a deploy fails.
	RollbackOnFailure bool `toml:"rollback_on_failure"`
	// Cloudflare cache purge after a successful deploy: ["everything"] or URLs.
	CloudflareZoneID string   `toml:"cloudflare_zone_id"`
	CloudflarePurge  []string `toml:"cloudflare_purge"`

	schedule    *cronSpec
	when        []whenCond
	pusherPath  []pathStep
	queueAll    bool
	payloadFile bool
	apiKey      string
	apiSecret   string

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

// placeholderSecret is the value secrets.env.example ships with. It is
// rejected so the service never runs with the template's secrets.
const placeholderSecret = "change-me"

const (
	defaultDeployTimeout = 30 * time.Minute
	defaultCITimeout     = 30 * time.Minute
)

// LoadConfig parses and validates the config file. Secrets are read from the
// environment separately by ResolveSecrets, so CLI commands work without them.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Listen:          "127.0.0.1:9000",
			MaxBodyBytes:    25 << 20, // GitHub caps webhook payloads at 25 MB
			ShutdownTimeout: Duration{5 * time.Minute},
			TrustedProxies:  []string{"127.0.0.0/8", "::1"},
			SocketMode:      "0666",
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
		Cloudflare: CloudflareConfig{
			APIURL: "https://api.cloudflare.com/client/v4",
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
	if abs, err := filepath.Abs(path); err == nil {
		cfg.path = abs
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if err := c.Server.validate(); err != nil {
		return fmt.Errorf("server: %w", err)
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
	if c.SMTP != (SMTPConfig{}) {
		if err := c.SMTP.validate(); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	if err := c.PwPush.validate(); err != nil {
		return err
	}
	if err := c.Hub.validate(); err != nil {
		return err
	}
	c.GitHub.commitStatus = c.GitHub.CommitStatus == nil || *c.GitHub.CommitStatus
	if len(c.Deploy) == 0 {
		return fmt.Errorf("no [deploy.<name>] sections defined")
	}

	if err := validateLabels("labels", c.Labels); err != nil {
		return err
	}
	paths := map[string]string{}
	for name, d := range c.Deploy {
		if err := validateLabels("deploy."+name+".labels", d.Labels); err != nil {
			return err
		}
		d.labels = mergeLabels(c.Labels, d.Labels)
		d.Name = name
		if err := d.validate(); err != nil {
			return fmt.Errorf("deploy.%s: %w", name, err)
		}
		if len(d.WaitForCI) > 0 && c.GitHub.TokenEnv == "" {
			return fmt.Errorf("deploy.%s: wait_for_ci needs [github] token_env (a token with Actions: read)", name)
		}
		if d.Email != nil {
			if err := d.Email.validate(c.SMTP.Host != ""); err != nil {
				return fmt.Errorf("deploy.%s.email: %w", name, err)
			}
		}
		if d.CloudflareZoneID != "" && c.Cloudflare.APITokenEnv == "" {
			return fmt.Errorf("deploy.%s: cloudflare_purge needs [cloudflare] api_token_env (a token with Zone → Cache Purge)", name)
		}
		if d.Path == "" {
			continue
		}
		if other, dup := paths[d.Path]; dup {
			return fmt.Errorf("deploy.%s: path %s already used by deploy.%s", name, d.Path, other)
		}
		paths[d.Path] = name
	}
	return nil
}

func (s *ServerConfig) validate() error {
	if path, ok := strings.CutPrefix(s.Listen, "unix:"); ok {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("listen: unix socket path must be absolute")
		}
		s.socketPath = path
	} else if _, _, err := net.SplitHostPort(s.Listen); err != nil {
		return fmt.Errorf("listen must be host:port or unix:/path: %w", err)
	}

	mode, err := strconv.ParseUint(s.SocketMode, 8, 32)
	if err != nil || mode > 0o777 {
		return fmt.Errorf("socket_mode must be an octal permission like \"0660\"")
	}
	s.socketMode = os.FileMode(mode)

	s.BasePath = strings.TrimRight(s.BasePath, "/")
	if s.BasePath != "" && !hookPathRe.MatchString(s.BasePath) {
		return fmt.Errorf("base_path must start with / and contain only letters, digits, / _ . -")
	}

	s.trusted = nil
	var proxies []string
	for _, p := range s.TrustedProxies {
		if strings.EqualFold(p, "cloudflare") {
			proxies = append(proxies, cloudflareRanges...)
		} else {
			proxies = append(proxies, p)
		}
	}
	for _, p := range proxies {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			addr, aerr := netip.ParseAddr(p)
			if aerr != nil {
				return fmt.Errorf("trusted_proxies: %q is not an IP or CIDR", p)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		s.trusted = append(s.trusted, prefix.Masked())
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
	if d.Schedule != "" {
		spec, err := parseCron(d.Schedule)
		if err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		d.schedule = spec
	}
	if d.Path == "" && d.Schedule != "" {
		// Scheduled only: no webhook, so no provider, secret or repository.
		if d.Provider != "" && d.Provider != providerSchedule {
			return fmt.Errorf("provider %s needs a path (a deploy without path only runs on its schedule)", d.Provider)
		}
		d.Provider = providerSchedule
	} else {
		if !hookPathRe.MatchString(d.Path) {
			return fmt.Errorf("path must start with / and contain only letters, digits, / _ . - (or set only a schedule)")
		}
		for _, reserved := range []string{"/status", "/history", "/deploy", "/rollback", "/healthz", "/metrics"} {
			if d.Path == reserved || strings.HasPrefix(d.Path, reserved+"/") {
				return fmt.Errorf("path %s is reserved", d.Path)
			}
		}
		if d.Provider == "" {
			d.Provider = "github"
		}
		if !knownProvider(d.Provider) {
			return fmt.Errorf("provider must be one of: %s", strings.Join(allProviderNames(), ", "))
		}
		if err := d.validateProvider(); err != nil {
			return err
		}
		if d.SecretEnv == "" {
			return fmt.Errorf("secret_env is required")
		}
	}
	if err := d.validateHooks(); err != nil {
		return err
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
	if d.CITimeout.Duration == 0 {
		d.CITimeout.Duration = defaultCITimeout
	}
	if d.CITimeout.Duration < 0 {
		return fmt.Errorf("ci_timeout must be positive")
	}
	if len(d.WaitForCI) > 0 && d.Provider != "github" {
		return fmt.Errorf("wait_for_ci is only supported with provider = \"github\"")
	}
	for _, w := range d.WaitForCI {
		if strings.TrimSpace(w) == "" {
			return fmt.Errorf("wait_for_ci has an empty workflow name")
		}
	}
	d.lock = d.Lock == nil || *d.Lock
	d.queue = d.Queue == nil || *d.Queue
	d.logOutput = d.LogOutput == nil || *d.LogOutput

	for name, p := range d.Params {
		if p == nil {
			return fmt.Errorf("param %s is empty", name)
		}
		if err := p.validate(name); err != nil {
			return err
		}
	}
	for _, kv := range d.Env {
		key, _, _ := strings.Cut(kv, "=")
		if _, clash := d.Params[key]; clash {
			return fmt.Errorf("%s is both an env entry and a param", key)
		}
	}
	var err error
	if d.when, err = parseWhen(d.When); err != nil {
		return err
	}
	if len(d.Statuses) > 0 {
		c := whenCond{from: "status", path: []pathStep{{key: "status", index: -1}}, values: d.Statuses}
		d.when = append(d.when, c)
	}
	switch d.QueueMode {
	case "":
		d.queueAll = d.Provider == providerWooCommerce || paymentProvider(d.Provider)
	case "latest":
	case "all":
		d.queueAll = true
	default:
		return fmt.Errorf("queue_mode must be latest or all")
	}
	if d.queueAll && !d.queue {
		return fmt.Errorf("queue_mode = \"all\" needs queue = true")
	}
	if d.QueueMax < 0 {
		return fmt.Errorf("queue_max must be positive")
	}
	if d.QueueMax == 0 {
		d.QueueMax = defaultQueueMax
	}
	if d.PayloadFile != nil {
		d.payloadFile = *d.PayloadFile
	} else {
		d.payloadFile = d.Provider == providerGeneric || d.Provider == providerWooCommerce || paymentProvider(d.Provider)
	}
	if d.QueueKey != "" {
		if _, ok := d.Params[d.QueueKey]; !ok {
			return fmt.Errorf("queue_key %s must be one of the params", d.QueueKey)
		}
		if !d.lock {
			return fmt.Errorf("queue_key needs lock = true")
		}
	}
	return nil
}

// validateProvider checks the settings that differ between git providers,
// which need a repository and branch, and generic or WooCommerce webhooks.
func (d *DeployConfig) validateProvider() error {
	genericOnly := map[string]bool{
		"auth": d.Auth != "", "signature_header": d.SignatureHeader != "", "token_header": d.TokenHeader != "",
		"timestamp_header": d.TimestampHeader != "", "max_skew": d.MaxSkew.Duration != 0,
		"delivery_header": d.DeliveryHeader != "", "pusher_from": d.PusherFrom != "",
	}
	wooOnly := map[string]bool{
		"store_url": d.StoreURL != "", "webhook_url": d.WebhookURL != "", "api_key_env": d.APIKeyEnv != "", "api_secret_env": d.APISecretEnv != "",
		"topics": len(d.Topics) > 0, "statuses": len(d.Statuses) > 0,
	}
	if paymentProvider(d.Provider) {
		delete(genericOnly, "max_skew") // they sign a timestamp too
	}
	if d.Provider != providerGeneric {
		for _, key := range sortedKeys(genericOnly) {
			if genericOnly[key] {
				return fmt.Errorf("%s is only for provider = \"generic\"", key)
			}
		}
	}
	if d.Provider != providerWooCommerce {
		for _, key := range sortedKeys(wooOnly) {
			if wooOnly[key] {
				return fmt.Errorf("%s is only for provider = \"woocommerce\"", key)
			}
		}
	}
	if !paymentProvider(d.Provider) && len(d.Events) > 0 {
		return fmt.Errorf("events is only for provider = stripe, paddle or lemonsqueezy")
	}
	if d.Provider == providerWooCommerce {
		return d.validateWooCommerce()
	}
	if paymentProvider(d.Provider) {
		return d.validatePayment()
	}
	if d.Provider != providerGeneric {
		if d.Repository == "" {
			return fmt.Errorf("repository is required")
		}
		if d.Branch == "" {
			d.Branch = "main"
		}
		return nil
	}

	if d.Branch != "" {
		return fmt.Errorf("branch does not apply to provider = \"generic\" (use when)")
	}
	switch d.Auth {
	case "", authHMAC:
		d.Auth = authHMAC
		if d.TokenHeader != "" {
			return fmt.Errorf("token_header is for auth = \"token\"")
		}
		if d.SignatureHeader == "" {
			d.SignatureHeader = defaultSignatureHeader
		}
	case authToken:
		if d.SignatureHeader != "" || d.TimestampHeader != "" {
			return fmt.Errorf("signature_header and timestamp_header are for auth = \"hmac\"")
		}
		if d.TokenHeader == "" {
			d.TokenHeader = defaultTokenHeader
		}
	default:
		return fmt.Errorf("auth must be hmac or token")
	}
	if d.MaxSkew.Duration < 0 {
		return fmt.Errorf("max_skew must be positive")
	}
	if d.MaxSkew.Duration == 0 {
		d.MaxSkew.Duration = defaultMaxSkew
	}
	if d.DeliveryHeader == "" {
		d.DeliveryHeader = defaultDeliveryHeader
	}
	if d.PusherFrom != "" {
		var err error
		if d.pusherPath, err = parseJSONPath(d.PusherFrom); err != nil {
			return fmt.Errorf("pusher_from: %w", err)
		}
	}
	return nil
}

// ResolveSecrets reads every *_env setting from the environment.
func (c *Config) ResolveSecrets() error {
	get := func(key, name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("%s: environment variable %s is empty or not set", key, name)
		}
		if v == placeholderSecret {
			return "", fmt.Errorf("%s: %s is still the placeholder %q from the template; set a real secret", key, name, placeholderSecret)
		}
		return v, nil
	}
	var err error
	for _, name := range c.DeployNames() {
		d := c.Deploy[name]
		if d.SecretEnv != "" {
			s, e := get("deploy."+name+".secret_env", d.SecretEnv)
			if e != nil {
				return e
			}
			d.secret = []byte(s)
		}
		if d.APIKeyEnv != "" {
			v, e := get("deploy."+name+".api_key_env", d.APIKeyEnv)
			if e != nil {
				return e
			}
			d.apiKey = v
		}
		if d.APISecretEnv != "" {
			v, e := get("deploy."+name+".api_secret_env", d.APISecretEnv)
			if e != nil {
				return e
			}
			d.apiSecret = v
		}
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
	if c.Cloudflare.APITokenEnv != "" {
		if c.Cloudflare.token, err = get("cloudflare.api_token_env", c.Cloudflare.APITokenEnv); err != nil {
			return err
		}
	}
	if c.SMTP.UserEnv != "" {
		if c.SMTP.user, err = get("smtp.user_env", c.SMTP.UserEnv); err != nil {
			return err
		}
		if c.SMTP.password, err = get("smtp.password_env", c.SMTP.PasswordEnv); err != nil {
			return err
		}
	}
	if c.Hub.URL != "" {
		if c.Hub.token, err = get("hub.token_env", c.Hub.TokenEnv); err != nil {
			return err
		}
	}
	if c.PwPush.TokenEnv != "" {
		if c.PwPush.token, err = get("pwpush.token_env", c.PwPush.TokenEnv); err != nil {
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
	names := []string{c.Server.APITokenEnv, c.Notify.URLEnv, c.Notify.TelegramTokenEnv, c.GitHub.TokenEnv, c.Cloudflare.APITokenEnv,
		c.SMTP.UserEnv, c.SMTP.PasswordEnv, c.PwPush.TokenEnv, c.Hub.TokenEnv}
	for _, d := range c.Deploy {
		names = append(names, d.SecretEnv, d.APIKeyEnv, d.APISecretEnv)
	}
	return names
}

const providerSchedule = "schedule"

func knownProvider(name string) bool {
	_, git := providers[name]
	return git || name == providerGeneric || name == providerWooCommerce || paymentProvider(name)
}

func allProviderNames() []string {
	names := append(providerNames(), providerGeneric, providerWooCommerce)
	return append(names, sortedKeys(paymentProviders)...)
}

// validateHooks checks the settings around the command.
func (d *DeployConfig) validateHooks() error {
	if d.HealthURL != "" {
		u, err := url.Parse(d.HealthURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("health_url must be an http(s) URL")
		}
	}
	if d.HealthTimeout.Duration < 0 {
		return fmt.Errorf("health_timeout must be positive")
	}
	if d.HealthTimeout.Duration == 0 {
		d.HealthTimeout.Duration = defaultHealthTimeout
	}
	if d.RollbackOnFailure {
		if _, git := providers[d.Provider]; !git {
			return fmt.Errorf("rollback_on_failure needs a git provider (it redeploys the last good commit)")
		}
	}
	if (d.CloudflareZoneID == "") != (len(d.CloudflarePurge) == 0) {
		return fmt.Errorf("cloudflare_zone_id and cloudflare_purge go together")
	}
	for _, p := range d.CloudflarePurge {
		if p == "everything" {
			if len(d.CloudflarePurge) > 1 {
				return fmt.Errorf("cloudflare_purge: \"everything\" goes alone")
			}
			continue
		}
		if u, err := url.Parse(p); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("cloudflare_purge: %q is not \"everything\" or a full URL", p)
		}
	}
	return nil
}
