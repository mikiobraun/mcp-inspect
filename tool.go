package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func toolCmd(flags *target) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tool <tool-name> " + targetUsage,
		Short: "Show a tool's description, parameters and result schema",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			rest, dash, err := targetArgs(cmd, args, 1)
			if err != nil {
				return err
			}
			t, authPath, err := resolveTarget(cmd, rest, dash, flags)
			if err != nil {
				return err
			}
			return withSession(t, authPath, func(ctx context.Context, session *mcp.ClientSession, raw *rawResults) error {
				// Page through the tool list; the raw pages are recorded by the transport.
				for _, err := range session.Tools(ctx, nil) {
					if err != nil {
						return fmt.Errorf("listing tools: %w", err)
					}
				}
				data, err := findRawTool(raw.get("tools/list"), name)
				if err != nil {
					return err
				}
				if asJSON {
					var buf bytes.Buffer
					if err := json.Indent(&buf, data, "", "  "); err != nil {
						return err
					}
					fmt.Println(buf.String())
					return nil
				}
				var def toolDef
				if err := json.Unmarshal(data, &def); err != nil {
					return fmt.Errorf("parsing tool %q: %w", name, err)
				}
				printTool(&def)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the tool definition as the server sent it")
	return cmd
}

// findRawTool returns the tool definition named name from raw tools/list results.
func findRawTool(pages []json.RawMessage, name string) (json.RawMessage, error) {
	var names []string
	for _, p := range pages {
		var page struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(p, &page); err != nil {
			return nil, fmt.Errorf("parsing tools/list result: %w", err)
		}
		for _, raw := range page.Tools {
			var head struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				return nil, fmt.Errorf("parsing tools/list result: %w", err)
			}
			if head.Name == name {
				return raw, nil
			}
			names = append(names, head.Name)
		}
	}
	return nil, fmt.Errorf("no tool %q; available: %s", name, strings.Join(names, ", "))
}

type toolDef struct {
	Name         string        `json:"name"`
	Title        string        `json:"title"`
	Description  string        `json:"description"`
	Annotations  orderedFields `json:"annotations"`
	InputSchema  *schema       `json:"inputSchema"`
	OutputSchema *schema       `json:"outputSchema"`
}

type field struct {
	Key   string
	Value json.RawMessage
}

// orderedFields is a JSON object with its keys in document order.
type orderedFields []field

func (o *orderedFields) UnmarshalJSON(data []byte) error {
	return decodeOrdered(data, func(key string, value json.RawMessage) error {
		*o = append(*o, field{key, value})
		return nil
	})
}

type namedSchema struct {
	Name   string
	Schema *schema
}

// orderedSchemas is a JSON object of schemas (properties, $defs) in document order.
type orderedSchemas []namedSchema

func (o *orderedSchemas) UnmarshalJSON(data []byte) error {
	return decodeOrdered(data, func(key string, value json.RawMessage) error {
		var s schema
		if err := json.Unmarshal(value, &s); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*o = append(*o, namedSchema{key, &s})
		return nil
	})
}

