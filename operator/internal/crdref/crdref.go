package crdref

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// header is emitted verbatim at the top of the generated document. No
// timestamp, no tool version, no git SHA -- anything non-deterministic
// would make crd-docs-check fire on every unrelated run.
const header = `# CRD reference

<!-- GENERATED FILE - DO NOT EDIT.
     Regenerate:  cd operator && make crd-docs
     Source of truth: the Go doc comments and +kubebuilder markers on
     operator/api/v1alpha1/*.go, via controller-gen into
     operator/config/crd/bases/*.yaml.
     CI (.github/workflows/operator.yml, job ` + "`api`" + `) regenerates this file
     and fails the build if the committed copy differs. -->
`

var whitespaceRe = regexp.MustCompile(`\s+`)

// Render returns the complete markdown document for crds, in the order
// given. It is a pure function of its input: no clock, no randomness, no
// I/O. Returns an error (never panics) for a CRD with no spec.versions, or
// a version with a nil Schema/OpenAPIV3Schema.
func Render(crds []*apiextensionsv1.CustomResourceDefinition) (string, error) {
	var b strings.Builder
	b.WriteString(header)
	for _, crd := range crds {
		section, err := renderCRD(crd)
		if err != nil {
			return "", err
		}
		b.WriteString(section)
	}
	return b.String(), nil
}

func renderCRD(crd *apiextensionsv1.CustomResourceDefinition) (string, error) {
	if len(crd.Spec.Versions) == 0 {
		return "", fmt.Errorf("crdref: CRD %s has no spec.versions", crd.Name)
	}
	version := selectVersion(crd)
	if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
		return "", fmt.Errorf("crdref: CRD %s version %s has no schema.openAPIV3Schema", crd.Name, version.Name)
	}
	schema := version.Schema.OpenAPIV3Schema

	var b strings.Builder
	fmt.Fprintf(&b, "\n## %s\n\n", crd.Spec.Names.Kind)

	writeMetadataBullets(&b, crd, version)

	if desc := cleanDescription(schema.Description); desc != "" {
		fmt.Fprintf(&b, "\n%s\n", desc)
	}

	writePrinterColumns(&b, version.AdditionalPrinterColumns)

	w := &walker{}
	specSchema := schema.Properties["spec"]
	statusSchema := schema.Properties["status"]
	w.walk("spec", &specSchema, false, false)
	w.walk("status", &statusSchema, false, false)

	b.WriteString("\n### .spec\n\n")
	writeFieldTable(&b, fieldsWithPrefix(w.fields, "spec"))

	b.WriteString("\n### .status\n\n")
	writeFieldTable(&b, fieldsWithPrefix(w.fields, "status"))

	writeValidationRules(&b, w.validations)

	return b.String(), nil
}

// selectVersion returns the storage version (there must be exactly one),
// falling back to the first version if none is marked storage=true (should
// not happen for a well-formed CRD, but Render must not panic on it).
func selectVersion(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinitionVersion {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			return &crd.Spec.Versions[i]
		}
	}
	return &crd.Spec.Versions[0]
}

func writeMetadataBullets(b *strings.Builder, crd *apiextensionsv1.CustomResourceDefinition, version *apiextensionsv1.CustomResourceDefinitionVersion) {
	versionNames := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		versionNames = append(versionNames, v.Name)
	}

	statusSubresource := "no"
	if version.Subresources != nil && version.Subresources.Status != nil {
		statusSubresource = "yes"
	}

	shortNames := "—"
	if len(crd.Spec.Names.ShortNames) > 0 {
		shortNames = strings.Join(crd.Spec.Names.ShortNames, ", ")
	}
	categories := "—"
	if len(crd.Spec.Names.Categories) > 0 {
		categories = strings.Join(crd.Spec.Names.Categories, ", ")
	}

	fmt.Fprintf(b, "- **API group/version:** `%s/%s`\n", crd.Spec.Group, strings.Join(versionNames, ", "))
	fmt.Fprintf(b, "- **Scope:** %s\n", crd.Spec.Scope)
	fmt.Fprintf(b, "- **Short names:** %s\n", shortNames)
	fmt.Fprintf(b, "- **Categories:** %s\n", categories)
	fmt.Fprintf(b, "- **Status subresource:** %s\n", statusSubresource)
}

