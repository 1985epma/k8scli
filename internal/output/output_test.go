package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	for _, value := range []string{"table", "wide", "json", "yaml"} {
		if _, err := ParseFormat(value); err != nil {
			t.Errorf("ParseFormat(%q): %v", value, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("ParseFormat(xml) should fail")
	}
}

func TestStructured(t *testing.T) {
	if Table.Structured() || Wide.Structured() || !JSON.Structured() || !YAML.Structured() {
		t.Error("Structured() reports the wrong formats")
	}
}

func TestWriteStructured(t *testing.T) {
	value := map[string]any{"name": "web", "replicas": 3}

	var jsonOut bytes.Buffer
	if err := WriteStructured(&jsonOut, JSON, value); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut.String(), `"replicas": 3`) {
		t.Errorf("json = %s", jsonOut.String())
	}

	var yamlOut bytes.Buffer
	if err := WriteStructured(&yamlOut, YAML, value); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yamlOut.String(), "replicas: 3") {
		t.Errorf("yaml = %s", yamlOut.String())
	}

	if err := WriteStructured(&yamlOut, Table, value); err == nil {
		t.Error("table is not a structured format")
	}
}
