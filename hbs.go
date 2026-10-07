package main

import (
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
)

// Handlebars templates (files ending in .hbs) for emails. Instead of a
// Handlebars engine, the common subset is translated into Go templates, so
// HTML gets html/template's context-aware escaping and no dependency is
// needed. Supported:
//
//	{{path.to.value}}  {{list.0.name}}  {{{raw}}}  {{this}}  {{@index}} {{@key}}
//	{{#if x}} {{else}} {{/if}}   {{#unless x}} {{/unless}}
//	{{#each list}} {{/each}}     {{#with obj}} {{/with}}
//	{{! comment }} {{!-- comment --}}   {{~ trims whitespace ~}}
//	../x and @root.x refer to the top level
//
// Like Handlebars, a missing value renders as nothing instead of failing.

var hbsPathRe = regexp.MustCompile(`^(@root\.|\.\./)?[A-Za-z_@][A-Za-z0-9_\-]*(\.(\[?[A-Za-z0-9_\-]+\]?))*$`)

// translateHandlebars turns Handlebars source into a Go template.
func translateHandlebars(src string) (string, error) {
	var out strings.Builder
	var stack []string
	eachDepth := 0
	rest := src
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:i])
		rest = rest[i:]

		var inner string
		raw := false
		switch {
		case strings.HasPrefix(rest, "{{!--"):
			end := strings.Index(rest, "--}}")
			if end < 0 {
				return "", fmt.Errorf("unterminated {{!-- comment")
			}
			rest = rest[end+4:]
			continue
		case strings.HasPrefix(rest, "{{!"):
			end := strings.Index(rest, "}}")
			if end < 0 {
				return "", fmt.Errorf("unterminated {{! comment")
			}
			rest = rest[end+2:]
			continue
		case strings.HasPrefix(rest, "{{{"):
			end := strings.Index(rest, "}}}")
			if end < 0 {
				return "", fmt.Errorf("unterminated {{{")
			}
			inner, raw = rest[3:end], true
			rest = rest[end+3:]
		default:
			end := strings.Index(rest, "}}")
			if end < 0 {
				return "", fmt.Errorf("unterminated {{")
			}
			inner = rest[2:end]
			rest = rest[end+2:]
		}

		open, close := "{{", "}}"
		if strings.HasPrefix(inner, "~") {
			open, inner = "{{- ", inner[1:]
		}
		if strings.HasSuffix(inner, "~") {
			close, inner = " -}}", inner[:len(inner)-1]
		}
		expr := strings.TrimSpace(inner)
		emit := func(action string) { out.WriteString(open + " " + action + " " + close) }

		if raw {
			ref, err := hbsRef(expr, eachDepth)
			if err != nil {
				return "", err
			}
			emit("raw " + ref)
			continue
		}
		switch {
		case strings.HasPrefix(expr, "#"):
			name, arg, _ := strings.Cut(expr[1:], " ")
			arg = strings.TrimSpace(arg)
			if arg == "" {
				return "", fmt.Errorf("{{#%s}} needs an argument", name)
			}
			ref, err := hbsRef(arg, eachDepth)
			if err != nil {
				return "", err
			}
			switch name {
			case "if":
				emit("if truthy " + ref)
			case "unless":
				emit("if not (truthy " + ref + ")")
			case "each":
				emit(fmt.Sprintf("range $i%d, $e%d := iter %s", eachDepth+1, eachDepth+1, ref))
				eachDepth++
			case "with":
				emit("with " + ref)
			default:
				return "", fmt.Errorf("block helper {{#%s}} is not supported (use #if, #unless, #each, #with)", name)
			}
			stack = append(stack, name)
		case strings.HasPrefix(expr, "/"):
			name := strings.TrimSpace(expr[1:])
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return "", fmt.Errorf("{{/%s}} does not close the open block (%s)", name, strings.Join(stack, " > "))
			}
			if name == "each" {
				eachDepth--
			}
			stack = stack[:len(stack)-1]
			emit("end")
		case expr == "else":
			if len(stack) == 0 {
				return "", fmt.Errorf("{{else}} outside a block")
			}
			emit("else")
		default:
			ref, err := hbsRef(expr, eachDepth)
			if err != nil {
				return "", err
			}
			emit("str " + ref)
		}
	}
	if len(stack) > 0 {
		return "", fmt.Errorf("unclosed {{#%s}}", stack[len(stack)-1])
	}
	return out.String(), nil
}