func writePrinterColumns(b *strings.Builder, cols []apiextensionsv1.CustomResourceColumnDefinition) {
	if len(cols) == 0 {
		return
	}
	b.WriteString("\n### Printer columns\n\n")
	b.WriteString("| Name | Type | JSONPath | Priority |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, c := range cols {
		fmt.Fprintf(b, "| %s | %s | `%s` | %d |\n", c.Name, c.Type, c.JSONPath, c.Priority)
	}
}

func writeFieldTable(b *strings.Builder, rows []fieldRow) {
	b.WriteString("| Field | Type | Required | Default | Constraints | Description |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s |\n", r.field, r.typ, r.required, r.def, r.constraints, r.description)
	}
}

func writeValidationRules(b *strings.Builder, rows []validationRow) {
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n### Validation rules (CEL)\n\n")
	b.WriteString("| Path | Rule | Message |\n")
	b.WriteString("|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| `%s` | `%s` | %s |\n", r.path, escapeCell(r.rule), escapeCell(r.message))
	}
}

func fieldsWithPrefix(rows []fieldRow, prefix string) []fieldRow {
	out := make([]fieldRow, 0, len(rows))
	dotted := prefix + "."
	for _, r := range rows {
		if strings.HasPrefix(r.field, dotted) {
			out = append(out, r)
		}
	}
	return out
}

// fieldRow is one row of a .spec/.status field table.
type fieldRow struct {
	field       string
	typ         string
	required    string
	def         string
	constraints string
	description string
}

// validationRow is one row of the "Validation rules (CEL)" table.
type validationRow struct {
	path    string
	rule    string
	message string
}

// walker performs the depth-first, sorted-keys traversal that produces
// both the field-table rows and the CEL-validation rows in one pass, so
// their paths can never disagree.
type walker struct {
	fields      []fieldRow
	validations []validationRow
}

// walk visits schema at path. When emit is true a fieldRow is appended for
// this node itself (required reflects whether the *parent* schema's
// Required list names this property). Properties are visited in
// sort.Strings order at every level -- Go map iteration over
// JSONSchemaProps.Properties is randomized, and without this sort the
// generator would produce a different file on every run, turning the
// crd-docs-check drift check into a coin flip.
func (w *walker) walk(path string, schema *apiextensionsv1.JSONSchemaProps, required bool, emit bool) {
	if schema == nil {
		return
	}
	if emit {
		w.fields = append(w.fields, buildRow(path, required, schema))
	}
	for _, v := range schema.XValidations {
		w.validations = append(w.validations, validationRow{path: path, rule: v.Rule, message: v.Message})
	}

	if len(schema.Properties) > 0 {
		names := make([]string, 0, len(schema.Properties))
		for name := range schema.Properties {
			names = append(names, name)
		}
		sort.Strings(names)

		requiredSet := make(map[string]bool, len(schema.Required))
		for _, r := range schema.Required {
			requiredSet[r] = true
		}

		for _, name := range names {
			child := schema.Properties[name]
			w.walk(path+"."+name, &child, requiredSet[name], true)
		}
	}

	if schema.Items != nil && schema.Items.Schema != nil {
		w.walk(path+"[]", schema.Items.Schema, false, true)
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		w.walk(path+"{}", schema.AdditionalProperties.Schema, false, true)
	}
}

func buildRow(path string, required bool, prop *apiextensionsv1.JSONSchemaProps) fieldRow {
	req := "no"
	if required {
		req = "yes"
	}
	return fieldRow{
		field:       path,
		typ:         typeString(prop),
		required:    req,
		def:         defaultString(prop),
		constraints: constraintsString(prop),
		description: cleanDescription(prop.Description),
	}
}

