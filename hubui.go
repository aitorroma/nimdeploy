package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The dashboard's templates, charts and static files. Pages are rendered on
// the server; htmx (served from the binary, like everything else) handles
// navigation, live panels and actions. The Content-Security-Policy allows
// only this origin's scripts and styles.

//go:embed hubstatic
var hubStaticFS embed.FS

func hubStatic() http.Handler {
	sub, _ := fs.Sub(hubStaticFS, "hubstatic")
	files := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	}))
}

// ---------- charts ----------

// sparkSVG is a small line chart with an area fill and the last point marked.
func sparkSVG(values []float64, color string) template.HTML {
	if len(values) < 2 {
		return ""
	}
	const w, h = 96.0, 34.0
	maxV := 0.0
	for _, v := range values {
		maxV = max(maxV, v)
	}
	if maxV == 0 {
		maxV = 1
	}
	step := w / float64(len(values)-1)
	var path strings.Builder
	var lx, ly float64
	for i, v := range values {
		lx, ly = float64(i)*step, h-3-(v/maxV)*(h-8)
		if i == 0 {
			fmt.Fprintf(&path, "M%.1f %.1f", lx, ly)
		} else {
			fmt.Fprintf(&path, " L%.1f %.1f", lx, ly)
		}
	}
	p := path.String()
	return template.HTML(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" aria-hidden="true"><path d="%s L%.0f %.0f L0 %.0f Z" fill="%s" opacity=".12"/><path d="%s" fill="none" stroke="%s" stroke-width="1.6" stroke-linejoin="round"/><circle cx="%.1f" cy="%.1f" r="2.6" fill="%s"/></svg>`,
		w, h, p, w, h, h, color, p, color, lx, ly, color))
}

