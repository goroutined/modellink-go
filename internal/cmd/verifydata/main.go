package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	modellink "github.com/goroutined/modellink-go"
)

const (
	maxArchiveBytes = 16 << 20
	maxFileBytes    = 16 << 20
	maxTotalBytes   = 64 << 20
)

var requiredFiles = []string{
	"api.json",
	"models.json",
	"catalog.json",
	"schema.json",
	"manifest.json",
}

type dataPackage struct {
	tarball       []byte
	integrity     string
	version       string
	schemaHash    string
	schemaVersion int
	files         map[string][]byte
}

type verificationReport struct {
	Candidate packageReport  `json:"candidate"`
	Baseline  *packageReport `json:"baseline,omitempty"`
	Client    clientReport   `json:"client"`
	Registry  registryReport `json:"registry"`
	Checks    []string       `json:"checks"`
	Warnings  []string       `json:"warnings,omitempty"`
}

type packageReport struct {
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
	SchemaSHA256  string `json:"schema_sha256"`
	Models        int    `json:"models"`
	Providers     int    `json:"providers"`
	Offerings     int    `json:"offerings"`
}

type clientReport struct {
	SupportedSchemaVersion int    `json:"supported_schema_version"`
	EmbeddedSchemaVersion  int    `json:"embedded_schema_version"`
	EmbeddedSchemaSHA256   string `json:"embedded_schema_sha256"`
}

type registryReport struct {
	MetadataRequests int64 `json:"metadata_requests"`
	TarballRequests  int64 `json:"tarball_requests"`
}

type options struct {
	tarball         string
	baselineTarball string
	strictSchema    bool
	jsonReport      bool
}

func main() {
	var options options
	flag.StringVar(&options.tarball, "tarball", "", "path to the candidate @modellink/data npm tarball (required)")
	flag.StringVar(&options.baselineTarball, "baseline-tarball", "", "path to a published npm tarball to simulate an upgrade")
	flag.BoolVar(&options.strictSchema, "strict-schema", false, "require the candidate Schema to match the SDK's embedded Schema")
	flag.BoolVar(&options.jsonReport, "json", false, "write the final report as JSON")
	flag.Parse()

	if options.tarball == "" {
		fmt.Fprintln(os.Stderr, "verifydata: -tarball is required")
		os.Exit(2)
	}
	if err := run(options); err != nil {
		fmt.Fprintf(os.Stderr, "verifydata: %v\n", err)
		os.Exit(1)
	}
}

