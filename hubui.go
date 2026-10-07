package main

import (
	"html/template"
	"net/url"
	"strings"
	"time"
)

// The hub's dashboard: server-rendered pages, no JavaScript, one stylesheet
// (the Content-Security-Policy allows nothing else).

var hubTemplates = template.Must(template.New("hub").Funcs(template.FuncMap{
	"ago": func(t any) string {
		switch v := t.(type) {
		case time.Time:
			return agoText(v, time.Now())
		case *time.Time:
			if v != nil {
				return agoText(*v, time.Now())
			}
		}
		return ""
	},
	"stamp": func(t any) string {
		switch v := t.(type) {
		case time.Time:
			if !v.IsZero() {
				return v.UTC().Format("2006-01-02 15:04:05 UTC")
			}
		case *time.Time:
			if v != nil {
				return v.UTC().Format("2006-01-02 15:04:05 UTC")
			}
		}
		return ""
	},
	"short": func(s string) string {
		if len(s) > 10 {
			return s[:10]
		}
		return s
	},
	"ref":    func(st *State) string { return eventRef(*st) },
	"labels": formatLabels,
	"filter": func(f hubFilter, key, value string) string {
		q := url.Values{}
		set := func(k, v string) {
			if v != "" {
				q.Set(k, v)
			}
		}
		set("client", f.Client)
		set("environment", f.Env)
		set("agent", f.Agent)
		set("status", f.Status)
		if value == "" {
			q.Del(key)
		} else {
			q.Set(key, value)
		}
		if len(q) == 0 {
			return "/"
		}
		return "/?" + q.Encode()
	},
	"deployURL": func(agent, deploy string) string {
		return "/d/" + url.PathEscape(agent) + "/" + url.PathEscape(deploy)
	},
	"cell": func(m hubMatrix, client, env string) []hubView { return m.Cells[client][env] },
	"eventText": func(typ string) string {
		switch typ {
		case hubEventDeployStart:
			return "started"
		case hubEventDeployDone:
			return "finished"
		case hubEventRejected:
			return "rejected"
		}
		return typ
	},
	"lines": func(l []string) string { return strings.Join(l, "\n") },
	"or": func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	},
}).Parse(hubHTML))

const hubHTML = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
{{if eq .Page "overview"}}<meta http-equiv="refresh" content="30">{{end}}
<title>{{if .D}}{{.D.Deploy}} · {{end}}nimdeploy hub</title>
<link rel="stylesheet" href="/static/hub.css">
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Ccircle cx='8' cy='8' r='7' fill='%232f6fed'/%3E%3C/svg%3E">
</head><body>
<header class="top"><a class="brand" href="/">nimdeploy <span>hub</span></a>
{{if .Who}}<nav><a href="/"{{if eq .Page "overview"}} class="on"{{end}}>Deploys</a><a href="/agents"{{if eq .Page "agents"}} class="on"{{end}}>Agents</a></nav>
<div class="who">{{if ne .Who "token"}}{{if ne .Who "anonymous"}}{{.Who}}{{end}}{{end}}
{{if .Auth}}<form method="post" action="/logout"><button class="link">Log out</button></form>{{end}}</div>{{end}}
</header><main>{{end}}

{{define "foot"}}</main><footer>nimdeploy {{.Version}}</footer></body></html>{{end}}

{{define "badge"}}<span class="badge s-{{or .Status "never"}}">{{or .Status "never"}}</span>{{end}}

{{define "login"}}{{template "head" .}}
<form class="login" method="post" action="/login">
<h1>Sign in</h1>
<p class="muted">The hub token ($NIMDEPLOY_HUB_TOKEN).</p>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<input type="hidden" name="next" value="{{.Next}}">
<input type="password" name="token" autocomplete="current-password" autofocus required aria-label="Token">
<button>Sign in</button>
</form>
{{template "foot" .}}{{end}}

{{define "error"}}{{template "head" .}}<p class="error">{{.Error}}</p>{{template "foot" .}}{{end}}

{{define "overview"}}{{template "head" .}}
<section class="stats">
<a class="stat" href="{{filter .Filter "status" ""}}"><b>{{.Total}}</b><span>deploys</span></a>
<a class="stat{{if .Failing}} bad{{end}}" href="{{filter .Filter "status" "failed"}}"><b>{{.Failing}}</b><span>failing</span></a>
<a class="stat{{if .Running}} busy{{end}}" href="{{filter .Filter "status" "running"}}"><b>{{.Running}}</b><span>running</span></a>
<a class="stat" href="/agents"><b>{{.Online}}</b><span>agents online</span></a>
<a class="stat{{if .Offline}} bad{{end}}" href="/agents"><b>{{.Offline}}</b><span>offline</span></a>
</section>

