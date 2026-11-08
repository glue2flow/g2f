package nodes

import (
	"fmt"
	"regexp"
	"strings"
)

// OutputSchema describes the items a node emits. The editor uses it to offer
// fields in the dynamic content picker (⚡) before the workflow has run; after a
// run, the recorded output is used as well.
type OutputSchema struct {
	// Passthrough means the items keep the fields of the input items (e.g. If,
	// Log, Wait). Fields are added on top.
	Passthrough bool `json:"passthrough,omitempty" yaml:"passthrough"`
	// PassthroughIf names a boolean parameter that turns passthrough on
	// (e.g. Set Fields' keepExisting). Empty: always.
	PassthroughIf string `json:"passthroughIf,omitempty" yaml:"passthroughIf"`
	// FromParam names a parameter whose value describes the items: an object
	// (its keys become fields) or a list of objects (the first one is used).
	// Used where the node emits what the user configured (Set Fields, test payloads).
	FromParam string `json:"fromParam,omitempty" yaml:"fromParam"`
	// Fields lists the fields of each output item.
	Fields []OutputField `json:"fields,omitempty" yaml:"fields"`
	// Dynamic means the shape depends on code or another workflow and cannot
	// be declared; the editor relies on the output of a previous run.
	Dynamic bool `json:"dynamic,omitempty" yaml:"dynamic"`
}

// OutputField is one field of an output item.
type OutputField struct {
	// Path is a dot path, e.g. "usage.totalTokens". Parents need not be listed.
	Path string `json:"path" yaml:"path"`
	// Type is string, number, boolean, object, array or any (default).
	Type        string `json:"type,omitempty" yaml:"type"`
	Description string `json:"description,omitempty" yaml:"description"`
	// FromParam: the keys of this parameter's value become sub-fields
	// (e.g. the webhook trigger's body from its test body).
	FromParam string `json:"fromParam,omitempty" yaml:"fromParam"`
}

var (
	fieldSegRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$-]*$`)
	fieldTypes = map[string]bool{"": true, "string": true, "number": true, "boolean": true, "object": true, "array": true, "any": true}
)

// Validate checks paths, types and parameter references. params are the
// parameters of the node (FromParam must name one of them).
func (s *OutputSchema) Validate(params []Param) error {
	if s == nil {
		return nil
	}
	hasParam := func(name string) bool {
		for _, p := range params {
			if p.Name == name {
				return true
			}
		}
		return false
	}
	if s.FromParam != "" && !hasParam(s.FromParam) {
		return fmt.Errorf("outputSchema.fromParam: unknown parameter %q", s.FromParam)
	}
	if s.PassthroughIf != "" && (!s.Passthrough || !hasParam(s.PassthroughIf)) {
		return fmt.Errorf("outputSchema.passthroughIf: needs passthrough and an existing parameter (%q)", s.PassthroughIf)
	}
	seen := map[string]bool{}
	for i, f := range s.Fields {
		if f.Path == "" {
			return fmt.Errorf("outputSchema.fields[%d]: path is required", i)
		}
		for _, seg := range strings.Split(f.Path, ".") {
			if !fieldSegRe.MatchString(seg) {
				return fmt.Errorf("outputSchema.fields[%d]: invalid path %q", i, f.Path)
			}
		}
		if seen[f.Path] {
			return fmt.Errorf("outputSchema: duplicate field %q", f.Path)
		}
		seen[f.Path] = true
		if !fieldTypes[f.Type] {
			return fmt.Errorf("outputSchema.fields[%d]: unknown type %q", i, f.Type)
		}
		if f.FromParam != "" && !hasParam(f.FromParam) {
			return fmt.Errorf("outputSchema.fields[%d]: unknown parameter %q", i, f.FromParam)
		}
	}
	return nil
}

// fields is a compact way to declare output fields: "path:type" or
// "path:type:description".
func fields(specs ...string) []OutputField {
	out := make([]OutputField, 0, len(specs))
	for _, s := range specs {
		parts := strings.SplitN(s, ":", 3)
		f := OutputField{Path: parts[0]}
		if len(parts) > 1 {
			f.Type = parts[1]
		}
		if len(parts) > 2 {
			f.Description = parts[2]
		}
		out = append(out, f)
	}
	return out
}

// dynamicSchema is the schema of nodes whose output is computed by user code.
var dynamicSchema = &OutputSchema{Dynamic: true}

// passthroughSchema is the schema of nodes that emit their input items.
var passthroughSchema = &OutputSchema{Passthrough: true}

// webhookRequestFields are the fields of an item built from an HTTP request.
var webhookRequestFields = fields(
	"method:string:HTTP method",
	"headers:object:Request headers (lower-case names)",
	"query:object:Query parameters",
	"body:any:Parsed JSON or form body, or the raw text",
	"receivedAt:string:Time the request arrived (RFC 3339)",
)
