package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// exitError makes main exit with a specific code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func callCmd(flags *target) *cobra.Command {
	var argsFlag string
	cmd := &cobra.Command{
		Use:   "call <tool-name> " + targetUsage,
		Short: "Call a tool with JSON arguments and print the raw result",
		Long: `Call a tool with JSON arguments and print the result as the server sent it.

Arguments (--args):
  '{"key": "value"}'   inline JSON object
  @file.json           read from a file
  -                    read from stdin
  (omitted)            {}

Exit codes: 0 success, 1 protocol or connection error, 2 the tool reported
an error (isError: true; the result is still printed).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			toolArgs, err := readToolArgs(argsFlag)
			if err != nil {
				return err
			}
			rest, dash, err := targetArgs(cmd, args, 1)
			if err != nil {
				return err
			}
			t, authPath, err := resolveTarget(cmd, rest, dash, flags)
			if err != nil {
				return err
			}
			return withSession(t, authPath, func(ctx context.Context, session *mcp.ClientSession, raw *rawResults) error {
				params := &mcp.CallToolParams{Name: name, Arguments: toolArgs}
				params.SetProgressToken("mcp-inspect")
				res, err := session.CallTool(ctx, params)
				if err != nil {
					return err
				}
				results := raw.get("tools/call")
				if len(results) == 0 {
					return errors.New("no raw tools/call result recorded")
				}
				// With multi round-trip requests, earlier results were intermediate.
				var buf bytes.Buffer
				if err := json.Indent(&buf, results[len(results)-1], "", "  "); err != nil {
					return err
				}
				fmt.Println(buf.String())
				if res.IsError {
					return &exitError{code: 2, msg: "tool reported an error (isError: true)"}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&argsFlag, "args", "", "tool arguments: a JSON object, @file, or - for stdin")
	return cmd
}

// readToolArgs resolves --args into a JSON object.
func readToolArgs(flag string) (json.RawMessage, error) {
	var data []byte
	var err error
	switch {
	case flag == "":
		return json.RawMessage("{}"), nil
	case flag == "-":
		data, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(flag, "@"):
		data, err = os.ReadFile(flag[1:])
	default:
		data = []byte(flag)
	}
	if err != nil {
		return nil, fmt.Errorf("reading --args: %w", err)
	}
	data = bytes.TrimSpace(data)
	if !json.Valid(data) {
		return nil, errors.New("--args is not valid JSON")
	}
	if len(data) == 0 || data[0] != '{' {
		return nil, errors.New("--args must be a JSON object")
	}
	return data, nil
}
