package main

import (
	"path/filepath"
	"testing"
)

func TestSchemaArgumentIsRelative(t *testing.T) {
	argument := schemaArgument()
	if filepath.IsAbs(argument) {
		t.Fatalf("schema argument %q must be relative", argument)
	}
	if volume := filepath.VolumeName(argument); volume != "" {
		t.Fatalf("schema argument %q has volume %q", argument, volume)
	}
}
