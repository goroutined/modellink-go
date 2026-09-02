package modellink

import (
	"context"
	"sync"
	"testing"
)

func TestSchemaWarnings(t *testing.T) {
	embedded := SchemaInfo()
	tests := []struct {
		name          string
		version       string
		schemaVersion int
		hash          string
		want          WarningCode
	}{
		{name: "exact", version: embedded.PackageVersion, schemaVersion: embedded.SchemaVersion, hash: embedded.SchemaSHA256},
		{name: "newer data package", version: "99.0.0", schemaVersion: embedded.SchemaVersion, hash: "newer", want: WarningSchemaSDKOutdated},
		{name: "older data package", version: "0.0.1", schemaVersion: embedded.SchemaVersion, hash: "older", want: WarningSchemaDataOutdated},
		{name: "older schema version", version: "99.0.0", schemaVersion: embedded.SchemaVersion - 1, hash: "older", want: WarningSchemaDataOutdated},
		{name: "unresolved drift", version: embedded.PackageVersion, schemaVersion: embedded.SchemaVersion, hash: "different", want: WarningSchemaMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := Manifest{
				Version:       test.version,
				SchemaVersion: test.schemaVersion,
				Files: ManifestFiles{
					SchemaJSON: ManifestFile{SHA256: test.hash},
				},
			}
			warnings := schemaWarnings(manifest)
			if test.want == "" {
				if len(warnings) != 0 {
					t.Fatalf("schemaWarnings returned %+v, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 || warnings[0].Code != test.want {
				t.Fatalf("schemaWarnings returned %+v, want %q", warnings, test.want)
			}
			if warnings[0].DataPackageVersion != test.version ||
				warnings[0].EmbeddedPackageVersion != embedded.PackageVersion {
				t.Fatalf("warning metadata is incomplete: %+v", warnings[0])
			}
		})
	}
}

func TestWarningHandlerRunsOncePerClient(t *testing.T) {
	registry := newTestRegistry(t, "99.0.0", map[string]int{"99.0.0": SupportedSchemaVersion})
	var mutex sync.Mutex
	var received []Warning
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
		OnWarning: func(warning Warning) {
			mutex.Lock()
			received = append(received, warning)
			mutex.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 24
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := client.LoadLatest(context.Background())
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := client.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(received) != 1 {
		t.Fatalf("warning handler ran %d times, want 1", len(received))
	}
	if received[0].Code != WarningSchemaSDKOutdated {
		t.Fatalf("warning code is %q", received[0].Code)
	}
}

func TestSnapshotWarningsReturnsCopy(t *testing.T) {
	snapshot := &Snapshot{warnings: []Warning{{Code: WarningSchemaMismatch}}}
	warnings := snapshot.Warnings()
	warnings[0].Code = WarningSchemaDataOutdated
	if snapshot.Warnings()[0].Code != WarningSchemaMismatch {
		t.Fatal("Snapshot.Warnings exposed mutable internal state")
	}
}

func TestComparePackageVersions(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  int
	}{
		{left: "1.2.3", right: "1.2.3"},
		{left: "1.2.4", right: "1.2.3", want: 1},
		{left: "1.2.3-alpha.2", right: "1.2.3-alpha.10", want: -1},
		{left: "1.2.3", right: "1.2.3-rc.1", want: 1},
		{left: "invalid", right: "1.2.3"},
	} {
		if got := comparePackageVersions(test.left, test.right); got != test.want {
			t.Fatalf("comparePackageVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
