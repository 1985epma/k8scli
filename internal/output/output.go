// Package output centralises how commands render results (table, wide, json, yaml).
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// Format is a supported output format.
type Format string

const (
	Table Format = "table"
	Wide  Format = "wide"
	JSON  Format = "json"
	YAML  Format = "yaml"
)

// ParseFormat validates the value given to --output.
func ParseFormat(value string) (Format, error) {
	switch format := Format(value); format {
	case Table, Wide, JSON, YAML:
		return format, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (use: table, wide, json, yaml)", value)
	}
}

// Structured reports whether the format is machine readable.
func (f Format) Structured() bool {
	return f == JSON || f == YAML
}

// WriteStructured renders v as indented JSON or YAML.
func WriteStructured(w io.Writer, format Format, v any) error {
	switch format {
	case JSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(v)
	case YAML:
		data, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("failed to encode yaml: %w", err)
		}
		_, err = w.Write(data)
		return err
	default:
		return fmt.Errorf("format %q is not structured", format)
	}
}

// NewTable returns a tabwriter configured like kubectl's tables.
// Callers must Flush it.
func NewTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
}
