// Package modellink downloads, verifies, caches and parses the versioned
// ModelLink catalog for Go applications.
//
// The zero-value Options use the domestic npmmirror registry and a shared,
// cross-platform file cache:
//
//	client, err := modellink.New(modellink.Options{})
//	snapshot, err := client.Load(ctx)
//	model, ok := snapshot.Offering("deepseek", "deepseek-v4-pro")
//
// Load reads the active verified cache without checking the network. Use
// CheckLatest and LoadLatest when the application decides to check for an
// update. Use NewFileCache to customize local retention, or implement Cache to
// connect Redis or another shared backend. Schema compatibility warnings are
// available from Snapshot.Warnings and can also be delivered through
// Options.OnWarning.
package modellink
