package modellink

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestEmbeddedSchemaMatchesLock(t *testing.T) {
	metadata := SchemaInfo()
	if metadata.SchemaVersion != SupportedSchemaVersion {
		t.Fatalf(
			"embedded schema version %d does not match supported version %d",
			metadata.SchemaVersion,
			SupportedSchemaVersion,
		)
	}
	digest := sha256.Sum256(Schema())
	if actual := hex.EncodeToString(digest[:]); actual != metadata.SchemaSHA256 {
		t.Fatalf("embedded schema SHA-256 is %s, lock contains %s", actual, metadata.SchemaSHA256)
	}
	var schema map[string]any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["x-modellink-schema-version"] != float64(SupportedSchemaVersion) {
		t.Fatal("embedded schema declares an unexpected compatibility version")
	}
}

func TestOptionalBooleanKeepsUnknownFalseAndTrueDistinct(t *testing.T) {
	for _, test := range []struct {
		input string
		want  *bool
	}{
		{input: `{}`},
		{input: `{"temperature":false}`, want: boolPointer(false)},
		{input: `{"temperature":true}`, want: boolPointer(true)},
	} {
		var model ModelMetadata
		if err := json.Unmarshal([]byte(test.input), &model); err != nil {
			t.Fatal(err)
		}
		if test.want == nil {
			if model.Temperature != nil {
				t.Fatal("missing optional boolean was not preserved")
			}
			continue
		}
		if model.Temperature == nil || *model.Temperature != *test.want {
			t.Fatalf("decoded temperature %v, want %v", model.Temperature, test.want)
		}
	}
}

func boolPointer(value bool) *bool {
	return &value
}