<form class="filters" method="get" action="/">
<label>Client <select name="client"><option value="">all</option>{{range .Clients}}<option{{if eq . $.Filter.Client}} selected{{end}}>{{.}}</option>{{end}}</select></label>
<label>Environment <select name="environment"><option value="">all</option>{{range .Envs}}<option{{if eq . $.Filter.Env}} selected{{end}}>{{.}}</option>{{end}}</select></label>
<label>Agent <select name="agent"><option value="">all</option>{{range .AgentNames}}<option{{if eq . $.Filter.Agent}} selected{{end}}>{{.}}</option>{{end}}</select></label>
<label>Status <select name="status"><option value="">all</option>{{range .Statuses}}<option{{if eq . $.Filter.Status}} selected{{end}}>{{.}}</option>{{end}}</select></label>
<button>Filter</button>{{if .Filter.Active}}<a href="/">Clear</a>{{end}}
</form>

{{if not .Deploys}}<p class="empty">No deploys{{if .Filter.Active}} match these filters{{else}} yet. Create an agent with <code>nimdeploy hub agent add &lt;name&gt;</code> and add <code>[hub]</code> to its server's config{{end}}.</p>
{{else}}
<h2>By client and environment</h2>
<div class="scroll"><table class="matrix">
<thead><tr><th></th>{{range .Matrix.Envs}}<th>{{or . "—"}}</th>{{end}}</tr></thead>
<tbody>{{range $c := .Matrix.Clients}}<tr><th><a href="{{filter $.Filter "client" $c}}">{{or $c "no client"}}</a></th>
{{range $e := $.Matrix.Envs}}<td>{{range cell $.Matrix $c $e}}<a class="chip s-{{or .State.Status "never"}}{{if not .AgentOnline}} stale{{end}}" href="{{deployURL .Agent .Deploy}}" title="{{.Agent}} · {{or .State.Status "never"}} {{ago .When}}">{{.Deploy}}</a>{{end}}</td>{{end}}
</tr>{{end}}</tbody></table></div>

<h2>Deploys</h2>
<div class="scroll"><table class="list">
<thead><tr><th>Deploy</th><th>Client</th><th>Env</th><th>Agent</th><th>Status</th><th>Commit</th><th>When</th><th>Took</th></tr></thead>
<tbody>{{range .Deploys}}<tr>
<td><a href="{{deployURL .Agent .Deploy}}">{{.Deploy}}</a>{{if not .InInventory}} <span class="muted">(removed)</span>{{end}}</td>
<td>{{.Client}}</td><td>{{.Env}}</td>
<td><span class="dot{{if .AgentOnline}} up{{end}}"></span>{{.Agent}}</td>
<td>{{template "badge" .State}}</td>
<td><code>{{short .State.Commit}}</code></td>
<td title="{{stamp .When}}">{{ago .When}}</td><td>{{.State.Duration}}</td>
</tr>{{end}}</tbody></table></div>
{{end}}

{{if .Timeline}}<h2>Activity</h2>
<ol class="timeline">{{range .Timeline}}<li class="t-{{.Type}}">
<time title="{{stamp .Time}}">{{ago .Time}}</time>
<a href="{{deployURL .Agent .Deploy}}">{{.Deploy}}</a> on {{.Agent}} {{eventText .Type}}
{{if .State}}{{if eq .Type "deploy.finished"}}{{template "badge" .State}}{{end}}{{end}}
{{if .Reason}}<span class="muted">— {{.Reason}}</span>{{end}}
</li>{{end}}</ol>{{end}}
{{template "foot" .}}{{end}}

{{define "agents"}}{{template "head" .}}
<h2>Agents</h2>
{{if not .Agents}}<p class="empty">No agents. Create one with <code>nimdeploy hub agent add &lt;name&gt;</code>.</p>{{else}}
<div class="scroll"><table class="list">
<thead><tr><th>Agent</th><th>State</th><th>Host</th><th>Version</th><th>Last seen</th><th>Up since</th><th>Deploys</th><th>Outbox</th><th>Labels</th></tr></thead>
<tbody>{{range .Agents}}{{$s := .StateText}}<tr>
<td><a href="/?agent={{.Name}}">{{.Name}}</a></td>
<td><span class="badge a-{{$s}}">{{$s}}</span></td>
<td>{{.Host}}</td><td>{{.Version}}</td>
<td title="{{stamp .LastSeen}}">{{ago .LastSeen}}</td>
<td title="{{stamp .StartedAt}}">{{ago .StartedAt}}</td>
<td>{{index $.Count .Name}}</td>
<td>{{if .Outbox}}<b class="warn">{{.Outbox}}</b>{{else}}0{{end}}</td>
<td class="muted">{{labels .Labels}}</td>
</tr>{{end}}</tbody></table></div>{{end}}
{{template "foot" .}}{{end}}

