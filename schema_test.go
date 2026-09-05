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

func TestProviderLinksPreserveOptionalFields(t *testing.T) {
	var legacy Provider
	if err := json.Unmarshal([]byte(`{"id":"example"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Links != nil {
		t.Fatal("missing links must remain nil for older data")
	}
	var provider Provider
	if err := json.Unmarshal([]byte(`{"links":{"models":"https://example.com/models","pricing":"https://example.com/pricing","api_key":"https://example.com/quickstart","console":"https://example.com/console"}}`), &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Links == nil {
		t.Fatal("provider links were not decoded")
	}
	for want, actual := range map[string]*string{
		"https://example.com/models":     provider.Links.Models,
		"https://example.com/pricing":    provider.Links.Pricing,
		"https://example.com/quickstart": provider.Links.APIKey,
		"https://example.com/console":    provider.Links.Console,
	} {
		if actual == nil || *actual != want {
			t.Fatalf("link %v, want %q", actual, want)
		}
	}
	var partial Provider
	if err := json.Unmarshal([]byte(`{"links":{"models":"https://example.com/models"}}`), &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Links == nil || partial.Links.APIKey != nil || partial.Links.Pricing != nil || partial.Links.Console != nil {
		t.Fatal("missing purpose-specific links must remain nil")
	}
}