// barsSVG draws deploys per day stacked by result, on one scale.
func barsSVG(lang string, days []dayCount) template.HTML {
	const W, H, padL, padB, padT = 760.0, 170.0, 28.0, 22.0, 8.0
	maxV := 5
	for _, d := range days {
		maxV = max(maxV, d.Success+d.Failed+d.Other)
	}
	maxV = (maxV + 4) / 5 * 5
	n := float64(len(days))
	bw := (W - padL) / n
	inner := bw * 0.62
	y := func(v int) float64 { return padT + (H-padT-padB)*(1-float64(v)/float64(maxV)) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s">`, W, H, template.HTMLEscapeString(hubT(lang, "chart.title")))
	for t := 0; t <= maxV; t += max(5, maxV/4/5*5) {
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="var(--line-soft)"/><text x="%.0f" y="%.1f" text-anchor="end">%d</text>`, padL, W, y(t), y(t), padL-6, y(t)+3, t)
	}
	labels := []string{hubT(lang, "chart.success"), hubT(lang, "chart.failed"), hubT(lang, "chart.other")}
	colors := []string{"var(--ok)", "var(--bad)", "var(--off)"}
	for i, d := range days {
		x := padL + float64(i)*bw + (bw-inner)/2
		base := 0
		for k, v := range []int{d.Success, d.Failed, d.Other} {
			if v == 0 {
				continue
			}
			y0, y1 := y(base), y(base+v)
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"><title>%s: %d</title></rect>`, x, y1, inner, y0-y1, colors[k], template.HTMLEscapeString(labels[k]), v)
			base += v
		}
		if i%2 == 1 || i == len(days)-1 {
			label := fmt.Sprintf("%d/%d", d.Day.Day(), int(d.Day.Month()))
			if i == len(days)-1 {
				label = hubT(lang, "chart.today")
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+inner/2, H-6, template.HTMLEscapeString(label))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// ---------- templates ----------

var hubTemplates = template.Must(template.New("hub").Funcs(template.FuncMap{
	"t":  hubT,
	"tf": func(lang, key string, args ...any) string { return hubT(lang, key, args...) },
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"inc":      func(i int) int { return i + 1 },
	"css":      func(s string) template.CSS { return template.CSS(s) },
	"attr":     func(s string) template.HTMLAttr { return template.HTMLAttr(s) },
	"url":      func(s string) template.URL { return template.URL(s) },
	"itoa":     strconv.Itoa,
	"lower":    strings.ToLower,
	"wait":     func(lang, name string) string { return hubT(lang, "new.wait", name) },
	"bg":       func(c string) template.CSS { return template.CSS("background:" + c) },
	"contains": strings.Contains,
	"errLine":  errLine,
	"foot": func(lang, name string, confirm bool) map[string]any {
		return map[string]any{"Lang": lang, "Name": name, "Confirm": confirm}
	},
	"envChoices": func() []string { return []string{"stage", "production", "dev"} },
	"steps":      func(lang string, n int) stepsData { return stepsData{Lang: lang, Step: n, Names: stepNames(lang)} },
	"stepsOOB": func(lang string, n int) stepsData {
		return stepsData{Lang: lang, Step: n, Names: stepNames(lang), OOB: true}
	},
	"actionsOf": func(p hubPage, d deployData) actionsData {
		ad := actionsData{Lang: p.Lang, CSRF: p.CSRF, Agent: d.Agent, Deploy: d.Deploy, Href: deployHref(d.Agent, d.Deploy),
			CanCommand: d.CanCommand, CanRollback: d.CanRollback, NoCommands: d.NoCommands, Commands: d.Commands}
		for _, c := range d.Commands {
			if c.StatusCls == "wait" || c.StatusCls == "run" {
				ad.Polling = true
			}
		}
		return ad
	},
}).Parse(hubHTML))

type stepsData struct {
	Lang  string
	Step  int
	Names []string
	OOB   bool
}

func stepNames(lang string) []string {
	return []string{hubT(lang, "new.step1"), hubT(lang, "new.step2"), hubT(lang, "new.step3")}
}

const hubHTML = `
{{define "head"}}<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false,"historyCacheSize":0}'>
<title>{{block "title" .}}nimdeploy hub{{end}}</title>
<link rel="stylesheet" href="/static/hub.css">
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 18 18'%3E%3Crect width='18' height='18' rx='4' fill='%23121915'/%3E%3Cpath d='M2 13h3l3-8 3 8h5' fill='none' stroke='%23e2e9e3' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E">
<script src="/static/htmx.min.js" defer></script>
<script src="/static/hub.js" defer></script>
</head>{{end}}

{{define "badge"}}<span class="badge s-{{.S}}">{{.L}}</span>{{end}}

{{define "page"}}{{template "head" .}}
<body hx-boost="true" hx-headers='{"X-CSRF-Token": "{{.CSRF}}"}' data-copied="{{if eq .Lang "es"}}Copiado{{else}}Copied{{end}}">
<div class="app">
  <aside class="side" id="side" aria-label="Navigation">
    <div class="brand">
      <span class="brand-mark" aria-hidden="true"><svg viewBox="0 0 18 18"><path d="M2 13h3l3-8 3 8h5" fill="none" stroke="#121915" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg></span>
      <span class="brand-name">nimdeploy<small>hub · {{.Version}}</small></span>
    </div>
    <nav class="nav">
      <a href="/" class="{{if eq .Page "overview"}}on{{end}}"><svg viewBox="0 0 16 16" fill="currentColor"><rect x="1" y="1" width="6" height="6" rx="1.5"/><rect x="9" y="1" width="6" height="6" rx="1.5"/><rect x="1" y="9" width="6" height="6" rx="1.5"/><rect x="9" y="9" width="6" height="6" rx="1.5"/></svg><span>{{t .Lang "nav.overview"}}</span></a>
      <a href="/activity" class="{{if eq .Page "activity"}}on{{end}}"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6"><path d="M1 8h3l2-5 4 10 2-5h3"/></svg><span>{{t .Lang "nav.activity"}}</span><span class="count">24h</span></a>
      <a href="/attention" class="{{if eq .Page "attention"}}on{{end}}"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6"><circle cx="8" cy="8" r="6.5"/><path d="M8 4.5v4M8 11v.5"/></svg><span>{{t .Lang "nav.attention"}}</span>{{if .Side.AttnCount}}<span class="count bad">{{.Side.AttnCount}}</span>{{end}}</a>
      <a href="/agents" class="{{if eq .Page "agents"}}on{{end}}"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6"><rect x="1.5" y="2.5" width="13" height="4" rx="1"/><rect x="1.5" y="9.5" width="13" height="4" rx="1"/><path d="M4 4.5h.5M4 11.5h.5"/></svg><span>{{t .Lang "nav.agents"}}</span><span class="count">{{.Side.AgentCount}}</span></a>
    </nav>
    <div class="side-section"><span class="label">{{t .Lang "side.clients"}}</span><span class="hint">{{t .Lang "side.from_label"}}</span></div>
    <div class="tree" id="tree">
      {{range .Side.Clients}}<div data-client-block="{{.Key}}">
        <a href="{{.Href}}" class="client {{if .Open}}open{{end}} {{if .On}}on{{end}}"><span class="chev" role="button" aria-label="toggle">▶</span><span class="avatar" style="{{bg .Color}}">{{.Initials}}</span><span class="cname">{{.Name}}</span>{{if .Failing}}<span class="pips"><span class="pip bad"></span></span>{{end}}</a>
        <div class="envs">{{range .Envs}}<a href="{{.Href}}" class="{{if .On}}on{{end}}"><span class="dot {{.Dot}}"></span>{{.Name}}<span class="pips">{{range .Pips}}<span class="pip {{.Cls}}" title="{{.Title}}"></span>{{end}}</span></a>{{end}}</div>
      </div>{{else}}<div style="padding:8px 10px;color:var(--side-muted);font-size:13px">—</div>{{end}}
    </div>
    <div class="side-foot">
      <span class="dot up"></span><span class="who">{{if eq .Who "token"}}admin{{else}}{{.Who}}{{end}}</span>
      <span class="lang">{{range .Side.Langs}}<a href="{{.Href}}" hx-boost="false" class="{{if .On}}on{{end}}">{{.Code}}</a>{{end}}</span>
      <button type="button" data-theme-toggle class="link">{{t .Lang "top.theme"}}</button>
      {{if .Auth}}<form method="post" action="/logout" hx-boost="false"><button class="link">{{t .Lang "top.logout"}}</button></form>{{end}}
    </div>
  </aside>
  <div class="main">
    <header class="top">
      <button class="btn small menu-btn" type="button" data-menu aria-label="Menu">☰</button>
      <div class="crumbs">{{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}{{if $c.Href}}<a href="{{$c.Href}}">{{$c.Label}}</a>{{else}}<b>{{$c.Label}}</b>{{end}}{{end}}</div>
      <label class="search"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="7" cy="7" r="5"/><path d="M11 11l3.5 3.5"/></svg><input id="q" type="search" placeholder="{{t .Lang "top.search"}}" aria-label="{{t .Lang "top.search"}}" autocomplete="off"><span class="kbd">⌘K</span></label>
      <span class="live"><i></i>{{t .Lang "top.live"}}</span>
      <button class="btn primary" type="button" hx-get="/ui/agents/new" hx-target="#drawer"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 3v10M3 8h10"/></svg>{{t .Lang "top.add_agent"}}</button>
    </header>
    <main class="view" id="view">
      {{if eq .Page "overview"}}{{template "overview" .}}
      {{else if eq .Page "client"}}{{template "client" .}}
      {{else if eq .Page "deploy"}}{{template "deploy" .}}
      {{else if eq .Page "agents"}}{{template "agents" .}}
      {{else if eq .Page "activity"}}{{template "activity" .}}
      {{else if eq .Page "attention"}}{{template "attention" .}}{{end}}
    </main>
  </div>
</div>
<div class="scrim" id="scrim" hidden></div>
<aside class="drawer" id="drawer" hidden aria-label="{{t .Lang "new.title"}}"></aside>
<div class="toast" id="toast" hidden></div>
</body></html>{{end}}

{{define "attn-list"}}<ul class="attn">{{range .}}<li data-href="{{.Href}}" tabindex="0"><span class="stripe {{if eq .Sev "warn"}}warn{{end}}"></span><div style="min-width:0"><div class="t"><a href="{{.Href}}">{{.Title}}</a></div><div class="d">{{.Desc}}</div></div><time>{{.Ago}}</time></li>{{end}}</ul>{{end}}

{{define "timeline"}}<ul class="timeline">{{range .}}<li><time>{{.Ago}}</time><span class="node {{.Node}}"></span><div class="what"><a href="{{.Href}}">{{.Deploy}}</a> <span class="muted">· {{.Client}}</span> {{.What}}{{if .Sub}}<div class="sub">{{.Sub}}</div>{{end}}</div></li>{{end}}</ul>{{end}}

{{define "hist"}}<span class="runs-dots" title="{{.HistTitle}}">{{range .Hist}}<i class="{{.}}"></i>{{end}}</span>{{end}}

{{define "overview"}}{{$l := .Lang}}{{with .Data}}
<div class="view-head"><div><h1>{{t $l "ov.title"}}</h1><p>{{.Sub}}</p></div>
  <div class="actions"><div class="seg" role="group">{{range .EnvLinks}}<a href="{{.Href}}" class="{{if .On}}on{{end}}">{{.Label}}</a>{{end}}</div></div></div>
<section class="kpis" id="kpis" hx-get="{{$.Path}}" hx-select="#kpis" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">
  {{range .KPIs}}<div class="kpi {{.Cls}}"><span class="label">{{.Label}}</span><b>{{.Value}}{{if .Unit}}<span style="font-size:16px;color:var(--muted);font-weight:500">{{.Unit}}</span>{{end}}</b><span class="delta {{.DeltaCls}}">{{.Delta}}</span>{{.Spark}}</div>{{end}}
</section>
<div class="grid-main">
  <div class="stack">
    <section class="panel">
      <div class="panel-head"><h2>{{t $l "chart.title"}}</h2><span class="sub">{{t $l "chart.sub"}}</span><div class="actions legend"><span><i style="background:var(--ok)"></i>{{t $l "chart.success"}}</span><span><i style="background:var(--bad)"></i>{{t $l "chart.failed"}}</span><span><i style="background:var(--off)"></i>{{t $l "chart.other"}}</span></div></div>
      <div class="chart-wrap">{{.Chart}}</div>
    </section>
    <section class="panel" id="matrix" hx-get="{{$.Path}}" hx-select="#matrix" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">
      <div class="panel-head"><h2>{{t $l "mx.title"}}</h2><span class="sub">{{t $l "mx.sub"}}</span></div>
      <div class="scroll"><table class="matrix">
        <thead><tr><th>{{t $l "mx.client"}}</th>{{range .Envs}}<th scope="col">{{.}}</th>{{end}}</tr></thead>
        <tbody>{{range .Rows}}<tr><th scope="row"><a href="{{.Href}}"><span class="avatar" style="{{bg .Color}}">{{.Initials}}</span>{{.Client}}</a></th>
          {{range .Cells}}{{if .Empty}}<td class="empty-cell">—</td>{{else}}<td><div class="cell-sum"><span>{{.Apps}}</span>{{if .Failing}}<span class="b">{{.Failing}}</span>{{end}}</div><div class="chips">{{range .Chips}}<a class="chip {{if .Stale}}stale{{end}}" href="{{.Href}}" title="{{.Title}}"><span class="pip {{.Cls}}"></span>{{.Name}}</a>{{end}}</div></td>{{end}}{{end}}
        </tr>{{end}}</tbody>
      </table></div>
    </section>
  </div>
  <div class="stack">
    <section class="panel" id="attn" hx-get="{{$.Path}}" hx-select="#attn" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">
      <div class="panel-head"><h2>{{t $l "attn.title"}}</h2>{{if .Attn}}<span class="badge s-failed" style="margin-left:auto">{{len .Attn}}</span>{{end}}</div>
      {{if .Attn}}{{template "attn-list" .Attn}}{{else}}<div class="all-clear">{{t $l "attn.clear"}}</div>{{end}}
    </section>
    <section class="panel" id="activity" hx-get="{{$.Path}}" hx-select="#activity" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">
      <div class="panel-head"><h2>{{t $l "act.title"}}</h2><div class="actions"><a href="/activity" style="font-size:12.5px">{{t $l "act.all"}}</a></div></div>
      {{template "timeline" .Timeline}}
    </section>
  </div>
</div>{{end}}{{end}}

{{define "client"}}{{$l := .Lang}}{{with .Data}}
<div class="view-head"><div class="client-head"><span class="avatar" style="{{bg .Color}}">{{.Initials}}</span><div><h1>{{.Name}}{{if .Env}} <span class="muted" style="font-weight:500;text-transform:capitalize">· {{.Env}}</span>{{end}}</h1>
  <div class="labels">{{range .Labels}}<span class="lbl">{{.}}</span>{{end}}</div></div></div>
  <div class="actions"><span class="muted" style="align-self:center;font-size:12.5px">{{.Summary}}</span><button class="btn" type="button" hx-get="{{.AddHref}}" hx-target="#drawer">{{t $l "cl.add_server"}}</button></div></div>
<div class="envgrid" id="envgrid" hx-get="{{$.Path}}" hx-select="#envgrid" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">{{range .Cols}}
  <section class="panel env-col">
    <div class="panel-head"><h2>{{.Name}}</h2><span class="env-tag {{if .Prod}}prod{{end}}">{{.Tag}}</span><span class="sub" style="margin-left:auto">{{.Count}}</span></div>
    {{range .Apps}}<div class="app-row" data-href="{{.Href}}" tabindex="0" role="link">
      <span class="stripe {{.Stripe}}"></span>
      <span class="name"><a href="{{.Href}}" style="color:inherit">{{.Name}}</a><span class="kind">{{.Kind}}</span></span>
      <span class="right">{{.Badge}}{{template "hist" .}}</span>
      <span class="meta">{{if .Commit}}<span class="sha">{{.Commit}}</span>{{end}}{{if .Msg}}<span>{{.Msg}}</span>{{end}}{{if .Schedule}}<span class="sha">{{.Schedule}}</span>{{end}}{{if .By}}<span>{{if .ByInit}}<span class="who-av">{{.ByInit}}</span> {{end}}{{.By}}</span>{{end}}<span>{{.Ago}}</span>{{if .Took}}<span>· {{.Took}}</span>{{end}}</span>
    </div>{{else}}<div class="setup">{{t $l "cl.no_apps"}}</div>{{end}}
    {{range .Agents}}<div class="agent-line"><span class="dot {{.Dot}}"></span><span><a href="{{.Href}}">{{.Name}}</a> · {{.Text}}</span></div>{{end}}
  </section>{{end}}
</div>{{end}}{{end}}

{{define "deploy"}}{{$l := .Lang}}{{with .Data}}
<div class="hero"><div><h1>{{.Deploy}} {{.Badge}}</h1>
  <div class="sub"><span>{{.Client}}</span><span>·</span><span style="text-transform:capitalize">{{.Env}}</span><span>·</span><span><span class="dot {{.AgentDot}}"></span> {{.Agent}}</span>{{template "hist" .}}</div></div>
  <div class="actions" style="display:flex;gap:8px"><button class="btn" type="button" data-copy-text="{{.RunCmd}}">{{t $l "dp.copy_cmd"}}</button></div></div>
<section class="panel facts">{{range .Facts}}<div><span class="label">{{index . 0}}</span><div class="v">{{index . 1}}</div></div>{{end}}</section>
{{if .Error}}<div class="note" style="background:var(--bad-soft)"><b style="color:var(--bad)">{{t $l "dp.error"}}</b><span>{{.Error}}</span></div>{{end}}
<div class="grid-main">
  <div class="stack">
    {{if .Phases}}<section class="panel"><div class="panel-head"><h2>{{t $l "dp.phases"}}</h2><span class="sub">{{t $l "dp.phases_sub"}}</span></div>
      <div class="waterfall">{{range .Phases}}<div class="wf-row"><span class="n" title="{{.Name}}">{{.Name}}</span><div class="wf-track"><div class="wf-bar {{.Cls}}" style="{{css (print "left:" .Left "%;width:" .Width "%")}}"></div></div><span class="t">{{.Took}}</span></div>{{end}}
      <div class="wf-axis"><div></div><div><span>0s</span><span>{{.PhaseMid}}</span><span>{{.PhaseEnd}}</span></div><div></div></div></div></section>{{end}}
    {{if .Hosts}}<section class="panel"><div class="panel-head"><h2>{{t $l "dp.hosts"}}</h2><span class="sub">{{.HostsSub}}</span></div>
      <div class="hosts">{{range .Hosts}}<span class="host {{.Outcome}}"><span class="dot {{if or (eq .Outcome "failed") (eq .Outcome "unreachable")}}down{{else}}up{{end}}"></span>{{.Host}}</span>{{end}}</div></section>{{end}}
    {{if .Log}}<section class="panel"><div class="panel-head"><h2>{{t $l "dp.log"}}</h2><span class="sub">{{t $l "dp.log_sub"}}</span></div><pre class="term">{{range $i, $line := .Log}}<div class="ln {{if errLine $line}}err{{end}}"><span>{{inc $i}}</span><span>{{$line}}</span></div>{{end}}</pre></section>{{end}}
  </div>
  <div class="stack">
    {{template "actions" (actionsOf $ .)}}
    <section class="panel" id="runs" hx-get="{{$.Path}}" hx-select="#runs" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">
      <div class="panel-head"><h2>{{t $l "dp.runs"}}</h2></div>
      {{if .Runs}}<table class="runs-table"><tbody>{{range .Runs}}<tr class="{{if .Current}}cur{{end}}"><td class="muted num">{{.Ago}}</td><td>{{.Badge}}</td><td>{{if .Commit}}<span class="sha">{{.Commit}}</span>{{end}}</td><td class="muted">{{.By}}</td><td class="muted num" style="text-align:right">{{.Took}}</td></tr>{{end}}</tbody></table>{{else}}<p class="muted" style="padding:14px 16px;margin:0">{{t $l "app.no_runs"}}</p>{{end}}
      {{if .Labels}}<div class="agent-line"><span>{{t $l "dp.labels"}}</span>{{range .Labels}}<span class="lbl">{{.}}</span>{{end}}</div>{{end}}
    </section>
  </div>
</div>{{end}}{{end}}

{{define "actions"}}<section class="panel" id="actions" {{if .Polling}}hx-get="/ui{{.Href}}/actions" hx-trigger="every 3s" hx-target="this" hx-swap="outerHTML"{{end}}>
  <div class="panel-head"><h2>{{t .Lang "dp.commands"}}</h2></div>
  <div style="padding:12px 16px;display:flex;flex-direction:column;gap:10px">
  {{if .Question}}<div class="confirm-box"><span>{{.Question}}</span>
      <button class="btn small primary" type="button" hx-post="/ui{{.Href}}/{{.Action}}" hx-target="#actions" hx-swap="outerHTML">{{t .Lang "dp.confirm"}}</button>
      <button class="btn small" type="button" hx-get="/ui{{.Href}}/actions" hx-target="#actions" hx-swap="outerHTML">{{t .Lang "dp.cancel"}}</button></div>
  {{else if .CanCommand}}<div style="display:flex;gap:8px;flex-wrap:wrap">
      <button class="btn" type="button" hx-get="/ui{{.Href}}/confirm/run" hx-target="#actions" hx-swap="outerHTML">{{t .Lang "dp.run_again"}}</button>
      {{if .CanRollback}}<button class="btn" type="button" hx-get="/ui{{.Href}}/confirm/rollback" hx-target="#actions" hx-swap="outerHTML">{{t .Lang "dp.rollback"}}</button>{{end}}</div>{{end}}
  {{if .NoCommands}}<div class="muted" style="font-size:12.5px">{{.NoCommands}}</div>{{end}}
  </div>
  {{if .Commands}}<ul class="cmds">{{range .Commands}}<li><span class="badge s-{{if eq .StatusCls "ok"}}success{{else if eq .StatusCls "bad"}}failed{{else if eq .StatusCls "run"}}running{{else}}queued{{end}}">{{.Status}}</span><b>{{.Action}}</b><span class="muted">{{.By}} · {{.Ago}}</span>{{if .Result}}<span class="muted">· {{.Result}}</span>{{end}}</li>{{end}}</ul>{{end}}
</section>{{end}}

{{define "agents"}}{{$l := .Lang}}
<div class="view-head"><div><h1>{{t $l "ag.title"}}</h1><p>{{t $l "ag.sub"}}</p></div>
  <div class="actions"><button class="btn primary" type="button" hx-get="/ui/agents/new" hx-target="#drawer">{{t $l "top.add_agent"}}</button></div></div>
<div class="agents" id="agentcards" hx-get="{{$.Path}}" hx-select="#agentcards" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">{{range .Data.Cards}}
  <section class="panel agent {{.State}}" id="agent-{{.Name}}">
    <div class="top-row"><span class="avatar" style="{{bg .Color}}">{{.Initials}}</span><div><b>{{.Name}}</b><div class="muted" style="font-size:12px">{{.Where}}</div></div><span class="badge s-{{.State}}">{{.StateLabel}}</span></div>
    {{if .Pending}}<div class="setup" style="margin:0">{{t $l "ag.pending_note"}}</div>{{else}}
    <div><div class="label" style="margin-bottom:5px">{{t $l "ag.beats"}}</div><div class="beats">{{range .Beats}}<i class="{{if not .}}miss{{end}}"></i>{{end}}</div></div>
    <dl><dt>{{t $l "ag.host"}}</dt><dd class="mono">{{.Host}}</dd><dt>{{t $l "ag.version"}}</dt><dd><span class="mono">{{.Version}}</span>{{if .Old}} <span class="badge s-queued">{{t $l "ag.update"}}</span>{{end}}</dd>
    <dt>{{t $l "ag.last_seen"}}</dt><dd>{{.LastSeen}}</dd><dt>{{t $l "ag.up_since"}}</dt><dd>{{.UpSince}}</dd><dt>{{t $l "ag.waiting"}}</dt><dd>{{.Waiting}}</dd>
    <dt>{{t $l "ag.cert"}}</dt><dd>{{.Cert}}</dd><dt>{{t $l "ag.commands"}}</dt><dd>{{.CommandsLabel}}</dd></dl>{{end}}
    {{template "agent-foot" (foot $l .Name false)}}
  </section>{{else}}<div class="setup" style="margin:0">{{t $l "ag.none"}}</div>{{end}}
</div>{{end}}

{{define "agent-foot"}}<div class="foot" id="foot-{{.Name}}">{{if .Confirm}}<span class="muted" style="font-size:12.5px;align-self:center">{{if eq .Lang "es"}}¿Revocar {{.Name}}? Sus eventos se rechazarán.{{else}}Revoke {{.Name}}? Its events will be refused.{{end}}</span>
  <button class="btn small danger" type="button" hx-post="/ui/agents/{{.Name}}/revoke">{{t .Lang "ag.confirm_revoke"}}</button>
  <button class="btn small" type="button" hx-get="/ui/agents/{{.Name}}/foot" hx-target="#foot-{{.Name}}" hx-swap="outerHTML">{{t .Lang "dp.cancel"}}</button>
{{else}}<button class="btn small" type="button" hx-get="/ui/agents/{{.Name}}/renew" hx-target="#drawer">{{t .Lang "ag.new_token"}}</button>
  <button class="btn small danger" type="button" hx-get="/ui/agents/{{.Name}}/foot?confirm=1" hx-target="#foot-{{.Name}}" hx-swap="outerHTML">{{t .Lang "ag.revoke"}}</button>{{end}}</div>{{end}}

{{define "activity"}}{{$l := .Lang}}<div class="view-head"><div><h1>{{t $l "act.title"}}</h1><p>{{t $l "act.sub"}}</p></div></div>
<section class="panel" id="actpanel" hx-get="{{$.Path}}" hx-select="#actpanel" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">{{template "timeline" .Data.Timeline}}</section>{{end}}

{{define "attention"}}{{$l := .Lang}}<div class="view-head"><div><h1>{{t $l "attn.title"}}</h1><p>{{t $l "attn.sub"}}</p></div></div>
<section class="panel" id="attnpanel" hx-get="{{$.Path}}" hx-select="#attnpanel" hx-target="this" hx-swap="outerHTML" hx-trigger="every 20s">{{if .Data.Attn}}{{template "attn-list" .Data.Attn}}{{else}}<div class="all-clear">{{t $l "attn.clear"}}</div>{{end}}</section>{{end}}

{{define "steps"}}<div class="steps" id="steps"{{if .OOB}} hx-swap-oob="true"{{end}}>{{range $i, $s := .Names}}<span class="{{if eq (inc $i) $.Step}}on{{else if lt (inc $i) $.Step}}done{{end}}">{{$s}}</span>{{end}}</div>{{end}}

{{define "drawer-new"}}{{$l := .Lang}}<div class="drawer-head"><h2>{{t $l "new.title"}}</h2><button class="btn small x" type="button" data-close>{{t $l "new.close"}}</button></div>
<form class="drawer-body" id="agentForm" hx-post="/ui/agents" hx-target="#drawer" hx-headers='{"X-CSRF-Token": "{{.CSRF}}"}'>
  {{template "steps" (steps $l 1)}}
  <p class="muted" style="margin:0">{{t $l "new.intro"}}</p>
  {{if .Error}}<div class="note" style="background:var(--bad-soft)">{{.Error}}</div>{{end}}
  <div class="field"><label for="ag-client">{{t $l "new.client"}}</label><input id="ag-client" name="client" list="clientList" value="{{.Client}}" placeholder="Acme" required maxlength="200"><datalist id="clientList">{{range .Clients}}<option value="{{.}}">{{end}}</datalist><span class="hint">{{t $l "new.client_hint"}}</span></div>
  <div class="row2">
    <div class="field"><label for="ag-env">{{t $l "new.env"}}</label><select id="ag-env" name="env">{{range $e := envChoices}}<option {{if eq $e $.Env}}selected{{end}}>{{$e}}</option>{{end}}</select></div>
    <div class="field"><label for="ag-name">{{t $l "new.name"}}</label><input id="ag-name" name="name" value="{{.Name}}" placeholder="acme-stage" required pattern="[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}"><span class="hint">{{t $l "new.name_hint"}}</span></div>
  </div>
  <label class="toggle" for="ag-cert"><span class="tt">{{t $l "new.cert"}}</span><span class="td">{{if .CertAvailable}}{{t $l "new.cert_d"}}{{else}}{{t $l "new.cert_off"}}{{end}}</span><input type="checkbox" id="ag-cert" name="cert" {{if .RequireCert}}checked{{end}} {{if not .CertAvailable}}disabled{{end}}><span class="sw"></span></label>
</form>
<div class="drawer-foot"><button class="btn" type="button" data-close>{{t $l "new.cancel"}}</button><button class="btn primary" type="submit" form="agentForm">{{t $l "new.create"}}</button></div>{{end}}

{{define "drawer-renew"}}{{$l := .Lang}}<div class="drawer-head"><h2>{{t $l "ag.new_token"}} · {{.Name}}</h2><button class="btn small x" type="button" data-close>{{t $l "new.close"}}</button></div>
<form class="drawer-body" id="renewForm" hx-post="/ui/agents/{{.Name}}/renew" hx-target="#drawer" hx-headers='{"X-CSRF-Token": "{{.CSRF}}"}'>
  <div class="note">{{if eq $l "es"}}El token actual{{if .RequireCert}} y el certificado{{end}} dejarán de funcionar en cuanto confirmes.{{else}}The current token{{if .RequireCert}} and certificate{{end}} stop working as soon as you confirm.{{end}}</div>
  <label class="toggle" for="rn-cert"><span class="tt">{{t $l "new.cert"}}</span><span class="td">{{if .CertAvailable}}{{t $l "new.cert_d"}}{{else}}{{t $l "new.cert_off"}}{{end}}</span><input type="hidden" name="cert" value="off"><input type="checkbox" id="rn-cert" name="cert" {{if .RequireCert}}checked{{end}} {{if not .CertAvailable}}disabled{{end}}><span class="sw"></span></label>
</form>
<div class="drawer-foot"><button class="btn" type="button" data-close>{{t $l "new.cancel"}}</button><button class="btn primary" type="submit" form="renewForm">{{t $l "ag.new_token"}}</button></div>{{end}}

{{define "drawer-issued"}}{{$l := .Lang}}<div class="drawer-head"><h2>{{tf $l "new.connect" .Name}}</h2><button class="btn small x" type="button" data-close>{{t $l "new.close"}}</button></div>
<div class="drawer-body">{{template "steps" (steps $l 2)}}
  <div class="field"><span class="lab">{{t $l "new.token"}}</span><div class="secret"><code id="tok">{{.Token}}</code><button class="btn small" type="button" data-copy="tok">{{t $l "new.copy"}}</button></div></div>
  <div class="note"><b>{{t $l "new.once"}}</b><span>{{t $l "new.once_d"}}</span></div>
  {{if .CertPEM}}<div class="field"><span class="lab">{{t $l "new.certfiles"}} · {{tf $l "ag.cert_until" .CertNotAfter}}</span>
    <div class="dl"><a class="btn small" href="{{url .CertDataURL}}" download="hub-client.pem" hx-boost="false">{{t $l "new.save_cert"}}</a><a class="btn small" href="{{url .KeyDataURL}}" download="hub-client.key" hx-boost="false">{{t $l "new.save_key"}}</a></div>
    <textarea class="pem" id="pem-cert" readonly>{{.CertPEM}}</textarea><textarea class="pem" id="pem-key" readonly>{{.KeyPEM}}</textarea></div>{{end}}
  {{if not .Renew}}<div class="field"><span class="lab"><span class="stepnum">1</span>{{t $l "new.s1"}}</span><div class="snippet"><pre id="sn-install">curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh</pre><button class="btn small" type="button" data-copy="sn-install">{{t $l "new.copy"}}</button></div></div>
  <div class="field"><span class="lab"><span class="stepnum">2</span>{{t $l "new.s2"}}</span><div class="snippet"><pre id="sn-cfg">{{.Config}}</pre><button class="btn small" type="button" data-copy="sn-cfg">{{t $l "new.copy"}}</button></div></div>{{end}}
  <div class="field"><span class="lab"><span class="stepnum">{{if .Renew}}1{{else}}3{{end}}</span>{{t $l "new.s3"}}</span><div class="snippet"><pre id="sn-sec">NIMDEPLOY_HUB_TOKEN={{.Token}}

# user installs; root installs: systemctl restart nimdeploy
systemctl --user restart nimdeploy</pre><button class="btn small" type="button" data-copy="sn-sec">{{t $l "new.copy"}}</button></div></div>
  {{template "drawer-wait" .}}
</div>
<div class="drawer-foot"><button class="btn" type="button" data-close>{{t $l "new.done"}}</button></div>{{end}}

{{define "drawer-wait"}}{{if .Connected}}<div class="waiting done" id="wait"><span class="check">✓</span><span><b>{{.Connected}}</b> <a href="{{.OpenHref}}">{{.OpenLabel}}</a></span></div>
{{template "steps" (stepsOOB .Lang 3)}}{{else}}<div class="waiting" id="wait" hx-get="{{.WaitHref}}" hx-trigger="every 3s" hx-target="this" hx-swap="outerHTML"><span class="spinner"></span><span>{{wait .Lang .Name}}</span></div>{{end}}{{end}}

{{define "login"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>nimdeploy hub</title><link rel="stylesheet" href="/static/hub.css"></head>
<body><form class="login" method="post" action="/login">
<h1 style="font-size:20px">nimdeploy hub</h1>
<p class="muted" style="margin:0">{{.Hint}}</p>
{{if .Error}}<p class="error" style="margin:0">{{.Error}}</p>{{end}}
<input type="hidden" name="next" value="{{.Next}}">
<input type="password" name="token" autocomplete="current-password" autofocus required aria-label="Token">
<button class="btn primary">{{.Button}}</button>
</form></body></html>{{end}}

{{define "error"}}<!doctype html><html><head><meta charset="utf-8"><title>nimdeploy hub</title><link rel="stylesheet" href="/static/hub.css"></head><body><div class="view"><p class="error">{{.Error}}</p><p><a href="/">←</a></p></div></body></html>{{end}}
`
