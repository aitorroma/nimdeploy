package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"
)

// WooCommerce sends webhooks like this (see WC_Webhook::deliver):
//   - in the background (Action Scheduler), JSON of the resource as the REST
//     API v3 shows it, built when it is sent;
//   - X-WC-Webhook-Signature: base64 HMAC-SHA256 of the body;
//   - X-WC-Webhook-Delivery-ID: wp_hash(webhook id + current second), so two
//     deliveries in the same second share it;
//   - no retries: a failed delivery is lost, and after more than 5 failures
//     in a row (anything but 2xx/301/302) the webhook is disabled;
//   - on creation, a ping: form body "webhook_id=N", no X-WC headers, no
//     signature; it must get exactly 200.
// So a signed request is never answered with an error (it would count as a
// failure), and accepted orders are queued on disk (queue_mode = "all").

const (
	providerWooCommerce = "woocommerce"
	defaultQueueMax     = 1000
)

var (
	defaultWooTopics = []string{"order.created", "order.updated"}
	wooTopicRe       = regexp.MustCompile(`^((coupon|customer|order|product)\.(created|updated|deleted|restored)|action\.[A-Za-z0-9_]+)$`)
	wooStatusRe      = regexp.MustCompile(`^[a-z0-9_-]+$`)
	wooPingRe        = regexp.MustCompile(`^webhook_id=\d+$`)
)

func (d *DeployConfig) validateWooCommerce() error {
	if d.Branch != "" {
		return fmt.Errorf("branch does not apply to provider = \"woocommerce\" (use statuses or when)")
	}
	if len(d.Topics) == 0 {
		d.Topics = append([]string(nil), defaultWooTopics...)
	}
	for _, t := range d.Topics {
		if !wooTopicRe.MatchString(t) {
			return fmt.Errorf("topic %q: use resource.event (order.created, order.updated, product.updated...) or action.<hook>", t)
		}
	}
	for _, s := range d.Statuses {
		if !wooStatusRe.MatchString(s) {
			return fmt.Errorf("status %q: use WooCommerce status slugs without wc- (pending, processing, completed...)", s)
		}
	}
	if d.StoreURL != "" {
		u, err := url.Parse(d.StoreURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("store_url must be the shop's URL, like https://shop.example.com")
		}
		d.StoreURL = strings.TrimRight(d.StoreURL, "/")
		if d.Repository == "" {
			d.Repository = d.StoreURL
		}
	}
	if d.WebhookURL != "" {
		u, err := url.Parse(d.WebhookURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !isLoopback(u.Hostname())) {
			return fmt.Errorf("webhook_url must be https://... (WooCommerce treats a redirect from http as delivered and the event is lost)")
		}
	}
	if (d.APIKeyEnv == "") != (d.APISecretEnv == "") {
		return fmt.Errorf("api_key_env and api_secret_env go together")
	}
	if d.APIKeyEnv != "" && d.StoreURL == "" {
		return fmt.Errorf("api_key_env needs store_url")
	}
	return nil
}

// verifyWooSignature checks X-WC-Webhook-Signature (base64 HMAC-SHA256).
func verifyWooSignature(secret, body []byte, header string) bool {
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func sameStore(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }
	return norm(a) == norm(b)
}

// resourceID is the "id" of the order/product/... in the payload.
func resourceID(doc any) string {
	if obj, ok := doc.(map[string]any); ok {
		if s, ok := scalarString(obj["id"]); ok && defaultParamMatch.MatchString(s) {
			return truncate(s, 64)
		}
	}
	return ""
}

