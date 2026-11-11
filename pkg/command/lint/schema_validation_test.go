package lint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.stackrox.io/kube-linter/pkg/run"
)

const schemaValidationConfig = `checks:
  doNotAutoAddDefaults: true
  include:
    - schema-validation
`

const invalidEnvPodTemplate = `apiVersion: v1
kind: Pod
metadata:
  name: invalid-env
spec:
  containers:
    - name: app
      image: busybox:1.36
      env:
        - name: ENABLED
          value: %s
`

// This object exercises the mixed valid/invalid input path without downloading
// a schema. Loading failures must still be reported when another object loads.
const validConfigMapWithIgnoredSchema = `apiVersion: v1
kind: ConfigMap
metadata:
  name: valid-config
  annotations:
    ignore-check.kube-linter.io/schema-validation: "test fixture"
data:
  key: value
---
`

func TestCommand_SchemaValidationLoadFailures(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		prefix     string
		fromStdin  bool
		wantDecode string
	}{
		{
			name:       "boolean value",
			value:      "true",
			wantDecode: "cannot unmarshal bool",
		},
		{
			name:       "numeric value",
			value:      "123",
			wantDecode: "cannot unmarshal number",
		},
		{
			name:       "valid object does not hide invalid object",
			value:      "true",
			prefix:     validConfigMapWithIgnoredSchema,
			wantDecode: "cannot unmarshal bool",
		},
		{
			name:       "stdin",
			value:      "true",
			fromStdin:  true,
			wantDecode: "cannot unmarshal bool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := tt.prefix + fmt.Sprintf(invalidEnvPodTemplate, tt.value)
			output, err := runSchemaValidationLint(t, manifest, schemaValidationConfig, tt.fromStdin)
			require.ErrorContains(t, err, "found 1 lint errors")

			var result run.Result
			require.NoError(t, json.Unmarshal(output, &result))
			require.Len(t, result.Reports, 1)
			assert.Equal(t, "failed-to-load-object", result.Reports[0].Check)
			assert.Contains(t, result.Reports[0].Diagnostic.Message, tt.wantDecode)
			assert.Equal(t, run.ChecksFailed, result.Summary.ChecksStatus)
		})
	}
}

func TestCommand_SchemaValidationLoadFailurePolicy(t *testing.T) {
	const duplicateEnvConfig = `checks:
  doNotAutoAddDefaults: true
  include:
    - duplicate-env-var
`
	const excludedSchemaConfig = `checks:
  doNotAutoAddDefaults: true
  include:
    - duplicate-env-var
    - schema-validation
  exclude:
    - schema-validation
`

	// A custom validator scoped to Services must not make loading failures in
	// unrelated kinds fatal. Neither input document requires a schema download.
	const customSchemaConfig = `customChecks:
  - name: service-schema
    template: kubeconform
    scope:
      objectKinds:
        - Service
checks:
  doNotAutoAddDefaults: true
`

	tests := []struct {
		name        string
		config      string
		args        []string
		wantReports int
	}{
		{
			name:        "schema check and flag report once",
			config:      schemaValidationConfig,
			args:        []string{"--fail-on-invalid-resource"},
			wantReports: 1,
		},
		{
			name:   "schema check disabled",
			config: duplicateEnvConfig,
		},
		{
			name:   "schema check explicitly excluded",
			config: excludedSchemaConfig,
		},
		{
			name:        "legacy flag without schema check",
			config:      duplicateEnvConfig,
			args:        []string{"--fail-on-invalid-resource"},
			wantReports: 1,
		},
		{
			name:   "custom schema check retains its scope",
			config: customSchemaConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validConfigMapWithIgnoredSchema + fmt.Sprintf(invalidEnvPodTemplate, "true")
			output, err := runSchemaValidationLint(t, manifest, tt.config, false, tt.args...)
			wantStatus := run.ChecksPassed
			if tt.wantReports > 0 {
				require.ErrorContains(t, err, "found 1 lint errors")
				wantStatus = run.ChecksFailed
			} else {
				require.NoError(t, err)
			}

			var result run.Result
			require.NoError(t, json.Unmarshal(output, &result))
			assert.Len(t, result.Reports, tt.wantReports)
			assert.Equal(t, wantStatus, result.Summary.ChecksStatus)
		})
	}
}

func TestCommand_SchemaValidationEmptyDocuments(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		args     []string
		wantErr  string
	}{
		{name: "empty input"},
		{name: "comment only", manifest: "# disabled resource\n"},
		{name: "empty documents", manifest: "---\n# first\n---\n# second\n"},
		{
			name:    "explicitly require objects",
			args:    []string{"--fail-if-no-objects-found"},
			wantErr: "no valid objects found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runSchemaValidationLint(t, tt.manifest, schemaValidationConfig, false, tt.args...)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func runSchemaValidationLint(t *testing.T, manifest, config string, fromStdin bool, extraArgs ...string) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	configPath := filepath.Join(dir, "config.yaml")
	outputPath := filepath.Join(dir, "result.json")
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0600))
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0600))

	input := manifestPath
	if fromStdin {
		// The loader reads os.Stdin directly. These tests must not run in parallel.
		stdin, err := os.Open(manifestPath)
		require.NoError(t, err)
		originalStdin := os.Stdin
		os.Stdin = stdin
		t.Cleanup(func() {
			os.Stdin = originalStdin
			require.NoError(t, stdin.Close())
		})
		input = "-"
	}

	args := []string{"--config", configPath, "--format", "json", "--output", outputPath}
	args = append(args, extraArgs...)
	args = append(args, input)
	err := createLintCommand(args...).Execute()
	output, readErr := os.ReadFile(outputPath)
	if !errors.Is(readErr, os.ErrNotExist) {
		require.NoError(t, readErr)
	}
	return output, err
}
