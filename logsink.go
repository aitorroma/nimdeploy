package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The service log (journald, docker logs): nimdeploy's lines are
// "words key=value key="quoted value"". logSink writes them as they are
// ("text") or as one JSON object per line ("json"), hides the values of
// configured secrets in both, and can copy each record to OTLP.

const redacted = "[REDACTED]"

// logRecord is one parsed line.
type logRecord struct {
	Time   time.Time
	Level  string
	Msg    string
	Fields [][2]string // in the order they appear
}

func (r logRecord) field(key string) string {
	for _, f := range r.Fields {
		if f[0] == key {
			return f[1]
		}
	}
	return ""
}

type logSink struct {
	mu      sync.Mutex
	out     io.Writer
	json    bool
	service string
	secrets []string
	export  func(logRecord) // OTLP logs, optional
	buf     []byte
}

var serviceLog = &logSink{out: os.Stderr, service: "nimdeploy"}

// installLogSink routes the standard logger through the sink.
func installLogSink(format, service string) {
	serviceLog.mu.Lock()
	serviceLog.json = format == "json"
	serviceLog.service = service
	serviceLog.mu.Unlock()
	log.SetFlags(0)
	log.SetOutput(serviceLog)
}

// setLogSecrets replaces the values hidden in the log. Short values are
// skipped: hiding "abc" everywhere would hide more than the secret.
func setLogSecrets(values []string) {
	var keep []string
	seen := map[string]bool{}
	for _, v := range values {
		if len(v) >= 8 && !seen[v] {
			seen[v] = true
			keep = append(keep, v)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return len(keep[i]) > len(keep[j]) })
	serviceLog.mu.Lock()
	serviceLog.secrets = keep
	serviceLog.mu.Unlock()
}

func setLogExport(f func(logRecord)) {
	serviceLog.mu.Lock()
	serviceLog.export = f
	serviceLog.mu.Unlock()
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		s.line(string(s.buf[:i]))
		s.buf = s.buf[i+1:]
	}
	return len(p), nil
}

func (s *logSink) redact(line string) string {
	for _, v := range s.secrets {
		line = strings.ReplaceAll(line, v, redacted)
	}
	return line
}

func (s *logSink) line(line string) {
	now := time.Now()
	line = s.redact(line)
	rec := parseLogLine(line)
	rec.Time = now
	if s.json {
		_, _ = s.out.Write(append(rec.json(s.service), '\n'))
	} else {
		_, _ = io.WriteString(s.out, now.Format("2006/01/02 15:04:05 ")+line+"\n")
	}
	if s.export != nil {
		s.export(rec)
	}
}

func (r logRecord) json(service string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"time":`)
	b.Write(jsonQuoted(r.Time.UTC().Format(time.RFC3339Nano)))
	b.WriteString(`,"level":`)
	b.Write(jsonQuoted(r.Level))
	b.WriteString(`,"service":`)
	b.Write(jsonQuoted(service))
	b.WriteString(`,"msg":`)
	b.Write(jsonQuoted(r.Msg))
	for _, f := range r.Fields {
		switch f[0] {
		case "time", "level", "service", "msg":
			f[0] = "field." + f[0]
		}
		b.WriteByte(',')
		b.Write(jsonQuoted(f[0]))
		b.WriteByte(':')
		b.Write(jsonQuoted(f[1]))
	}
	b.WriteByte('}')
	return b.Bytes()
}

func jsonQuoted(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

var logKeyRe = regexp.MustCompile(`^[a-z_][a-z0-9_.]*$`)

// parseLogLine splits "words key=value key="v w"" into a message and fields.
func parseLogLine(line string) logRecord {
	var rec logRecord
	var words []string
	rest := strings.TrimSpace(line)
	for rest != "" {
		tok, n := nextLogToken(rest)
		rest = strings.TrimLeft(rest[n:], " ")
		if k, v, ok := strings.Cut(tok, "="); ok && logKeyRe.MatchString(k) {
			if uq, err := strconv.Unquote(v); err == nil && strings.HasPrefix(v, `"`) {
				v = uq
			}
			rec.Fields = append(rec.Fields, [2]string{k, v})
			continue
		}
		words = append(words, tok)
	}
	rec.Msg = strings.Join(words, " ")
	rec.Level = logLevel(rec)
	return rec
}

// nextLogToken returns the next space-separated token, keeping key="a b" whole.
func nextLogToken(s string) (string, int) {
	eq := strings.Index(s, `="`)
	sp := strings.IndexByte(s, ' ')
	if eq >= 0 && (sp < 0 || eq < sp) {
		// key="...": find the closing quote, honouring escapes.
		for i := eq + 2; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				return s[:i+1], i + 1
			}
		}
		return s, len(s)
	}
	if sp < 0 {
		return s, len(s)
	}
	return s[:sp], sp
}

func logLevel(r logRecord) string {
	msg := strings.ToLower(r.Msg)
	status := r.field("status")
	switch {
	case r.field("error") != "" || status == StatusFailed || status == StatusInterrupted ||
		strings.Contains(msg, "cannot ") || strings.Contains(msg, "failed") || strings.HasPrefix(msg, "fatal"):
		return "error"
	case strings.Contains(msg, "warning") || strings.Contains(msg, "rejected") || strings.Contains(msg, "refused") ||
		strings.Contains(msg, "dropped") || status == StatusSkipped:
		return "warn"
	}
	if code, err := strconv.Atoi(status); err == nil && code >= 500 {
		return "error"
	}
	return "info"
}

// secretValues lists the resolved secrets of a config, for the log sink.
func (c *Config) secretValues() []string {
	v := []string{c.Server.apiToken, c.Notify.url, c.Notify.telegramToken, c.GitHub.token, c.Cloudflare.token,
		c.SMTP.password, c.Hub.token, c.PwPush.token}
	for _, d := range c.Deploy {
		v = append(v, string(d.secret), d.apiKey, d.apiSecret)
	}
	for _, h := range c.OTel.headers {
		v = append(v, h)
	}
	return v
}