func (s *Server) handleWooCommerce(d *DeployConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
			return
		}
		if r.Header.Get("X-WC-Webhook-ID") == "" {
			// The ping WooCommerce sends when a webhook is created: unsigned,
			// and it only activates the webhook on exactly 200.
			if wooPingRe.Match(bytes.TrimSpace(body)) {
				log.Printf("deploy=%s woocommerce ping (%s) from %s", d.Name, strings.TrimSpace(string(body)), s.clientIP(r))
				writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "pong"})
				return
			}
			writeError(w, http.StatusUnauthorized, "not a WooCommerce webhook")
			return
		}
		if !verifyWooSignature(d.secret, body, r.Header.Get("X-WC-Webhook-Signature")) {
			log.Printf("deploy=%s rejected: invalid woocommerce signature from %s (webhook %s)", d.Name, s.clientIP(r), r.Header.Get("X-WC-Webhook-ID"))
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		topic := r.Header.Get("X-WC-Webhook-Topic")
		// Same delivery ID within a second is possible: add the body to tell them apart.
		sum := sha256.Sum256(body)
		delivery := r.Header.Get("X-WC-Webhook-Delivery-ID") + "-" + hex.EncodeToString(sum[:6])

		if src := r.Header.Get("X-WC-Webhook-Source"); d.StoreURL != "" && src != "" && !sameStore(src, d.StoreURL) {
			ignore(w, d, delivery, "store "+src+" is not "+d.StoreURL)
			return
		}
		if !contains(d.Topics, topic) {
			ignore(w, d, delivery, "topic "+topic+" not in "+strings.Join(d.Topics, ", "))
			return
		}
		doc, err := decodeJSON(body)
		if err != nil {
			s.wooReject(w, d, Trigger{Event: topic, Delivery: delivery}, "invalid JSON payload")
			return
		}
		t := Trigger{
			Source:     TriggerWebhook,
			Provider:   d.Provider,
			Delivery:   delivery,
			Repository: d.Repository,
			Pusher:     "woocommerce",
			Event:      topic,
			ResourceID: resourceID(doc),
			Payload:    body,
		}
		params, reason, err := applyRules(d, doc)
		switch {
		case err != nil:
			s.wooReject(w, d, t, err.Error())
			return
		case reason != "":
			ignore(w, d, delivery, fmt.Sprintf("%s #%s: %s", topic, t.ResourceID, reason))
			return
		}
		t.Params = params
		s.submit(w, d, t)
	}
}

// wooReject answers 200 (an error would count towards WooCommerce disabling
// the webhook), logs why and notifies: a paid order may be waiting.
func (s *Server) wooReject(w http.ResponseWriter, d *DeployConfig, t Trigger, reason string) {
	log.Printf("deploy=%s delivery=%s rejected %s #%s: %s", d.Name, t.Delivery, t.Event, t.ResourceID, reason)
	s.runner.notifyRejected(d, t, reason)
	writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "rejected", "reason": reason})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// --- REST API client ------------------------------------------------------------------

type wooClient struct {
	base, key, secret string
	http              *http.Client
}

