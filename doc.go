// Package modellink downloads, verifies, caches and parses the versioned
// ModelLink catalog for Go applications.
//
// The zero-value Options use the domestic npmmirror registry and a shared,
// cross-platform file cache:
//
//	client, err := modellink.New(modellink.Options{})
//	snapshot, err := client.Load(ctx)
//	model, ok := snapshot.ProviderModel("deepseek", "deepseek-v4-pro")
//
// Load prefers the active verified cache and only calls LoadLatest when no
// usable cache exists. LoadCached never accesses the registry; LoadLatest
// always checks it and prevents automatic downgrade. LoadVersion reads an
// explicit version without activating it, while SwitchVersion deliberately
// changes the active version. Use NewFileCache to customize local retention,
// or implement Cache to connect Redis or another shared backend. Compatibility
// warnings are available from Snapshot.Warnings and Options.OnWarning.
package modellink
