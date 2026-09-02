package modellink

// Snapshot is one immutable, verified ModelLink catalog version.
type Snapshot struct {
	Manifest Manifest
	Catalog  Catalog

	files    map[DataFile][]byte
	warnings []Warning
}

// DataFile identifies a JSON file published in @modellink/data.
type DataFile string

const (
	FileAPI      DataFile = "api.json"
	FileModels   DataFile = "models.json"
	FileCatalog  DataFile = "catalog.json"
	FileSchema   DataFile = "schema.json"
	FileManifest DataFile = "manifest.json"
)

// Schema returns a copy of the schema published with this snapshot.
func (snapshot *Snapshot) Schema() []byte {
	contents, _ := snapshot.File(FileSchema)
	return contents
}

// File returns a copy of a verified raw JSON file.
func (snapshot *Snapshot) File(name DataFile) ([]byte, bool) {
	if snapshot == nil {
		return nil, false
	}
	contents, ok := snapshot.files[name]
	return append([]byte(nil), contents...), ok
}

// Warnings returns a copy of the non-fatal compatibility warnings found while
// loading this snapshot.
func (snapshot *Snapshot) Warnings() []Warning {
	if snapshot == nil {
		return nil
	}
	return append([]Warning(nil), snapshot.warnings...)
}

func (snapshot *Snapshot) Model(id string) (ModelMetadata, bool) {
	if snapshot == nil {
		return ModelMetadata{}, false
	}
	model, ok := snapshot.Catalog.Models[id]
	return model, ok
}

func (snapshot *Snapshot) Provider(id string) (Provider, bool) {
	if snapshot == nil {
		return Provider{}, false
	}
	provider, ok := snapshot.Catalog.Providers[id]
	return provider, ok
}

func (snapshot *Snapshot) Offering(
	providerID string,
	modelID string,
) (ProviderModel, bool) {
	provider, ok := snapshot.Provider(providerID)
	if !ok {
		return ProviderModel{}, false
	}
	model, ok := provider.Models[modelID]
	return model, ok
}
