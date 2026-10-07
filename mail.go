package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	htmltemplate "html/template"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
)

// Emails sent by a deploy: typically the welcome email of a provisioning
// script, with what the script created (in $DEPLOY_OUTPUT) and secrets
// turned into Password Pusher links. Nothing in them is logged; a failed
// send is kept in the outbox and retried with "nimdeploy mail retry".

type SMTPConfig struct {
	Host        string `toml:"host"`
	Port        int    `toml:"port"`
	TLS         string `toml:"tls"` // starttls (default), tls (port 465), none (localhost only)
	UserEnv     string `toml:"user_env"`
	PasswordEnv string `toml:"password_env"`
	From        string `toml:"from"`

	user, password string
	from           *mail.Address
}

func (s *SMTPConfig) validate() error {
	if s.Host == "" {
		return errors.New("host is required")
	}
	if s.TLS == "" {
		s.TLS = "starttls"
		if s.Port == 465 {
			s.TLS = "tls"
		}
	}
	switch s.TLS {
	case "starttls", "tls":
	case "none":
		if !isLoopback(s.Host) {
			return errors.New(`tls = "none" is only allowed for a local relay (localhost)`)
		}
	default:
		return errors.New("tls must be starttls, tls or none")
	}
	if s.Port == 0 {
		s.Port = map[string]int{"starttls": 587, "tls": 465, "none": 25}[s.TLS]
	}
	if (s.UserEnv == "") != (s.PasswordEnv == "") {
		return errors.New("user_env and password_env go together")
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("from %q: %w", s.From, err)
	}
	s.from = from
	return nil
}

type EmailConfig struct {
	On       string   `toml:"on"`      // success (default), failure, always
	To       []string `toml:"to"`      // fixed recipients
	ToFrom   string   `toml:"to_from"` // JSON path in the payload, e.g. billing.email
	Bcc      []string `toml:"bcc"`
	Subject  string   `toml:"subject"`  // template
	Template string   `toml:"template"` // file: .html → HTML + text, else text; .hbs → Handlebars
	Secrets  []string `toml:"secrets"`  // $DEPLOY_OUTPUT keys sent as pwpush links
	// Once sends it only once per resource (order, session...), so replays
	// and retries don't email the customer twice. Default true.
	Once *bool `toml:"once"`

	toPath  []pathStep
	once    bool
	subject templateExecutor
	body    templateExecutor
	html    bool
	hbs     bool
}

type templateExecutor interface {
	Execute(io.Writer, any) error
}

func (e *EmailConfig) validate(hasSMTP bool) error {
	if !hasSMTP {
		return errors.New("needs an [smtp] section")
	}
	switch e.On {
	case "":
		e.On = "success"
	case "success", "failure", "always":
	default:
		return errors.New("on must be success, failure or always")
	}
	if len(e.To) == 0 && e.ToFrom == "" {
		return errors.New("to or to_from is required")
	}
	for _, a := range append(append([]string(nil), e.To...), e.Bcc...) {
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("address %q: %w", a, err)
		}
	}
	if e.ToFrom != "" {
		p, err := parseJSONPath(e.ToFrom)
		if err != nil {
			return fmt.Errorf("to_from: %w", err)
		}
		e.toPath = p
	}
	if e.Subject == "" {
		return errors.New("subject is required")
	}
	if e.Template == "" || !filepath.IsAbs(e.Template) {
		return errors.New("template must be an absolute path to a template file")
	}
	src, err := os.ReadFile(e.Template)
	if err != nil {
		return fmt.Errorf("template: %w", err)
	}
	name := strings.ToLower(e.Template)
	e.hbs = strings.HasSuffix(name, ".hbs") || strings.HasSuffix(name, ".handlebars")
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".hbs"), ".handlebars")
	e.html = strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".htm")
	base := filepath.Base(e.Template)
	if e.hbs {
		if e.subject, err = parseHandlebars("subject", e.Subject, false); err != nil {
			return fmt.Errorf("subject: %w", err)
		}
		if e.body, err = parseHandlebars(base, string(src), e.html); err != nil {
			return fmt.Errorf("template %s: %w", base, err)
		}
	} else {
		if e.subject, err = texttemplate.New("subject").Funcs(hbsFuncs(false)).Option("missingkey=zero").Parse(e.Subject); err != nil {
			return fmt.Errorf("subject: %w", err)
		}
		if e.html {
			e.body, err = htmltemplate.New(base).Funcs(hbsFuncs(true)).Option("missingkey=zero").Parse(string(src))
		} else {
			e.body, err = texttemplate.New(base).Funcs(hbsFuncs(false)).Option("missingkey=zero").Parse(string(src))
		}
		if err != nil {
			return fmt.Errorf("template: %w", err)
		}
	}
	for _, s := range e.Secrets {
		if !paramNameRe.MatchString(s) {
			return fmt.Errorf("secrets: %q must be an UPPER_CASE output key", s)
		}
	}
	e.once = e.Once == nil || *e.Once
	return nil
}

