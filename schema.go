package modellink

import (
	_ "embed"
	"encoding/json"
	"sync"
)

const SupportedSchemaVersion = 2

//go:embed schema/schema.json
var embeddedSchema []byte

//go:embed schema/schema.lock.json
var embeddedSchemaLock []byte

type SchemaMetadata struct {
	Package        string `json:"package"`
	PackageVersion string `json:"package_version"`
	SchemaVersion  int    `json:"schema_version"`
	SchemaSHA256   string `json:"schema_sha256"`
	SourceRevision string `json:"source_revision"`
}

var (
	schemaMetadataOnce sync.Once
	schemaMetadata     SchemaMetadata
)

func Schema() []byte {
	return append([]byte(nil), embeddedSchema...)
}

func SchemaInfo() SchemaMetadata {
	schemaMetadataOnce.Do(func() {
		if err := json.Unmarshal(embeddedSchemaLock, &schemaMetadata); err != nil {
			panic("modellink: invalid embedded schema lock: " + err.Error())
		}
	})
	return schemaMetadata
}