// decodeOrdered calls f for each member of a JSON object, in document order.
func decodeOrdered(data []byte, f func(key string, value json.RawMessage) error) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("expected a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if err := f(tok.(string), value); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// schema is the part of a JSON schema we render. Keywords we don't interpret
// structurally are kept as raw JSON (Combinators) or key/value details.
type schema struct {
	Types       []string
	Description string
	Default     json.RawMessage
	Enum        []json.RawMessage
	Const       json.RawMessage
	Required    []string
	Properties  orderedSchemas
	Items       *schema
	Ref         string
	Defs        orderedSchemas
	Details     []field // format, pattern, minimum, ...
	Combinators []field // oneOf, anyOf, allOf, not, ... shown as JSON
	Unparsed    json.RawMessage
}

var detailKeywords = []string{"format", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
	"multipleOf", "minLength", "maxLength", "minItems", "maxItems", "uniqueItems"}

var combinatorKeywords = []string{"oneOf", "anyOf", "allOf", "not", "if", "then", "else", "patternProperties", "prefixItems"}

func (s *schema) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// Boolean schemas and other shapes we don't interpret.
		s.Unparsed = trimmed
		return nil
	}
	var w struct {
		Type        json.RawMessage   `json:"type"`
		Description string            `json:"description"`
		Default     json.RawMessage   `json:"default"`
		Enum        []json.RawMessage `json:"enum"`
		Const       json.RawMessage   `json:"const"`
		Required    []string          `json:"required"`
		Properties  orderedSchemas    `json:"properties"`
		Items       *schema           `json:"items"`
		Ref         string            `json:"$ref"`
		Defs        orderedSchemas    `json:"$defs"`
		Definitions orderedSchemas    `json:"definitions"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*s = schema{
		Description: w.Description,
		Default:     w.Default,
		Enum:        w.Enum,
		Const:       w.Const,
		Required:    w.Required,
		Properties:  w.Properties,
		Items:       w.Items,
		Ref:         w.Ref,
		Defs:        append(w.Defs, w.Definitions...),
	}
	if len(w.Type) > 0 {
		if w.Type[0] == '"' {
			s.Types = []string{""}
			if err := json.Unmarshal(w.Type, &s.Types[0]); err != nil {
				return err
			}
		} else if err := json.Unmarshal(w.Type, &s.Types); err != nil {
			return err
		}
	}
	return decodeOrdered(data, func(key string, value json.RawMessage) error {
		switch {
		case slices.Contains(detailKeywords, key):
			s.Details = append(s.Details, field{key, value})
		case slices.Contains(combinatorKeywords, key),
			key == "additionalProperties" && bytes.HasPrefix(bytes.TrimSpace(value), []byte("{")):
			s.Combinators = append(s.Combinators, field{key, value})
		}
		return nil
	})
}

const wrapWidth = 80

func printTool(t *toolDef) {
	name := t.Name
	if t.Title != "" && t.Title != t.Name {
		name += " (" + t.Title + ")"
	}
	fmt.Println(name)
	printWrapped(t.Description, 2)

	if len(t.Annotations) > 0 {
		var parts []string
		for _, a := range t.Annotations {
			parts = append(parts, a.Key+": "+compactJSON(a.Value))
		}
		fmt.Println()
		fmt.Println("annotations:")
		printWrapped(strings.Join(parts, ", "), 2)
	}

	fmt.Println()
	fmt.Println("parameters:")
	printSchemaBody(t.InputSchema, 2)
	if t.InputSchema != nil && len(t.InputSchema.Defs) > 0 {
		fmt.Println()
		fmt.Println("parameter definitions:")
		printDefs(t.InputSchema.Defs, 2)
	}

	if t.OutputSchema != nil {
		fmt.Println()
		fmt.Println("returns:")
		printSchemaBody(t.OutputSchema, 2)
		if len(t.OutputSchema.Defs) > 0 {
			fmt.Println()
			fmt.Println("result definitions:")
			printDefs(t.OutputSchema.Defs, 2)
		}
	}
}

// printSchemaBody prints the properties of an object schema, or the schema
// itself if it isn't a plain object.
func printSchemaBody(s *schema, indent int) {
	pad := strings.Repeat(" ", indent)
	switch {
	case s == nil:
		fmt.Println(pad + "none")
	case len(s.Properties) > 0:
		printProperties(s, indent)
	case s.Unparsed != nil:
		fmt.Println(pad + compactJSON(s.Unparsed))
	case s.Ref != "" || len(s.Combinators) > 0 || (len(s.Types) > 0 && !slices.Equal(s.Types, []string{"object"})):
		fmt.Println(pad + typeSummary(s, false))
		printSchemaDetails(s, indent+4)
	default:
		fmt.Println(pad + "none")
	}
}

func printDefs(defs orderedSchemas, indent int) {
	for i, d := range defs {
		if i > 0 {
			fmt.Println()
		}
		fmt.Println(strings.Repeat(" ", indent) + d.Name + "  " + typeSummary(d.Schema, false))
		printSchemaDetails(d.Schema, indent+4)
		printChildren(d.Schema, indent+2)
	}
}

func printProperties(s *schema, indent int) {
	width := 0
	for _, p := range s.Properties {
		width = max(width, len(p.Name))
	}
	pad := strings.Repeat(" ", indent)
	for _, p := range s.Properties {
		required := slices.Contains(s.Required, p.Name)
		fmt.Printf("%s%-*s  %s\n", pad, width, p.Name, typeSummary(p.Schema, required))
		printSchemaDetails(p.Schema, indent+4)
		printChildren(p.Schema, indent+2)
	}
}

// printChildren prints nested properties of an object, or of an array's items.
func printChildren(s *schema, indent int) {
	switch {
	case len(s.Properties) > 0:
		printProperties(s, indent)
	case s.Items != nil && len(s.Items.Properties) > 0:
		printProperties(s.Items, indent)
	}
}

func printSchemaDetails(s *schema, indent int) {
	printWrapped(s.Description, indent)

	var parts []string
	if s.Default != nil {
		parts = append(parts, "default: "+compactJSON(s.Default))
	}
	if s.Const != nil {
		parts = append(parts, "const: "+compactJSON(s.Const))
	}
	if len(s.Enum) > 0 {
		parts = append(parts, "one of: "+joinJSON(s.Enum))
	}
	for _, d := range s.Details {
		parts = append(parts, d.Key+": "+compactJSON(d.Value))
	}
	if s.Items != nil {
		if len(s.Items.Enum) > 0 {
			parts = append(parts, "items one of: "+joinJSON(s.Items.Enum))
		}
		for _, d := range s.Items.Details {
			parts = append(parts, "items "+d.Key+": "+compactJSON(d.Value))
		}
	}
	printWrapped(strings.Join(parts, ", "), indent)

	combinators := s.Combinators
	if s.Items != nil {
		for _, c := range s.Items.Combinators {
			combinators = append(combinators, field{"items " + c.Key, c.Value})
		}
	}
	pad := strings.Repeat(" ", indent)
	for _, c := range combinators {
		// One line if it fits, indented JSON otherwise.
		text := compactJSON(c.Value)
		if len(pad)+len(c.Key)+2+len(text) > wrapWidth {
			var buf bytes.Buffer
			if err := json.Indent(&buf, c.Value, pad, "  "); err == nil {
				text = buf.String()
			}
		}
		fmt.Printf("%s%s: %s\n", pad, c.Key, text)
	}
}

// typeSummary renders a schema's type, e.g. "array of object, nullable, required".
func typeSummary(s *schema, required bool) string {
	parts := []string{baseType(s)}
	if slices.Contains(s.Types, "null") {
		parts = append(parts, "nullable")
	}
	if required {
		parts = append(parts, "required")
	}
	return strings.Join(parts, ", ")
}

func baseType(s *schema) string {
	switch string(s.Unparsed) {
	case "":
	case "true":
		return "any"
	case "false":
		return "never"
	default:
		return "schema " + compactJSON(s.Unparsed)
	}
	types := slices.DeleteFunc(slices.Clone(s.Types), func(t string) bool { return t == "null" })
	t := strings.Join(types, "|")
	switch {
	case t == "array" && s.Items != nil:
		return "array of " + baseType(s.Items)
	case t != "":
		return t
	case s.Ref != "":
		return "$ref " + s.Ref
	case len(s.Combinators) > 0:
		return s.Combinators[0].Key
	case len(s.Properties) > 0:
		return "object"
	default:
		return "any"
	}
}

func compactJSON(data json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return string(data)
	}
	return buf.String()
}

func joinJSON(values []json.RawMessage) string {
	var parts []string
	for _, v := range values {
		parts = append(parts, compactJSON(v))
	}
	return strings.Join(parts, " | ")
}

// printWrapped prints text word-wrapped at wrapWidth, indented. Line breaks in
// the text start new lines.
func printWrapped(text string, indent int) {
	if strings.TrimSpace(text) == "" {
		return
	}
	pad := strings.Repeat(" ", indent)
	for para := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			fmt.Println()
			continue
		}
		line := pad + words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) > wrapWidth {
				fmt.Println(line)
				line = pad + w
				continue
			}
			line += " " + w
		}
		fmt.Println(line)
	}
}