// coreType returns the base type cell for prop, without the int-or-string
// or free-form suffixes: array<itemType> for arrays, map[string]valueType
// for maps (additionalProperties with a schema), "object" for plain
// objects, else prop.Type verbatim (which may be empty, e.g. an
// int-or-string field expressed via anyOf rather than type).
func coreType(prop *apiextensionsv1.JSONSchemaProps) string {
	switch {
	case prop.Type == "array":
		item := ""
		if prop.Items != nil && prop.Items.Schema != nil {
			item = coreType(prop.Items.Schema)
		}
		return fmt.Sprintf("array<%s>", item)
	case prop.AdditionalProperties != nil && prop.AdditionalProperties.Schema != nil:
		return fmt.Sprintf("map[string]%s", coreType(prop.AdditionalProperties.Schema))
	case prop.Type == "object":
		return "object"
	default:
		return prop.Type
	}
}

func typeString(prop *apiextensionsv1.JSONSchemaProps) string {
	t := coreType(prop)
	if prop.XIntOrString {
		t = strings.TrimSpace(t + " (int-or-string)")
	}
	if prop.XPreserveUnknownFields != nil && *prop.XPreserveUnknownFields {
		t = strings.TrimSpace(t + " (free-form)")
	}
	return t
}

func defaultString(prop *apiextensionsv1.JSONSchemaProps) string {
	if prop.Default == nil || len(prop.Default.Raw) == 0 {
		return "—"
	}
	return "`" + string(prop.Default.Raw) + "`"
}

func constraintsString(prop *apiextensionsv1.JSONSchemaProps) string {
	var parts []string

	if len(prop.Enum) > 0 {
		vals := make([]string, 0, len(prop.Enum))
		for _, e := range prop.Enum {
			vals = append(vals, enumValueString(e))
		}
		parts = append(parts, "enum: "+strings.Join(vals, ", "))
	}
	if prop.MinLength != nil {
		parts = append(parts, fmt.Sprintf("minLength: %d", *prop.MinLength))
	}
	if prop.MaxLength != nil {
		parts = append(parts, fmt.Sprintf("maxLength: %d", *prop.MaxLength))
	}
	if prop.Pattern != "" {
		parts = append(parts, fmt.Sprintf("pattern: `%s`", prop.Pattern))
	}
	if prop.Minimum != nil {
		parts = append(parts, fmt.Sprintf("minimum: %s", formatFloat(*prop.Minimum)))
	}
	if prop.Maximum != nil {
		parts = append(parts, fmt.Sprintf("maximum: %s", formatFloat(*prop.Maximum)))
	}
	if prop.MaxItems != nil {
		parts = append(parts, fmt.Sprintf("maxItems: %d", *prop.MaxItems))
	}
	if prop.MaxProperties != nil {
		parts = append(parts, fmt.Sprintf("maxProperties: %d", *prop.MaxProperties))
	}
	if prop.Format != "" {
		parts = append(parts, fmt.Sprintf("format: %s", prop.Format))
	}

	if len(parts) == 0 {
		return "—"
	}
	return escapeCell(strings.Join(parts, "; "))
}

// enumValueString renders one enum member's raw JSON as plain text (no
// surrounding quotes for strings), so "rootless-podman" renders as
// rootless-podman, not "rootless-podman".
func enumValueString(j apiextensionsv1.JSON) string {
	var v interface{}
	if err := json.Unmarshal(j.Raw, &v); err != nil {
		return string(j.Raw)
	}
	return fmt.Sprint(v)
}

// formatFloat renders a whole-numbered float without a trailing ".0" (e.g.
// -1000, not -1000.000000), while still handling a genuine fraction.
func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// cleanDescription collapses newlines/runs of whitespace to a single space
// and escapes "|" so a multi-line controller-gen doc comment (emitted as a
// YAML |- block scalar) can never break a markdown table row.
func cleanDescription(s string) string {
	s = whitespaceRe.ReplaceAllString(strings.TrimSpace(s), " ")
	return escapeCell(s)
}

// escapeCell escapes "|" defensively so any table-cell value -- a
// description, a CEL rule, a CEL message -- can never break the table it
// is rendered into.
func escapeCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