func newWooClient(d *DeployConfig) (*wooClient, error) {
	if d.StoreURL == "" || d.apiKey == "" || d.apiSecret == "" {
		return nil, fmt.Errorf("deploy.%s needs store_url, api_key_env and api_secret_env (with their values in secrets.env)", d.Name)
	}
	u, _ := url.Parse(d.StoreURL)
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("store_url must use https: the API keys travel with every request")
	}
	return &wooClient{base: d.StoreURL + "/wp-json/wc/v3", key: d.apiKey, secret: d.apiSecret, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *wooClient) do(method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.key, c.secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nimdeploy/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		var e struct{ Code, Message string }
		_ = json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = truncate(strings.TrimSpace(string(b)), 200)
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type wooWebhook struct {
	ID          int    `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Status      string `json:"status,omitempty"`
	Topic       string `json:"topic,omitempty"`
	DeliveryURL string `json:"delivery_url,omitempty"`
	Secret      string `json:"secret,omitempty"`
}

func (c *wooClient) webhooks() ([]wooWebhook, error) {
	var all []wooWebhook
	for page := 1; ; page++ {
		var batch []wooWebhook
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "status": {"all"}}
		if err := c.do(http.MethodGet, "/webhooks", q, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

// --- CLI -----------------------------------------------------------------------------------

const wooUsage = `usage: nimdeploy woocommerce <command>

  add       configure a deploy that runs on WooCommerce events and create its webhooks
  register  create or update the deploy's webhooks in the shop (idempotent)
  status    show the shop's webhooks for the deploy; -enable reactivates disabled ones
  replay    run the deploy again for orders, by ID or by status (missed or failed events)
  note      add a note to an order and optionally change its status (for scripts)

Run "nimdeploy woocommerce <command> -h" for its options.
`

func cliWooCommerce(cfg *Config, configPath, envFile string, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, wooUsage)
		return 2
	}
	switch args[0] {
	case "add":
		return wooAdd(configPath, envFile, args[1:])
	case "register":
		return wooWithDeploy(cfg, envFile, "register", args[1:], wooRegister)
	case "status":
		return wooWithDeploy(cfg, envFile, "status", args[1:], wooStatus)
	case "replay":
		return wooWithDeploy(cfg, envFile, "replay", args[1:], wooReplay)
	case "note":
		return wooWithDeploy(cfg, envFile, "note", args[1:], wooNote)
	default:
		fmt.Fprint(os.Stderr, wooUsage)
		return 2
	}
}

// wooWithDeploy loads the secrets and the deploy named by the first argument.
func wooWithDeploy(cfg *Config, envFile, cmd string, args []string,
	fn func(cfg *Config, envFile string, d *DeployConfig, c *wooClient, args []string) int) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "usage: nimdeploy woocommerce %s <deploy> [options]   (-h for options)\n", cmd)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			return 0
		}
		return 2
	}
	d, ok := cfg.Deploy[args[0]]
	if !ok || d.Provider != providerWooCommerce {
		var woo []string
		for _, n := range cfg.DeployNames() {
			if cfg.Deploy[n].Provider == providerWooCommerce {
				woo = append(woo, n)
			}
		}
		fmt.Fprintf(os.Stderr, "%q is not a woocommerce deploy (have: %s)\n", args[0], dash(strings.Join(woo, ", ")))
		return 2
	}
	if err := loadSecretsInto(envFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := cfg.ResolveSecrets(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := newWooClient(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return fn(cfg, envFile, d, c, args[1:])
}

// loadSecretsInto exports secrets.env entries not already in the environment.
func loadSecretsInto(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot read %s: run it as the service user (sudo -iu deploy ...) or with sudo", path)
	}
	if err != nil {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if k, v, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(l, "#") {
			k = strings.TrimSpace(k)
			if os.Getenv(k) == "" {
				os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
			}
		}
	}
	return nil
}

func wooAdd(configPath, envFile string, args []string) int {
	fset := flag.NewFlagSet("woocommerce add", flag.ExitOnError)
	store := fset.String("store", "", "the shop, e.g. https://shop.example.com (required)")
	public := fset.String("url", "", "public base URL where nginx serves nimdeploy's /hooks/, e.g. https://deploy.example.com")
	name := fset.String("name", "woocommerce", "deploy name: hook path /hooks/<name>, logs, CLI")
	command := fset.String("command", "", "what to run for each event, e.g. /home/deploy/bin/provision.sh (required; run with bash)")
	dir := fset.String("dir", "", "working directory (default: current)")
	topics := fset.String("topics", strings.Join(defaultWooTopics, ","), "WooCommerce topics, comma separated")
	statuses := fset.String("statuses", "processing,completed", `order statuses that run the command ("" = any)`)
	noRegister := fset.Bool("no-register", false, "only write the config; create the webhooks later with \"register\"")
	fset.Usage = func() {
		fmt.Fprint(fset.Output(), `usage: nimdeploy woocommerce add -store URL -url URL -command CMD [options]

Adds a woocommerce deploy to the config, generates its webhook secret, stores
the shop's REST API keys in secrets.env and creates the webhooks in the shop.
The keys come from $WC_CONSUMER_KEY / $WC_CONSUMER_SECRET, or are asked for.
Create them in WooCommerce → Settings → Advanced → REST API (Read/Write).

`)
		fset.PrintDefaults()
	}
	fset.Parse(args)
	if *store == "" || *command == "" {
		fset.Usage()
		return 2
	}
	if !deployNameRe.MatchString(*name) {
		fmt.Fprintf(os.Stderr, "invalid -name %q (letters, digits, _ . -)\n", *name)
		return 2
	}
	if *dir == "" {
		*dir, _ = os.Getwd()
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfgBytes, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\ninstall nimdeploy first (nimdeploy install, or setup-root.sh)\n", err)
		return 1
	}
	if regexp.MustCompile(`(?m)^\[deploy\.` + regexp.QuoteMeta(*name) + `\]`).Match(cfgBytes) {
		fmt.Fprintf(os.Stderr, "deploy.%s already exists in %s; use another -name, or \"nimdeploy woocommerce register %s\"\n", *name, configPath, *name)
		return 1
	}
	envName := strings.Trim(nonEnvChars.ReplaceAllString(strings.ToUpper(*name), "_"), "_")
	webhookURL := ""
	if *public != "" {
		pu, err := url.Parse(strings.TrimRight(*public, "/"))
		if err != nil || pu.Host == "" || (pu.Scheme != "https" && !isLoopback(pu.Hostname())) {
			fmt.Fprintln(os.Stderr, "-url must be https://<host>: WooCommerce counts the http→https redirect as delivered and the event is lost")
			return 2
		}
		webhookURL = pu.String() + "/hooks/" + *name
	}

	key, secret := os.Getenv("WC_CONSUMER_KEY"), os.Getenv("WC_CONSUMER_SECRET")
	if key == "" {
		key = prompt("WooCommerce consumer key (ck_...): ", false)
	}
	if secret == "" {
		secret = prompt("WooCommerce consumer secret (cs_...): ", true)
	}
	if key == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "the REST API consumer key and secret are required (WC_CONSUMER_KEY / WC_CONSUMER_SECRET)")
		return 2
	}

	var list = func(s string) string {
		var q []string
		for _, v := range strings.Split(s, ",") {
			if v = strings.TrimSpace(v); v != "" {
				q = append(q, strconv.Quote(v))
			}
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	section := fmt.Sprintf(`
[deploy.%s]
provider = "woocommerce"
path = "/hooks/%s"
store_url = %q
webhook_url = %q
secret_env = "%s_WEBHOOK_SECRET"
api_key_env = "%s_WC_KEY"
api_secret_env = "%s_WC_SECRET"
topics = %s
statuses = %s
working_directory = %q
# The order is in $DEPLOY_PAYLOAD_FILE, its ID in $DEPLOY_RESOURCE_ID.
command = "/bin/bash"
args = ["-eo", "pipefail", "-c", %q]
timeout = "30m"
`, *name, *name, strings.TrimRight(*store, "/"), webhookURL, envName, envName, envName, list(*topics), list(*statuses), absDir, *command)
	if strings.TrimSpace(*statuses) == "" {
		section = strings.Replace(section, "statuses = []\n", "", 1)
	}
	if webhookURL == "" {
		section = strings.Replace(section, "webhook_url = \"\"\n", "", 1)
	}

	restore := snapshot(configPath, envFile)
	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteString(section)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		buf := make([]byte, 32)
		_, _ = rand.Read(buf)
		err = upsertEnv(envFile, map[string]string{
			envName + "_WEBHOOK_SECRET": hex.EncodeToString(buf),
			envName + "_WC_KEY":         key,
			envName + "_WC_SECRET":      secret,
		})
	}
	var cfg *Config
	if err == nil {
		cfg, err = LoadConfig(configPath)
	}
	if err == nil {
		if err = loadSecretsInto(envFile); err == nil {
			err = cfg.ResolveSecrets()
		}
	}
	if err != nil {
		restore()
		fmt.Fprintf(os.Stderr, "nothing changed: %v\n", err)
		return 1
	}
	step("config   %s (deploy.%s added)", configPath, *name)
	step("secrets  %s (%s_WEBHOOK_SECRET, %s_WC_KEY, %s_WC_SECRET)", envFile, envName, envName, envName)

	d := cfg.Deploy[*name]
	c, err := newWooClient(d)
	if err == nil {
		var probe []map[string]any
		err = c.do(http.MethodGet, "/orders", url.Values{"per_page": {"1"}}, nil, &probe)
	}
	if err != nil {
		caution("the shop's API did not answer with these keys: %v", err)
	} else {
		step("shop     %s answers (REST API keys OK)", d.StoreURL)
	}
	if err == nil && webhookURL != "" && !*noRegister {
		if code := wooRegister(cfg, envFile, d, c, nil); code != 0 {
			return code
		}
	}

	fmt.Printf(`
Next:
  1. Apply the new secrets:   restart nimdeploy (systemctl --user restart nimdeploy,
                              or sudo systemctl restart nimdeploy)
  2. nginx must pass %s to nimdeploy ("nimdeploy nginx" prints the block)
`, "/hooks/"+*name)
	if webhookURL == "" || *noRegister {
		fmt.Printf("  3. Create the webhooks:     nimdeploy woocommerce register %s -url https://<public host>\n", *name)
	}
	fmt.Printf(`  ·  Check them any time:     nimdeploy woocommerce status %s
  ·  Missed an order?         nimdeploy woocommerce replay %s <order-id>
`, *name, *name)
	return 0
}

// upsertEnv sets keys in an env file (0600), keeping everything else.
func upsertEnv(path string, values map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, k := range sortedKeys(values) {
		line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(k) + `=.*$`)
		if line.Match(b) {
			b = line.ReplaceAll(b, []byte(k+"="+values[k]))
		} else {
			if len(b) > 0 && b[len(b)-1] != '\n' {
				b = append(b, '\n')
			}
			b = append(b, []byte(k+"="+values[k]+"\n")...)
		}
	}
	return os.WriteFile(path, b, 0o600)
}

// prompt reads a line from the terminal; hidden turns echo off.
func prompt(label string, hidden bool) string {
	if !isTerminal(os.Stdin) {
		return ""
	}
	fmt.Fprint(os.Stderr, label)
	if hidden {
		var old syscall.Termios
		fd := os.Stdin.Fd()
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old))); e == 0 {
			noEcho := old
			noEcho.Lflag &^= syscall.ECHO
			syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&noEcho)))
			defer func() {
				syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func wooRegister(cfg *Config, _ string, d *DeployConfig, c *wooClient, args []string) int {
	fset := flag.NewFlagSet("woocommerce register", flag.ExitOnError)
	public := fset.String("url", "", "public URL of the hook (default: webhook_url from the config), or the base https://deploy.example.com")
	fset.Parse(args)
	target := d.WebhookURL
	if *public != "" {
		target = strings.TrimRight(*public, "/")
		if !strings.HasSuffix(target, d.Path) {
			target += cfg.Server.BasePath + d.Path
		}
	}
	if target == "" {
		fmt.Fprintf(os.Stderr, "deploy.%s has no webhook_url: pass -url https://<public host>\n", d.Name)
		return 2
	}
	if u, err := url.Parse(target); err != nil || (u.Scheme != "https" && !isLoopback(u.Hostname())) {
		fmt.Fprintln(os.Stderr, "the webhook URL must be https: WooCommerce counts the http→https redirect as delivered and the event is lost")
		return 2
	}
	existing, err := c.webhooks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	failed := false
	for _, topic := range d.Topics {
		wh := wooWebhook{
			Name:        fmt.Sprintf("nimdeploy %s (%s)", d.Name, topic),
			Topic:       topic,
			DeliveryURL: target,
			Secret:      string(d.secret),
			Status:      "active",
		}
		var found *wooWebhook
		for i := range existing {
			if existing[i].DeliveryURL == target && existing[i].Topic == topic {
				found = &existing[i]
				break
			}
		}
		var out wooWebhook
		if found != nil {
			err = c.do(http.MethodPut, fmt.Sprintf("/webhooks/%d", found.ID), nil, wh, &out)
			if err == nil {
				step("webhook  %s → %s (#%d updated, %s)", topic, target, out.ID, out.Status)
			}
		} else {
			err = c.do(http.MethodPost, "/webhooks", nil, wh, &out)
			if err == nil {
				step("webhook  %s → %s (#%d created, %s)", topic, target, out.ID, out.Status)
			}
		}
		if err != nil {
			caution("webhook %s: %v", topic, err)
			failed = true
		}
	}
	if failed {
		return 1
	}
	fmt.Println("WooCommerce sent a ping to each one; it is in the service log (journalctl) if nimdeploy already runs with this config.")
	return 0
}

func wooStatus(_ *Config, _ string, d *DeployConfig, c *wooClient, args []string) int {
	fset := flag.NewFlagSet("woocommerce status", flag.ExitOnError)
	enable := fset.Bool("enable", false, "reactivate webhooks of this deploy that WooCommerce disabled or paused")
	fset.Parse(args)
	all, err := c.webhooks()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTOPIC\tSTATUS\tDELIVERY URL")
	mine, bad := 0, 0
	for _, wh := range all {
		if !strings.HasSuffix(strings.TrimRight(wh.DeliveryURL, "/"), d.Path) || (d.WebhookURL != "" && wh.DeliveryURL != d.WebhookURL) {
			continue
		}
		mine++
		st := wh.Status
		if st != "active" {
			bad++
			if *enable {
				if err := c.do(http.MethodPut, fmt.Sprintf("/webhooks/%d", wh.ID), nil, wooWebhook{Status: "active"}, nil); err != nil {
					st += " (enable failed: " + err.Error() + ")"
				} else {
					st += " → active"
				}
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", wh.ID, wh.Topic, st, wh.DeliveryURL)
	}
	tw.Flush()
	switch {
	case mine == 0:
		fmt.Printf("no webhooks of %s point to this deploy: nimdeploy woocommerce register %s\n", d.StoreURL, d.Name)
		return 1
	case bad > 0 && !*enable:
		fmt.Printf("\n%d not active: WooCommerce disables a webhook after 5 failed deliveries in a row.\nFix the cause (journalctl -u nimdeploy), then: nimdeploy woocommerce status %s -enable\nand recover what was missed: nimdeploy woocommerce replay %s -status processing -after <date>\n", bad, d.Name, d.Name)
		return 1
	}
	for _, t := range d.Topics {
		found := false
		for _, wh := range all {
			if wh.Topic == t && strings.HasSuffix(strings.TrimRight(wh.DeliveryURL, "/"), d.Path) {
				found = true
			}
		}
		if !found {
			fmt.Printf("topic %s has no webhook: nimdeploy woocommerce register %s\n", t, d.Name)
		}
	}
	return 0
}

func wooReplay(cfg *Config, envFile string, d *DeployConfig, c *wooClient, args []string) int {
	fset := flag.NewFlagSet("woocommerce replay", flag.ExitOnError)
	status := fset.String("status", "", "replay orders with these statuses (comma separated), e.g. processing")
	after := fset.String("after", "", "with -status: orders created after this date (2026-10-01 or RFC 3339)")
	limit := fset.Int("limit", 100, "with -status: at most this many orders")
	topic := fset.String("topic", "order.updated", "event name the run gets as DEPLOY_EVENT")
	dry := fset.Bool("n", false, "dry run: list the orders, run nothing")
	fset.Usage = func() {
		fmt.Fprint(fset.Output(), `usage: nimdeploy woocommerce replay <deploy> <order-id>...
       nimdeploy woocommerce replay <deploy> -status processing [-after 2026-10-01] [-limit 100]

Fetches the orders from the shop and runs the deploy for each, through the
same statuses/when/params checks as a webhook. The script should be
idempotent: replaying an order that was already handled must do nothing.

`)
		fset.PrintDefaults()
	}
	fset.Parse(args)

	var orders []json.RawMessage
	switch {
	case fset.NArg() > 0:
		for _, id := range fset.Args() {
			if _, err := strconv.Atoi(id); err != nil {
				fmt.Fprintf(os.Stderr, "%q is not an order ID\n", id)
				return 2
			}
			var o json.RawMessage
			if err := c.do(http.MethodGet, "/orders/"+id, nil, nil, &o); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			orders = append(orders, o)
		}
	case *status != "":
		q := url.Values{"status": {*status}, "orderby": {"date"}, "order": {"asc"}}
		if *after != "" {
			ts := *after
			if len(ts) == len("2006-01-02") {
				ts += "T00:00:00"
			}
			q.Set("after", ts)
		}
		for page := 1; len(orders) < *limit; page++ {
			var batch []json.RawMessage
			q.Set("per_page", strconv.Itoa(min(100, *limit-len(orders))))
			q.Set("page", strconv.Itoa(page))
			if err := c.do(http.MethodGet, "/orders", q, nil, &batch); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			orders = append(orders, batch...)
			if len(batch) == 0 || len(batch) < 100 {
				break
			}
		}
	default:
		fset.Usage()
		return 2
	}
	if len(orders) == 0 {
		fmt.Println("no orders")
		return 0
	}

	var api *client
	if !*dry {
		var err error
		if api, err = newClient(cfg, envFile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	user := firstNonEmpty(os.Getenv("SUDO_USER"), os.Getenv("USER"), "cli")
	failed := 0
	for _, o := range orders {
		var head struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(o, &head)
		if *dry {
			fmt.Printf("#%d %s\n", head.ID, head.Status)
			continue
		}
		var res SubmitResult
		_, err := api.do(http.MethodPost, "/deploy/"+d.Name, manualRequest{User: user, Event: *topic, Payload: o}, &res)
		switch {
		case err != nil:
			failed++
			fmt.Printf("#%d %s: %v\n", head.ID, head.Status, err)
		case res.Result == "":
			fmt.Printf("#%d %s: ignored\n", head.ID, head.Status)
		default:
			fmt.Printf("#%d %s: %s (log %s)\n", head.ID, head.Status, res.Result, res.State.Log)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func wooNote(_ *Config, _ string, d *DeployConfig, c *wooClient, args []string) int {
	fset := flag.NewFlagSet("woocommerce note", flag.ExitOnError)
	customer := fset.Bool("customer", false, "customer note: WooCommerce emails it to the customer")
	status := fset.String("status", "", "also set the order status, e.g. completed or on-hold")
	fset.Usage = func() {
		fmt.Fprint(fset.Output(), `usage: nimdeploy woocommerce note <deploy> [-customer] [-status completed] <order-id> <text>

For deploy scripts to close the loop on the order. Inside a script:
  "$NIMDEPLOY" woocommerce note "$DEPLOY_NAME" -status completed "$DEPLOY_RESOURCE_ID" "Provisioned on $(hostname)"

`)
		fset.PrintDefaults()
	}
	fset.Parse(args)
	if fset.NArg() != 2 {
		fset.Usage()
		return 2
	}
	id, text := fset.Arg(0), fset.Arg(1)
	if _, err := strconv.Atoi(id); err != nil {
		fmt.Fprintf(os.Stderr, "%q is not an order ID\n", id)
		return 2
	}
	if strings.TrimSpace(text) != "" {
		if err := c.do(http.MethodPost, "/orders/"+id+"/notes", nil, map[string]any{"note": text, "customer_note": *customer}, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if *status != "" {
		if !wooStatusRe.MatchString(*status) {
			fmt.Fprintf(os.Stderr, "invalid status %q\n", *status)
			return 2
		}
		if err := c.do(http.MethodPut, "/orders/"+id, nil, map[string]string{"status": *status}, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	fmt.Printf("order #%s updated (%s)\n", id, d.StoreURL)
	return 0
}