// mailData is what templates see.
type mailData struct {
	Deploy, Status, Trigger, Event, ResourceID, Commit, Host string
	Output                                                   map[string]string // script results, secrets removed
	Links                                                    map[string]string // pwpush links for the secrets
	Params                                                   map[string]string
	Labels                                                   map[string]string
	Payload                                                  any // the webhook body (order, event...)
}

// readOutput parses $DEPLOY_OUTPUT: a JSON object, or KEY=VALUE lines.
func readOutput(path string) (map[string]string, error) {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '{' {
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("DEPLOY_OUTPUT is not valid JSON: %w", err)
		}
		for k, v := range obj {
			if s, ok := scalarString(v); ok {
				out[k] = s
			}
		}
		return out, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}

// maskEmail turns ana@example.com into a***@example.com for logs.
func maskEmail(addr string) string {
	user, domain, ok := strings.Cut(addr, "@")
	if !ok || user == "" {
		return "***"
	}
	return user[:1] + "***@" + domain
}

type outgoing struct {
	From  string   `json:"from"`
	To    []string `json:"to"`
	Rcpt  []string `json:"rcpt"` // to + bcc
	Msg   []byte   `json:"msg"`
	Key   string   `json:"key,omitempty"` // "once" marker to write on success
	About string   `json:"about"`         // for logs: deploy, resource
}

// composeEmail renders an email. Secrets in the output become pwpush links
// first; the raw values never reach the template.
func composeEmail(e *EmailConfig, s *SMTPConfig, pp *pwPush, data *mailData, extraTo []string) (*outgoing, error) {
	data.Links = map[string]string{}
	for _, k := range e.Secrets {
		v, ok := data.Output[k]
		delete(data.Output, k)
		if !ok || v == "" {
			continue
		}
		if pp == nil {
			return nil, errors.New("secrets need a [pwpush] section")
		}
		link, err := pp.push(v, strings.TrimSpace(fmt.Sprintf("%s %s · %s", data.Deploy, refOrEmpty(data.ResourceID), k)), 0, 0)
		if err != nil {
			return nil, fmt.Errorf("pwpush %s: %w", k, err)
		}
		data.Links[k] = link
	}

	to := append([]string(nil), e.To...)
	to = append(to, extraTo...)
	if e.toPath != nil {
		if v, ok := lookupJSON(data.Payload, e.toPath); ok {
			if addr, ok := scalarString(v); ok && addr != "" {
				to = append(to, addr)
			}
		}
	}
	var rcpt, toHdr []string
	for _, a := range to {
		addr, err := mail.ParseAddress(a)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", a, err)
		}
		rcpt = append(rcpt, addr.Address)
		toHdr = append(toHdr, addr.String())
	}
	if len(rcpt) == 0 {
		return nil, fmt.Errorf("no recipient (to_from %s not found in the payload)", e.ToFrom)
	}
	for _, a := range e.Bcc {
		addr, _ := mail.ParseAddress(a)
		rcpt = append(rcpt, addr.Address)
	}

	var subj, body bytes.Buffer
	var ctx any = data
	if e.hbs {
		ctx = data.toHandlebarsData()
	}
	if err := e.subject.Execute(&subj, ctx); err != nil {
		return nil, fmt.Errorf("subject: %w", err)
	}
	if err := e.body.Execute(&body, ctx); err != nil {
		return nil, fmt.Errorf("template %s: %w", filepath.Base(e.Template), err)
	}
	msg := buildMessage(s.from.String(), toHdr, strings.ReplaceAll(subj.String(), "\n", " "), body.String(), e.html)
	return &outgoing{From: s.from.Address, To: toHdr, Rcpt: rcpt, Msg: msg}, nil
}

