package modellink

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