{{define "deploy"}}{{template "head" .}}
{{with .D}}
<p class="crumbs"><a href="/">Deploys</a> / <a href="/?agent={{.Agent}}">{{.Agent}}</a></p>
<h1>{{.Title}} {{template "badge" .State}}</h1>
<dl class="facts">
<dt>Agent</dt><dd><span class="dot{{if .AgentOnline}} up{{end}}"></span>{{.Agent}}{{if not .AgentOnline}} <span class="muted">(offline: the state may be old)</span>{{end}}</dd>
{{if .Repository}}<dt>Repository</dt><dd>{{.Repository}}{{if .Branch}} · {{.Branch}}{{end}}</dd>{{end}}
{{if .Provider}}<dt>Provider</dt><dd>{{.Provider}}</dd>{{end}}
{{if .Schedule}}<dt>Schedule</dt><dd><code>{{.Schedule}}</code></dd>{{end}}
{{if .State.Commit}}<dt>Commit</dt><dd><code>{{.State.Commit}}</code>{{if .State.Pusher}} by {{.State.Pusher}}{{end}}</dd>{{end}}
{{if .State.Trigger}}<dt>Trigger</dt><dd>{{.State.Trigger}}</dd>{{end}}
{{if .State.FinishedAt}}<dt>Finished</dt><dd title="{{stamp .State.FinishedAt}}">{{ago .State.FinishedAt}}{{if .State.Duration}}, took {{.State.Duration}}{{end}}</dd>
{{else if .State.StartedAt}}<dt>Started</dt><dd title="{{stamp .State.StartedAt}}">{{ago .State.StartedAt}}</dd>{{end}}
{{if .State.Error}}<dt>Error</dt><dd class="error">{{.State.Error}}</dd>{{end}}
{{if .Labels}}<dt>Labels</dt><dd>{{range $k, $v := .Labels}}<span class="label">{{$k}}={{$v}}</span> {{end}}</dd>{{end}}
</dl>
{{end}}
<h2>History</h2>
{{if not .Events}}<p class="empty">No events yet.</p>{{end}}
<ol class="timeline">{{range .Events}}<li class="t-{{.Type}}">
<time title="{{stamp .Time}}">{{ago .Time}}</time> {{eventText .Type}}
{{if .State}}{{if eq .Type "deploy.finished"}}{{template "badge" .State}}{{end}}
{{with .State}}{{if .Commit}}<code>{{short .Commit}}</code>{{else if ref .}}<code>{{ref .}}</code>{{end}}{{if .Duration}} <span class="muted">{{.Duration}}</span>{{end}}{{end}}{{end}}
{{if .Reason}}<span class="muted">— {{.Reason}}</span>{{end}}
{{if .LogTail}}<details><summary>Log (last {{len .LogTail}} lines)</summary><pre>{{lines .LogTail}}</pre></details>{{end}}
</li>{{end}}</ol>
{{template "foot" .}}{{end}}
`

const hubCSS = `:root{--bg:#f6f7f9;--fg:#1c2230;--muted:#6b7280;--card:#fff;--line:#e3e6eb;--accent:#2f6fed;
--ok:#1f9d55;--okbg:#e3f6ea;--bad:#d64545;--badbg:#fde8e8;--busy:#2f6fed;--busybg:#e5edfd;--warn:#b7791f;--warnbg:#fdf3e1;--off:#8a94a6;--offbg:#eef0f3}
@media (prefers-color-scheme:dark){:root{--bg:#11141a;--fg:#e6e9ef;--muted:#9aa3b2;--card:#191d25;--line:#2a303b;--accent:#6c9cff;
--ok:#4cc38a;--okbg:#173327;--bad:#f07171;--badbg:#3a1d1f;--busy:#6c9cff;--busybg:#1c2a46;--warn:#e5b45a;--warnbg:#3a2e17;--off:#8a94a6;--offbg:#242a34}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
code,pre{font:12px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.top{display:flex;align-items:center;gap:24px;padding:12px 16px;background:var(--card);border-bottom:1px solid var(--line);flex-wrap:wrap}
.brand{font-weight:700;color:var(--fg);font-size:16px}.brand span{color:var(--accent)}
nav{display:flex;gap:16px}nav a{color:var(--muted)}nav a.on{color:var(--fg);font-weight:600}
.who{margin-left:auto;display:flex;gap:12px;align-items:center;color:var(--muted)}
.who form{margin:0}button.link{background:none;border:0;color:var(--accent);padding:0;font:inherit;cursor:pointer}
main{max-width:1200px;margin:0 auto;padding:16px}
footer{max-width:1200px;margin:0 auto;padding:16px;color:var(--muted);font-size:12px}
h1{font-size:20px;margin:8px 0 16px;display:flex;gap:10px;align-items:center;flex-wrap:wrap}
h2{font-size:15px;margin:28px 0 10px}
.muted{color:var(--muted)}.error{color:var(--bad)}.warn{color:var(--warn)}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:10px}
.stat{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:12px;color:var(--fg)}
.stat:hover{text-decoration:none;border-color:var(--accent)}
.stat b{display:block;font-size:24px}.stat span{color:var(--muted)}
.stat.bad b{color:var(--bad)}.stat.busy b{color:var(--busy)}
.filters{display:flex;gap:12px;align-items:end;flex-wrap:wrap;margin:16px 0 0}
.filters label{display:flex;flex-direction:column;font-size:12px;color:var(--muted);gap:2px}
select,input,button{font:inherit;color:var(--fg);background:var(--card);border:1px solid var(--line);border-radius:6px;padding:6px 8px}
button{cursor:pointer}button:hover{border-color:var(--accent)}
.scroll{overflow-x:auto;background:var(--card);border:1px solid var(--line);border-radius:8px}
table{border-collapse:collapse;width:100%}
th,td{padding:8px 10px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top;white-space:nowrap}
tbody tr:last-child th,tbody tr:last-child td{border-bottom:0}
thead th{font-size:12px;color:var(--muted);font-weight:600}
.matrix td{white-space:normal;min-width:140px}
.chip{display:inline-block;margin:2px 4px 2px 0;padding:2px 8px;border-radius:999px;font-size:12px;border:1px solid transparent}
.chip:hover{text-decoration:none;border-color:currentColor}
.chip.stale{opacity:.55;border-style:dashed;border-color:currentColor}
.badge{display:inline-block;padding:1px 8px;border-radius:999px;font-size:12px;font-weight:600}
.s-success{color:var(--ok);background:var(--okbg)}.s-failed{color:var(--bad);background:var(--badbg)}
.s-running,.s-waiting{color:var(--busy);background:var(--busybg)}
.s-skipped,.s-interrupted{color:var(--warn);background:var(--warnbg)}.s-never{color:var(--off);background:var(--offbg)}
.a-online{color:var(--ok);background:var(--okbg)}.a-offline{color:var(--bad);background:var(--badbg)}
.a-revoked,.a-never.seen{color:var(--off);background:var(--offbg)}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--bad);margin-right:6px}.dot.up{background:var(--ok)}
.timeline{list-style:none;margin:0;padding:0;background:var(--card);border:1px solid var(--line);border-radius:8px}
.timeline li{padding:8px 12px;border-bottom:1px solid var(--line)}.timeline li:last-child{border-bottom:0}
.timeline time{display:inline-block;min-width:70px;color:var(--muted)}
.timeline .t-deploy\.rejected{background:var(--warnbg)}
details{margin-top:6px}summary{cursor:pointer;color:var(--muted)}
pre{background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:10px;overflow-x:auto;max-height:420px;white-space:pre-wrap;word-break:break-all}
.facts{display:grid;grid-template-columns:max-content 1fr;gap:6px 16px;background:var(--card);border:1px solid var(--line);border-radius:8px;padding:14px;margin:0}
.facts dt{color:var(--muted)}.facts dd{margin:0;word-break:break-word}
.label{display:inline-block;background:var(--offbg);border-radius:4px;padding:0 6px;margin:1px 0;font-size:12px}
.crumbs{color:var(--muted);margin:0}
.empty{background:var(--card);border:1px dashed var(--line);border-radius:8px;padding:20px;color:var(--muted)}
.login{max-width:340px;margin:60px auto;display:flex;flex-direction:column;gap:10px;background:var(--card);border:1px solid var(--line);border-radius:10px;padding:24px}
.login h1{margin:0}
@media (max-width:640px){.top{gap:12px}.who{margin-left:0}.facts{grid-template-columns:1fr}.facts dt{margin-top:6px}}
`
