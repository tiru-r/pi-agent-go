package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// nativeDescriptor is the JSON schema for a native (subprocess) extension.
// A .json file in the extensions directory is treated as a native extension when
// it contains a "command" field and has no sibling .js file with the same base name.
//
// The subprocess receives tool parameters as JSON on stdin and must write
// {"content":"...","is_error":false} as JSON to stdout, or plain text on stdout.
type nativeDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Command     []string        `json:"command"`
}

type nativeExtension struct {
	info Info
	cmd  []string
}

func loadNative(path string) (*nativeExtension, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var desc nativeDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(desc.Command) == 0 {
		return nil, fmt.Errorf("missing command")
	}
	if desc.Name == "" {
		return nil, fmt.Errorf("missing name")
	}
	schema := desc.Schema
	if schema == nil {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return &nativeExtension{
		info: Info{Name: desc.Name, Description: desc.Description, Schema: schema},
		cmd:  desc.Command,
	}, nil
}

func (e *nativeExtension) Info() Info   { return e.info }
func (e *nativeExtension) Close() error { return nil }

func (e *nativeExtension) Execute(ctx context.Context, params json.RawMessage) (string, bool, error) {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	if params == nil {
		params = json.RawMessage("{}")
	}

	cmd := exec.CommandContext(tctx, e.cmd[0], e.cmd[1:]...) //nolint:gosec
	cmd.Stdin = bytes.NewReader(params)

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return string(ee.Stderr), true, nil
		}
		return "", true, err
	}

	var resp struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return string(out), false, nil
	}
	return resp.Content, resp.IsError, nil
}