// hbsRef turns a Handlebars reference into a Go template expression.
func hbsRef(expr string, eachDepth int) (string, error) {
	switch expr {
	case "this", ".":
		return ".", nil
	case "@index", "@key":
		if eachDepth == 0 {
			return "", fmt.Errorf("%s outside {{#each}}", expr)
		}
		return fmt.Sprintf("$i%d", eachDepth), nil
	}
	if strings.ContainsAny(expr, " \"'()") {
		return "", fmt.Errorf("{{%s}}: helpers and arguments are not supported, only paths", expr)
	}
	if !hbsPathRe.MatchString(expr) {
		return "", fmt.Errorf("{{%s}} is not a valid path", expr)
	}
	ctx := "."
	if p, ok := strings.CutPrefix(expr, "@root."); ok {
		ctx, expr = "$", p
	} else if p, ok := strings.CutPrefix(expr, "../"); ok {
		ctx, expr = "$", p
	} else if p, ok := strings.CutPrefix(expr, "this."); ok {
		expr = p
	}
	return fmt.Sprintf("(get %s %s)", ctx, strconv.Quote(expr)), nil
}

// hbsFuncs back the translated templates; html selects the raw type.
func hbsFuncs(html bool) map[string]any {
	return map[string]any{
		"get":    hbsGet,
		"str":    hbsString,
		"truthy": hbsTruthy,
		"iter":   hbsIter,
		"raw": func(v any) any {
			if html {
				return htmltemplate.HTML(hbsString(v))
			}
			return hbsString(v)
		},
	}
}

// hbsGet walks a dotted path through maps and slices; missing → nil.
func hbsGet(ctx any, path string) any {
	cur := ctx
	for _, part := range strings.Split(path, ".") {
		part = strings.Trim(part, "[]")
		switch c := cur.(type) {
		case map[string]any:
			cur = c[part]
		case map[string]string:
			v, ok := c[part]
			if !ok {
				return nil
			}
			cur = v
		case []any:
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n >= len(c) {
				return nil
			}
			cur = c[n]
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

func hbsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case map[string]any, []any, map[string]string:
		return "" // like Handlebars' [object Object], but quieter
	}
	return fmt.Sprint(v)
}

// hbsTruthy follows Handlebars: false, empty, 0, nil and [] are false.
func hbsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return true
	case map[string]string:
		return true
	}
	return true
}

// hbsIter makes lists and objects rangeable (objects by sorted key).
func hbsIter(v any) any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		return x
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m := make(map[string]any, len(x))
		for _, k := range keys {
			m[k] = x[k]
		}
		return m
	}
	return []any{}
}

// parseHandlebars compiles a .hbs template for HTML or plain text.
func parseHandlebars(name, src string, html bool) (templateExecutor, error) {
	goSrc, err := translateHandlebars(src)
	if err != nil {
		return nil, err
	}
	if html {
		return htmltemplate.New(name).Funcs(hbsFuncs(true)).Parse(goSrc)
	}
	return texttemplate.New(name).Funcs(hbsFuncs(false)).Parse(goSrc)
}

// toHandlebarsData is the context .hbs templates see: plain maps, so every
// path can be walked and missing ones render empty.
func (d mailData) toHandlebarsData() map[string]any {
	toAny := func(m map[string]string) map[string]any {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return map[string]any{
		"Deploy": d.Deploy, "Status": d.Status, "Trigger": d.Trigger, "Event": d.Event,
		"ResourceID": d.ResourceID, "Commit": d.Commit, "Host": d.Host,
		"Output": toAny(d.Output), "Links": toAny(d.Links), "Params": toAny(d.Params),
		"Payload": d.Payload,
		"Labels":  toAny(d.Labels),
	}
}
