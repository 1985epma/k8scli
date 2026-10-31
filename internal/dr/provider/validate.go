package provider

import (
	"fmt"
	"regexp"
	"sort"
)

// ident is the only shape accepted for any value that ends up in a command line: ASCII letters,
// digits, dot, underscore and dash, starting with a letter or digit. That rules out a leading '-'
// (an option), whitespace and line breaks, '=' and ';' (option values and shell syntax), and every
// non-ASCII lookalike. Plan parameters were validated once when the plan was loaded; this is the
// second, independent check right before use.
var ident = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validIdent returns the value when it is safe, or an error naming the parameter.
func validIdent(param, value string) (string, error) {
	if !ident.MatchString(value) {
		return "", fmt.Errorf("parameter %q: %q is not a valid name (ASCII letters, digits, '.', '_' and '-' only, starting with a letter or digit, at most 128 characters)", param, value)
	}
	return value, nil
}

// params reads and validates the parameters of one provider kind.
type params struct {
	kind string
	raw  map[string]string
	err  error
}

func newParams(kind string, raw map[string]string) *params { return &params{kind: kind, raw: raw} }

// required returns a validated, mandatory parameter.
func (p *params) required(name string) string {
	if p.err != nil {
		return ""
	}
	value, ok := p.raw[name]
	if !ok || value == "" {
		p.err = fmt.Errorf("provider %s: parameter %q is required", p.kind, name)
		return ""
	}
	return p.valid(name, value)
}

// optional returns a validated parameter or "" when it is absent.
func (p *params) optional(name string) string {
	if p.err != nil {
		return ""
	}
	value := p.raw[name]
	if value == "" {
		return ""
	}
	return p.valid(name, value)
}

func (p *params) valid(name, value string) string {
	checked, err := validIdent(name, value)
	if err != nil {
		p.err = fmt.Errorf("provider %s: %w", p.kind, err)
		return ""
	}
	return checked
}

// rejectUnknown fails on parameters the provider does not understand, so a typo is not silently ignored.
func (p *params) rejectUnknown(known ...string) {
	if p.err != nil {
		return
	}
	allowed := map[string]bool{}
	for _, name := range known {
		allowed[name] = true
	}
	var extra []string
	for name := range p.raw {
		if !allowed[name] {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		p.err = fmt.Errorf("provider %s: unknown parameter %q (known: %v)", p.kind, extra[0], known)
	}
}
