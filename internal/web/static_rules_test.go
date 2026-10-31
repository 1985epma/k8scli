package web

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The console is served with a strict Content-Security-Policy (no inline styles or scripts, nothing
// from other origins). These tests read the embedded files so a regression fails here, not in a
// browser nobody opened.

var (
	inlineStyle   = regexp.MustCompile(`(?i)\sstyle\s*=\s*["'\x60]`)
	inlineHandler = regexp.MustCompile(`(?i)\son(click|change|input|submit|load|error|focus|blur|key\w+|mouse\w+|pointer\w+|touch\w+)\s*=`)
	externalLoad  = regexp.MustCompile(`(?i)(<script[^>]+src\s*=\s*["']https?:|<link[^>]+href\s*=\s*["']https?:|@import|url\(\s*["']?https?:|\bfetch\(\s*["'\x60]https?:|\bimport\(\s*["']https?:|from\s+["']https?:)`)
	dynamicCode   = regexp.MustCompile(`\beval\(|new\s+Function\(|document\.write\(`)
)

func staticSources(t *testing.T) map[string]string {
	t.Helper()
	sources := map[string]string{}
	err := fs.WalkDir(staticFiles, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch path.Ext(p) {
		case ".js", ".css", ".html":
			raw, readErr := staticFiles.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			sources[p] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) < 10 {
		t.Fatalf("only %d static files found; is the embed broken?", len(sources))
	}
	return sources
}

func TestStaticFilesRespectTheContentSecurityPolicy(t *testing.T) {
	for name, source := range staticSources(t) {
		if path.Ext(name) != ".css" && inlineStyle.MatchString(source) {
			t.Errorf("%s: inline style attribute (CSP style-src 'self' blocks it): %s", name, inlineStyle.FindString(source))
		}
		if path.Ext(name) != ".css" && inlineHandler.MatchString(source) {
			t.Errorf("%s: inline event handler (CSP script-src 'self' blocks it): %s", name, inlineHandler.FindString(source))
		}
		if externalLoad.MatchString(source) {
			t.Errorf("%s: loads something from another origin: %s", name, externalLoad.FindString(source))
		}
		if dynamicCode.MatchString(source) {
			t.Errorf("%s: dynamic code evaluation: %s", name, dynamicCode.FindString(source))
		}
	}
}

func TestStaticIndexIsSelfContained(t *testing.T) {
	index := staticSources(t)["static/index.html"]
	if strings.Contains(index, "<script>") {
		t.Error("index.html has an inline script")
	}
	for _, want := range []string{`rel="stylesheet" href="/app.css"`, `type="module" src="/app.js"`} {
		if !strings.Contains(index, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
}

// The UI is English only; a Portuguese word in a string means a translated draft slipped in.
func TestStaticFilesAreEnglish(t *testing.T) {
	portuguese := regexp.MustCompile(`(?i)\b(não|nao|você|voce|salvar|criar|escalar|carregando|configuração|erro ao)\b`)
	for name, source := range staticSources(t) {
		if hit := portuguese.FindString(source); hit != "" {
			t.Errorf("%s: Portuguese text %q", name, hit)
		}
	}
}

// A guard that never fires is worse than none: prove each rule catches what it is meant to.
func TestStaticRulesCatchViolations(t *testing.T) {
	for _, tc := range []struct {
		rule *regexp.Regexp
		bad  string
		ok   string
	}{
		{inlineStyle, `<div style="color:red">`, `el.style.left = '4px'`},
		{inlineHandler, `<button onclick="go()">`, `registerPage('x', { onClick: handler })`},
		{externalLoad, `<script src="https://cdn.example.com/a.js">`, `<script type="module" src="/app.js">`},
		{externalLoad, `fetch('https://api.example.com')`, `fetch('/api/state')`},
		{dynamicCode, `eval(code)`, `evaluate(code)`},
	} {
		if !tc.rule.MatchString(tc.bad) {
			t.Errorf("rule %v does not catch %q", tc.rule, tc.bad)
		}
		if tc.rule.MatchString(tc.ok) {
			t.Errorf("rule %v wrongly flags %q", tc.rule, tc.ok)
		}
	}
}
