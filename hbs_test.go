package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderHbs(t *testing.T, src string, html bool, data any) string {
	t.Helper()
	tpl, err := parseHandlebars("t", src, html)
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, data); err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	return b.String()
}

func TestHandlebars(t *testing.T) {
	payload, _ := decodeJSON([]byte(`{"id":1234,"total":"49.00","paid":true,"coupon":null,"billing":{"first_name":"Ana","email":"ana@example.com"},
		"line_items":[{"name":"Hosting Pro","sku":"pro","quantity":1},{"name":"Dominio <.com>","sku":"dom","quantity":2}],"meta":{"b":"2","a":"1"}}`))
	data := mailData{Deploy: "orders", ResourceID: "1234",
		Output: map[string]string{"SITE_URL": "https://c1234.nimbox360.com", "NOTE": "<b>bold</b>"},
		Links:  map[string]string{"PASSWORD": "https://pw.nimbox360.com/p/abc/r"},
		Params: map[string]string{"PLAN": "pro"}, Payload: payload}.toHandlebarsData()

	cases := []struct{ src, want string }{
		{`Hola {{Payload.billing.first_name}}`, `Hola Ana`},
		{`{{Output.SITE_URL}} · #{{ResourceID}} · {{Params.PLAN}}`, `https://c1234.nimbox360.com · #1234 · pro`},
		{`{{Payload.line_items.0.sku}} {{Payload.line_items.[1].quantity}}`, `pro 2`},
		{`[{{Payload.missing.deep.path}}][{{Output.NOPE}}][{{Payload.coupon}}]`, `[][][]`},
		{`{{#if Payload.paid}}pagado{{else}}pendiente{{/if}}`, `pagado`},
		{`{{#if Payload.coupon}}cupón{{else}}sin cupón{{/if}}`, `sin cupón`},
		{`{{#unless Links.PASSWORD}}sin enlace{{/unless}}ok`, `ok`},
		{`{{#each Payload.line_items}}{{@index}}:{{name}}×{{quantity}};{{/each}}`, `0:Hosting Pro×1;1:Dominio <.com>×2;`},
		{`{{#each Payload.meta}}{{@key}}={{this}} {{/each}}`, `a=1 b=2 `},
		{`{{#each Payload.line_items}}{{sku}}@{{@root.Deploy}}/{{../ResourceID}} {{/each}}`, `pro@orders/1234 dom@orders/1234 `},
		{`{{#each Payload.nothing}}x{{else}}vacío{{/each}}`, `vacío`},
		{`{{#with Payload.billing}}{{first_name}} <{{email}}>{{/with}}`, `Ana <ana@example.com>`},
		{`a{{! comment }}b{{!-- {{block}} --}}c`, `abc`},
		{"x  {{~ Params.PLAN ~}}  y", `xproy`},
		{`{{{Output.NOTE}}}`, `<b>bold</b>`},
	}
	for _, c := range cases {
		if got := renderHbs(t, c.src, false, data); got != c.want {
			t.Errorf("%s\n got %q\nwant %q", c.src, got, c.want)
		}
	}

	// HTML: escaped by context, raw only with {{{ }}}.
	html := renderHbs(t, `<p>{{Payload.line_items.1.name}} {{Output.NOTE}} {{{Output.NOTE}}}</p><a href="{{Links.PASSWORD}}">ver</a>`, true, data)
	if html != `<p>Dominio &lt;.com&gt; &lt;b&gt;bold&lt;/b&gt; <b>bold</b></p><a href="https://pw.nimbox360.com/p/abc/r">ver</a>` {
		t.Errorf("html %q", html)
	}

	for src, want := range map[string]string{
		`{{#if x}}`:                "unclosed {{#if}}",
		`{{#if x}}{{/each}}`:       "does not close",
		`{{/if}}`:                  "does not close",
		`{{else}}`:                 "outside a block",
		`{{#each}}{{/each}}`:       "needs an argument",
		`{{#custom x}}{{/custom}}`: "not supported",
		`{{format date "YYYY"}}`:   "helpers and arguments are not supported",
		`{{@index}}`:               "outside {{#each}}",
		`{{ bad path! }}`:          "not supported",
		`{{x`:                      "unterminated",
		`{{!-- never closed`:       "unterminated",
		`{{{raw}`:                  "unterminated",
	} {
		if _, err := parseHandlebars("t", src, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", src, err, want)
		}
	}
}

func TestHandlebarsEmail(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "bienvenida.html.hbs")
	_ = os.WriteFile(tmpl, []byte(`<p>Hola {{Payload.billing.first_name}},</p>
{{#each Payload.line_items}}<li>{{name}}</li>{{/each}}
<p><a href="{{Links.PASSWORD}}">contraseña</a></p>`), 0o600)
	e := &EmailConfig{ToFrom: "billing.email", Subject: "Pedido #{{ResourceID}} de {{Payload.billing.first_name}}", Template: tmpl}
	if err := e.validate(true); err != nil {
		t.Fatal(err)
	}
	if !e.hbs || !e.html {
		t.Fatalf("hbs %v html %v", e.hbs, e.html)
	}
	payload, _ := decodeJSON([]byte(`{"billing":{"first_name":"Ana","email":"ana@example.com"},"line_items":[{"name":"Hosting"}]}`))
	s := &SMTPConfig{Host: "127.0.0.1", TLS: "none", From: "hola@nimbox360.com"}
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	data := &mailData{ResourceID: "9", Output: map[string]string{}, Payload: payload}
	data.Links = map[string]string{}
	m, err := composeEmail(e, s, nil, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := strings.ReplaceAll(string(m.Msg), "=\r\n", "")
	for _, want := range []string{"Subject: Pedido #9 de Ana", "<li>Hosting</li>", "Hola Ana,", "Content-Type: text/plain"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
	if strings.Join(m.Rcpt, ",") != "ana@example.com" {
		t.Errorf("rcpt %v", m.Rcpt)
	}
}

func TestExampleWelcomeTemplate(t *testing.T) {
	src, err := os.ReadFile("deploy/examples/templates/welcome.html.hbs")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := decodeJSON([]byte(`{"billing":{"first_name":"Ana"},"line_items":[{"name":"Hosting Pro","quantity":1}]}`))
	data := mailData{ResourceID: "1234", Output: map[string]string{"SITE_URL": "https://c1234.nimbox360.com", "USERNAME": "c1234"},
		Links: map[string]string{"PASSWORD": "https://pw.nimbox360.com/p/x/r"}, Payload: payload}.toHandlebarsData()
	out := renderHbs(t, string(src), true, data)
	for _, want := range []string{"Hola Ana,", "<b>#1234</b>", "<li>Hosting Pro × 1</li>", `href="https://pw.nimbox360.com/p/x/r"`, "<b>c1234</b>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Panel") {
		t.Error("PANEL_URL row should be hidden when empty")
	}
}