func refOrEmpty(id string) string {
	if id == "" {
		return ""
	}
	return "#" + id
}

var (
	tagRe    = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	blankRe  = regexp.MustCompile(`\n[ \t]*\n([ \t]*\n)+`)
	hrefRe   = regexp.MustCompile(`(?i)<a\s[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	brRe     = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</h[1-6]>|</li>|</tr>`)
	cellRe   = regexp.MustCompile(`(?i)</t[dh]>\s*<t[dh][^>]*>`) // between cells of a row
	entities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ")
)

// htmlToText is a readable text part for clients that don't show HTML.
func htmlToText(h string) string {
	h = hrefRe.ReplaceAllString(h, "$2 ($1)")
	h = cellRe.ReplaceAllString(h, ": ")
	h = brRe.ReplaceAllString(h, "\n")
	h = tagRe.ReplaceAllString(h, "")
	h = entities.Replace(h)
	lines := strings.Split(h, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(blankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func buildMessage(from string, to []string, subject, body string, html bool) []byte {
	var b bytes.Buffer
	host, _ := os.Hostname()
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "nimdeploy.local"
	if addr, err := mail.ParseAddress(from); err == nil {
		if _, d, ok := strings.Cut(addr.Address, "@"); ok {
			domain = d
		}
	}
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s.%s@%s>\r\n", hex.EncodeToString(id), strings.ReplaceAll(host, ".", "-"), domain)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("X-Mailer: nimdeploy\r\n")
	part := func(ctype, content string) {
		fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", ctype)
		w := quotedprintable.NewWriter(&b)
		_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\n", "\r\n")))
		_ = w.Close()
		b.WriteString("\r\n")
	}
	if !html {
		part("text/plain", body)
		return b.Bytes()
	}
	boundary := "nimdeploy-" + hex.EncodeToString(id)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	part("text/plain", htmlToText(body))
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	part("text/html", body)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// sendSMTP delivers a message, with TLS as configured.
func sendSMTP(s *SMTPConfig, m *outgoing) error {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if s.TLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.Host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS (use tls = \"tls\" with port 465)")
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return err
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.password, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	for _, r := range m.Rcpt {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", maskEmail(r), err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(m.Msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// sendDeployEmail runs after a deploy: compose, send, or keep in the outbox.
// It returns a line for the deploy log (never secrets or full addresses).
func (r *Runner) sendDeployEmail(d *DeployConfig, t Trigger, status, outputPath string, notifier *Notifier) string {
	e := d.Email
	switch {
	case e == nil:
		return ""
	case e.On == "success" && status != StatusSuccess, e.On == "failure" && status != StatusFailed:
		return ""
	}
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()

	marker := ""
	if e.once && t.ResourceID != "" {
		sum := sha256.Sum256([]byte(t.ResourceID + "\x00" + e.Template))
		marker = filepath.Join(r.dir, d.Name, "mail-sent", hex.EncodeToString(sum[:12]))
		if _, err := os.Stat(marker); err == nil {
			return "email: already sent for #" + t.ResourceID + " (once), not sent again"
		}
	}
	output, err := readOutput(outputPath)
	if err != nil {
		return "email: " + err.Error()
	}
	data := mailData{Labels: copyLabels(d.labels), Deploy: d.Name, Status: status, Trigger: t.Source, Event: t.Event, ResourceID: t.ResourceID,
		Commit: t.Commit, Output: output, Params: map[string]string{}}
	data.Host, _ = os.Hostname()
	for _, p := range t.Params {
		data.Params[p.Name] = p.Value
	}
	if len(t.Payload) > 0 {
		data.Payload, _ = decodeJSON(t.Payload)
	}
	var pp *pwPush
	if len(e.Secrets) > 0 {
		pp = newPwPush(cfg.PwPush)
	}
	m, err := composeEmail(e, &cfg.SMTP, pp, &data, nil)
	if err != nil {
		go notifier.MailFailed(d, t, err.Error(), false)
		return "email: not sent: " + err.Error()
	}
	m.Key, m.About = marker, d.Name+" "+refOrEmpty(t.ResourceID)
	masked := make([]string, len(m.Rcpt))
	for i, a := range m.Rcpt {
		masked[i] = maskEmail(a)
	}
	if err := sendSMTP(&cfg.SMTP, m); err != nil {
		file, oerr := r.saveOutbox(d.Name, m)
		if oerr != nil {
			log.Printf("deploy=%s cannot save email to the outbox: %v", d.Name, oerr)
		}
		go notifier.MailFailed(d, t, err.Error(), oerr == nil)
		return fmt.Sprintf("email: send to %s failed: %v; kept in %s, retry with: nimdeploy mail retry", strings.Join(masked, ", "), err, file)
	}
	markSent(marker)
	return fmt.Sprintf("email: sent to %s (%s, %d secret links)", strings.Join(masked, ", "), filepath.Base(e.Template), len(data.Links))
}

func markSent(marker string) {
	if marker == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(marker), 0o750)
	_ = os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o640)
}

func (r *Runner) saveOutbox(deploy string, m *outgoing) (string, error) {
	dir := filepath.Join(r.dir, deploy, "outbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+shortID("")+".json")
	return file, os.WriteFile(file, b, 0o600)
}

// --- CLI ---------------------------------------------------------------------------------

const mailUsage = `usage: nimdeploy mail <command>

  test   -to ADDR                send a test email with the [smtp] settings
  send   -to ADDR -subject S -template FILE [-data FILE] [-payload FILE] [-secrets K1,K2]
                                 send an email from a script (secrets become pwpush links)
  retry  [-n]                    resend the emails kept in the outbox after a failure
`

func cliMail(cfg *Config, envFile string, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, mailUsage)
		return 2
	}
	if cfg.SMTP.Host == "" {
		fmt.Fprintln(os.Stderr, "nimdeploy mail: no [smtp] section in the config")
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
	switch args[0] {
	case "test":
		return mailTest(cfg, args[1:])
	case "send":
		return mailSend(cfg, args[1:])
	case "retry":
		return mailRetry(cfg, args[1:])
	}
	fmt.Fprint(os.Stderr, mailUsage)
	return 2
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func mailTest(cfg *Config, args []string) int {
	fset := flag.NewFlagSet("mail test", flag.ExitOnError)
	var to listFlag
	fset.Var(&to, "to", "recipient (repeatable)")
	fset.Parse(args)
	if len(to) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nimdeploy mail test -to you@example.com")
		return 2
	}
	host, _ := os.Hostname()
	msg := buildMessage(cfg.SMTP.from.String(), to, "nimdeploy test email from "+host,
		fmt.Sprintf("This is a test email from nimdeploy %s on %s.\nSMTP: %s:%d (%s)\n", version, host, cfg.SMTP.Host, cfg.SMTP.Port, cfg.SMTP.TLS), false)
	var rcpt []string
	for _, a := range to {
		addr, err := mail.ParseAddress(a)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		rcpt = append(rcpt, addr.Address)
	}
	if err := sendSMTP(&cfg.SMTP, &outgoing{From: cfg.SMTP.from.Address, To: to, Rcpt: rcpt, Msg: msg}); err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy mail test:", err)
		return 1
	}
	fmt.Printf("sent to %s through %s:%d\n", strings.Join(to, ", "), cfg.SMTP.Host, cfg.SMTP.Port)
	return 0
}

func mailSend(cfg *Config, args []string) int {
	fset := flag.NewFlagSet("mail send", flag.ExitOnError)
	var to, bcc listFlag
	fset.Var(&to, "to", "recipient (repeatable)")
	fset.Var(&bcc, "bcc", "hidden recipient (repeatable)")
	subject := fset.String("subject", "", "subject (a template: {{ .Output.SITE_URL }}...)")
	tmpl := fset.String("template", "", "body template file (.html for HTML)")
	data := fset.String("data", os.Getenv("DEPLOY_OUTPUT"), "results file: JSON or KEY=VALUE lines (default $DEPLOY_OUTPUT)")
	payload := fset.String("payload", os.Getenv("DEPLOY_PAYLOAD_FILE"), "JSON available as .Payload (default $DEPLOY_PAYLOAD_FILE)")
	secrets := fset.String("secrets", "", "comma-separated keys of -data to send as pwpush links")
	fset.Parse(args)
	if *subject == "" || *tmpl == "" || len(to) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nimdeploy mail send -to ADDR -subject S -template FILE [-data FILE] [-payload FILE] [-secrets K1,K2]")
		return 2
	}
	abs, _ := filepath.Abs(*tmpl)
	e := &EmailConfig{To: to, Bcc: bcc, Subject: *subject, Template: abs}
	for _, s := range strings.Split(*secrets, ",") {
		if s = strings.TrimSpace(s); s != "" {
			e.Secrets = append(e.Secrets, s)
		}
	}
	if err := e.validate(true); err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy mail send:", err)
		return 2
	}
	md := mailData{Deploy: os.Getenv("DEPLOY_NAME"), Trigger: os.Getenv("DEPLOY_TRIGGER"), Event: os.Getenv("DEPLOY_EVENT"),
		ResourceID: os.Getenv("DEPLOY_RESOURCE_ID"), Commit: os.Getenv("DEPLOY_COMMIT"), Params: map[string]string{}}
	md.Host, _ = os.Hostname()
	out, err := map[string]string{}, error(nil)
	if *data != "" {
		if out, err = readOutput(*data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	md.Output = out
	if *payload != "" {
		if b, err := os.ReadFile(*payload); err == nil {
			md.Payload, _ = decodeJSON(b)
		}
	}
	var pp *pwPush
	if len(e.Secrets) > 0 {
		pp = newPwPush(cfg.PwPush)
	}
	m, err := composeEmail(e, &cfg.SMTP, pp, &md, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy mail send:", err)
		return 1
	}
	if err := sendSMTP(&cfg.SMTP, m); err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy mail send:", err)
		return 1
	}
	masked := make([]string, len(m.Rcpt))
	for i, a := range m.Rcpt {
		masked[i] = maskEmail(a)
	}
	fmt.Printf("sent to %s\n", strings.Join(masked, ", "))
	return 0
}

func mailRetry(cfg *Config, args []string) int {
	fset := flag.NewFlagSet("mail retry", flag.ExitOnError)
	dry := fset.Bool("n", false, "only list the outbox")
	fset.Parse(args)
	files, _ := filepath.Glob(filepath.Join(cfg.Logging.Directory, "*", "outbox", "*.json"))
	if len(files) == 0 {
		fmt.Println("outbox empty")
		return 0
	}
	failed := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		var m outgoing
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		if err != nil {
			fmt.Printf("%s: %v\n", f, err)
			failed++
			continue
		}
		masked := make([]string, len(m.Rcpt))
		for i, a := range m.Rcpt {
			masked[i] = maskEmail(a)
		}
		if *dry {
			fmt.Printf("%s  %s → %s\n", filepath.Base(f), m.About, strings.Join(masked, ", "))
			continue
		}
		if err := sendSMTP(&cfg.SMTP, &m); err != nil {
			fmt.Printf("%s  %s: still failing: %v\n", filepath.Base(f), m.About, err)
			failed++
			continue
		}
		markSent(m.Key)
		_ = os.Remove(f)
		fmt.Printf("%s  %s → %s: sent\n", filepath.Base(f), m.About, strings.Join(masked, ", "))
	}
	if failed > 0 {
		return 1
	}
	return 0
}