func run(options options) error {
	candidate, err := loadDataPackage(options.tarball)
	if err != nil {
		return fmt.Errorf("load candidate: %w", err)
	}

	var baseline *dataPackage
	if options.baselineTarball != "" {
		loaded, err := loadDataPackage(options.baselineTarball)
		if err != nil {
			return fmt.Errorf("load baseline: %w", err)
		}
		baseline = loaded
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	report, err := verifyPackages(ctx, baseline, candidate, options.strictSchema)
	if err != nil {
		return err
	}
	if options.jsonReport {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
		return nil
	}

	fmt.Printf("Verified @modellink/data %s with modellink-go\n", candidate.version)
	fmt.Printf("Inventory: %d models, %d providers, %d offerings\n",
		report.Candidate.Models, report.Candidate.Providers, report.Candidate.Offerings)
	if report.Baseline != nil {
		fmt.Printf("Upgrade: %s -> %s\n", report.Baseline.Version, report.Candidate.Version)
	}
	for _, warning := range report.Warnings {
		fmt.Printf("Warning: %s\n", warning)
	}
	return nil
}

func loadDataPackage(name string) (*dataPackage, error) {
	contents, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if len(contents) > maxArchiveBytes {
		return nil, errors.New("package archive exceeds size limit")
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(contents))
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gzipReader.Close()

	files := make(map[string][]byte)
	var total uint64
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if !strings.HasPrefix(header.Name, "package/") || path.Clean(header.Name) != header.Name {
			return nil, fmt.Errorf("unexpected package path %q", header.Name)
		}
		fileName := strings.TrimPrefix(header.Name, "package/")
		if fileName == "" {
			return nil, fmt.Errorf("unexpected package path %q", header.Name)
		}
		if header.Size < 0 || header.Size > maxFileBytes {
			return nil, fmt.Errorf("package file %s has invalid size", fileName)
		}
		if total+uint64(header.Size) > maxTotalBytes {
			return nil, errors.New("package files exceed total size limit")
		}
		total += uint64(header.Size)

		fileContents, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, fmt.Errorf("read package file %s: %w", fileName, err)
		}
		if int64(len(fileContents)) != header.Size {
			return nil, fmt.Errorf("package file %s has invalid size", fileName)
		}
		files[fileName] = fileContents
	}

	for _, name := range requiredFiles {
		if files[name] == nil {
			return nil, fmt.Errorf("package is missing %s", name)
		}
	}

	var manifest struct {
		Version       string                  `json:"version"`
		SchemaVersion int                     `json:"schema_version"`
		Files         map[string]manifestFile `json:"files"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.Version == "" {
		return nil, errors.New("package manifest has no version")
	}
	for _, name := range requiredFiles[:4] {
		entry := manifest.Files[name]
		if entry.SHA256 == "" {
			return nil, fmt.Errorf("manifest is missing SHA-256 for %s", name)
		}
		digest := sha256.Sum256(files[name])
		actual := hex.EncodeToString(digest[:])
		if actual != entry.SHA256 {
			return nil, fmt.Errorf("%s SHA-256 mismatch: %s != %s", name, actual, entry.SHA256)
		}
		if int64(len(files[name])) != entry.Size {
			return nil, fmt.Errorf("%s size mismatch: %d != %d", name, len(files[name]), entry.Size)
		}
	}

	schemaDigest := sha256.Sum256(files["schema.json"])
	archiveDigest := sha512.Sum512(contents)
	return &dataPackage{
		tarball:       contents,
		integrity:     "sha512-" + base64.StdEncoding.EncodeToString(archiveDigest[:]),
		version:       manifest.Version,
		schemaHash:    hex.EncodeToString(schemaDigest[:]),
		schemaVersion: manifest.SchemaVersion,
		files:         files,
	}, nil
}

type manifestFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type localRegistry struct {
	server           *httptest.Server
	mu               sync.RWMutex
	latest           *dataPackage
	metadataRequests atomic.Int64
	tarballRequests  atomic.Int64
}

func startLocalRegistry(initial *dataPackage) *localRegistry {
	registry := &localRegistry{latest: initial}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.handle))
	return registry
}

func (registry *localRegistry) handle(response http.ResponseWriter, request *http.Request) {
	requestPath := request.URL.Path
	if requestPath == "/@modellink/data/latest" || requestPath == "/@modellink%2Fdata/latest" {
		registry.metadataRequests.Add(1)
		registry.mu.RLock()
		latest := registry.latest
		registry.mu.RUnlock()
		registry.writeMetadata(response, request, latest)
		return
	}

	if version, ok := strings.CutPrefix(requestPath, "/@modellink/data/"); ok {
		registry.metadataRequests.Add(1)
		registry.mu.RLock()
		latest := registry.latest
		registry.mu.RUnlock()
		if version == latest.version {
			registry.writeMetadata(response, request, latest)
			return
		}
	}
	archiveName, ok := strings.CutPrefix(requestPath, "/tarballs/")
	version, hasSuffix := strings.CutSuffix(archiveName, ".tgz")
	if ok && hasSuffix {
		registry.mu.RLock()
		latest := registry.latest
		registry.mu.RUnlock()
		if version == latest.version {
			registry.tarballRequests.Add(1)
			response.Header().Set("Content-Type", "application/gzip")
			_, _ = response.Write(latest.tarball)
			return
		}
	}
	http.NotFound(response, request)
}

func (registry *localRegistry) writeMetadata(
	response http.ResponseWriter,
	request *http.Request,
	release *dataPackage,
) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"version": release.version,
		"dist": map[string]string{
			"tarball":   "http://" + request.Host + "/tarballs/" + release.version + ".tgz",
			"integrity": release.integrity,
		},
	})
}

func (registry *localRegistry) setLatest(candidate *dataPackage) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.latest = candidate
}

func (registry *localRegistry) close() {
	registry.server.Close()
}

func verifyPackages(
	ctx context.Context,
	baseline *dataPackage,
	candidate *dataPackage,
	strictSchema bool,
) (*verificationReport, error) {
	if candidate.schemaVersion > modellink.SupportedSchemaVersion {
		return nil, fmt.Errorf(
			"candidate Schema v%d is newer than modellink-go Schema v%d",
			candidate.schemaVersion,
			modellink.SupportedSchemaVersion,
		)
	}

	embeddedSchema := modellink.SchemaInfo()
	if strictSchema && candidate.schemaHash != embeddedSchema.SchemaSHA256 {
		return nil, fmt.Errorf(
			"candidate Schema SHA-256 %s does not match embedded SDK Schema SHA-256 %s",
			candidate.schemaHash,
			embeddedSchema.SchemaSHA256,
		)
	}

	initial := candidate
	if baseline != nil {
		initial = baseline
	}
	registry := startLocalRegistry(initial)
	defer registry.close()

	tempDir, err := os.MkdirTemp("", "modellink-verifydata-")
	if err != nil {
		return nil, fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	cache, err := modellink.NewFileCache(modellink.FileCacheOptions{
		Directory:   tempDir,
		MaxVersions: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("create temporary cache: %w", err)
	}
	client, err := modellink.New(modellink.Options{
		Registry: registry.server.URL,
		Cache:    cache,
	})
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	checks := []string{
		"registry_metadata",
		"tarball_integrity",
		"manifest_sha256",
		"go_typed_decode",
		"raw_files",
		"inventory",
		"cache_reload_offline",
	}
	var baselineReport *packageReport
	if baseline != nil {
		baselineSnapshot, err := client.LoadLatest(ctx)
		if err != nil {
			return nil, fmt.Errorf("load baseline %s: %w", baseline.version, err)
		}
		baselineInventory, err := verifySnapshot(baselineSnapshot, baseline)
		if err != nil {
			return nil, fmt.Errorf("verify baseline %s: %w", baseline.version, err)
		}
		report := packageReportFor(baseline, baselineInventory)
		baselineReport = &report

		registry.setLatest(candidate)
		status, err := client.CheckLatest(ctx)
		if err != nil {
			return nil, fmt.Errorf("check candidate: %w", err)
		}
		if status.CurrentVersion != baseline.version ||
			status.LatestVersion != candidate.version ||
			!status.UpdateAvailable ||
			status.RegistryBehind {
			return nil, fmt.Errorf(
				"unexpected update status: current=%s latest=%s update_available=%t registry_behind=%t",
				status.CurrentVersion,
				status.LatestVersion,
				status.UpdateAvailable,
				status.RegistryBehind,
			)
		}
		checks = append(checks, "upgrade_from_baseline")
	} else {
		found, err := client.FindLatest(ctx)
		if err != nil {
			return nil, fmt.Errorf("find candidate: %w", err)
		}
		if found != candidate.version {
			return nil, fmt.Errorf("registry returned %q, candidate is %q", found, candidate.version)
		}
	}

	candidateSnapshot, err := client.LoadLatest(ctx)
	if err != nil {
		return nil, fmt.Errorf("load candidate %s: %w", candidate.version, err)
	}
	if candidateSnapshot.Manifest.Version != candidate.version {
		return nil, fmt.Errorf(
			"loaded %s, candidate is %s; candidate may be older than baseline",
			candidateSnapshot.Manifest.Version,
			candidate.version,
		)
	}
	inventory, err := verifySnapshot(candidateSnapshot, candidate)
	if err != nil {
		return nil, fmt.Errorf("verify candidate %s: %w", candidate.version, err)
	}

	current, err := client.CurrentVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current version: %w", err)
	}
	if current != candidate.version {
		return nil, fmt.Errorf("current version is %q, candidate is %q", current, candidate.version)
	}

	metadataBefore := registry.metadataRequests.Load()
	tarballsBefore := registry.tarballRequests.Load()
	cachedSnapshot, err := client.LoadCached(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cached snapshot: %w", err)
	}
	if cachedSnapshot.Manifest.Version != candidate.version {
		return nil, fmt.Errorf("cached version is %q, candidate is %q", cachedSnapshot.Manifest.Version, candidate.version)
	}
	loadedSnapshot, err := client.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load from cache: %w", err)
	}
	if loadedSnapshot.Manifest.Version != candidate.version {
		return nil, fmt.Errorf("Load returned %q, candidate is %q", loadedSnapshot.Manifest.Version, candidate.version)
	}
	if registry.metadataRequests.Load() != metadataBefore || registry.tarballRequests.Load() != tarballsBefore {
		return nil, errors.New("cached reload accessed the local registry")
	}

	warnings := make([]string, 0)
	for _, warning := range candidateSnapshot.Warnings() {
		warnings = append(warnings, warning.Message)
	}

	return &verificationReport{
		Candidate: packageReportFor(candidate, inventory),
		Baseline:  baselineReport,
		Client: clientReport{
			SupportedSchemaVersion: modellink.SupportedSchemaVersion,
			EmbeddedSchemaVersion:  embeddedSchema.SchemaVersion,
			EmbeddedSchemaSHA256:   embeddedSchema.SchemaSHA256,
		},
		Registry: registryReport{
			MetadataRequests: registry.metadataRequests.Load(),
			TarballRequests:  registry.tarballRequests.Load(),
		},
		Checks:   checks,
		Warnings: warnings,
	}, nil
}

type inventory struct {
	models    int
	providers int
	offerings int
}

func verifySnapshot(snapshot *modellink.Snapshot, candidate *dataPackage) (inventory, error) {
	for _, name := range requiredFiles {
		contents, ok := snapshot.File(modellink.DataFile(name))
		if !ok {
			return inventory{}, fmt.Errorf("snapshot is missing %s", name)
		}
		if !bytes.Equal(contents, candidate.files[name]) {
			return inventory{}, fmt.Errorf("snapshot %s does not match candidate package", name)
		}
	}

	var rawModels map[string]json.RawMessage
	if err := json.Unmarshal(candidate.files["models.json"], &rawModels); err != nil {
		return inventory{}, fmt.Errorf("decode models.json keys: %w", err)
	}
	var rawProviders map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(candidate.files["api.json"], &rawProviders); err != nil {
		return inventory{}, fmt.Errorf("decode api.json keys: %w", err)
	}

	if len(rawModels) != len(snapshot.Catalog.Models) {
		return inventory{}, fmt.Errorf(
			"model count mismatch: raw=%d typed=%d",
			len(rawModels),
			len(snapshot.Catalog.Models),
		)
	}
	if len(rawProviders) != len(snapshot.Catalog.Providers) {
		return inventory{}, fmt.Errorf(
			"provider count mismatch: raw=%d typed=%d",
			len(rawProviders),
			len(snapshot.Catalog.Providers),
		)
	}
	if len(rawModels) == 0 || len(rawProviders) == 0 {
		return inventory{}, errors.New("catalog has no models or providers")
	}

	for id := range rawModels {
		model, ok := snapshot.Model(id)
		if !ok {
			return inventory{}, fmt.Errorf("typed catalog is missing model %q", id)
		}
		if model.ID != id {
			return inventory{}, fmt.Errorf("model key %q has typed ID %q", id, model.ID)
		}
	}

	offerings := 0
	for providerID, rawProvider := range rawProviders {
		provider, ok := snapshot.Provider(providerID)
		if !ok {
			return inventory{}, fmt.Errorf("typed catalog is missing provider %q", providerID)
		}
		if provider.ID != providerID {
			return inventory{}, fmt.Errorf("provider key %q has typed ID %q", providerID, provider.ID)
		}
		if len(rawProvider.Models) != len(provider.Models) {
			return inventory{}, fmt.Errorf(
				"provider %s model count mismatch: raw=%d typed=%d",
				providerID,
				len(rawProvider.Models),
				len(provider.Models),
			)
		}
		for modelID := range rawProvider.Models {
			providerModel, ok := snapshot.ProviderModel(providerID, modelID)
			if !ok {
				return inventory{}, fmt.Errorf("typed catalog is missing offering %s/%s", providerID, modelID)
			}
			if providerModel.ID != modelID {
				return inventory{}, fmt.Errorf(
					"offering key %s/%s has typed ID %q",
					providerID,
					modelID,
					providerModel.ID,
				)
			}
		}
		offerings += len(rawProvider.Models)
	}

	return inventory{
		models:    len(rawModels),
		providers: len(rawProviders),
		offerings: offerings,
	}, nil
}

func packageReportFor(candidate *dataPackage, inventory inventory) packageReport {
	return packageReport{
		Version:       candidate.version,
		SchemaVersion: candidate.schemaVersion,
		SchemaSHA256:  candidate.schemaHash,
		Models:        inventory.models,
		Providers:     inventory.providers,
		Offerings:     inventory.offerings,
	}
}
