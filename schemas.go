// Package generator is the module root. It exists only to embed the shipped
// JSON Schema artifacts so the single static binary carries them with no
// runtime file dependency. Code that needs a schema reads it from SchemaFS.
package generator

import "embed"

// SchemaFS holds the authoritative message-definition schema and the config
// schema. They remain the source of truth in schema/; this just bundles them.
//
//go:embed schema/sofabuffers-schema-v1.json schema/sofabgen-config-schema.json
var SchemaFS embed.FS

// ConfigSchemaPath / MessageSchemaPath are the in-FS names.
const (
	MessageSchemaPath = "schema/sofabuffers-schema-v1.json"
	ConfigSchemaPath  = "schema/sofabgen-config-schema.json"
)
