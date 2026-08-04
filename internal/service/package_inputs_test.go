package service

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInputsOfReadsAPackageDeclaration(t *testing.T) {
	var doc map[string]any
	raw := `
inputs:
  - interface: s3
    namedConnection:
      name: "{{ .Parameters.s3Connection }}"
    alias: storage
    optional: true
  - interface: postgresql
    namedConnection:
      name: "{{ .Parameters.pgConnection }}"
  - interface: trino
    release:
      name: other
`
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}

	inputs := inputsOf(doc)
	if len(inputs) != 3 {
		t.Fatalf("got %d inputs, want 3", len(inputs))
	}
	if inputs[0].Alias != "storage" || inputs[0].Parameter != "s3Connection" || !inputs[0].Optional {
		t.Errorf("input 0 = %+v", inputs[0])
	}
	// Alias defaults to the interface name.
	if inputs[1].Alias != "postgresql" || inputs[1].Parameter != "pgConnection" {
		t.Errorf("input 1 = %+v", inputs[1])
	}
	// Bound to a release output: the user has no choice to make, so no parameter.
	if inputs[2].Parameter != "" {
		t.Errorf("input 2 should offer no choice: %+v", inputs[2])
	}
}

func TestInputsOfToleratesAPackageWithout(t *testing.T) {
	if got := inputsOf(map[string]any{"schema": map[string]any{}}); got != nil {
		t.Errorf("got %v, want nil — no inputs is the normal case", got)
	}
}
