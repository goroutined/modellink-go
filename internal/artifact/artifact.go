package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	PackageName           = "@modellink/data"
	DefaultRegistry       = "https://registry.npmmirror.com"
	AuthoritativeRegistry = "https://registry.npmjs.org"
	maxMetadataBytes      = 1 << 20
	maxArchiveBytes       = 16 << 20
	maxFileBytes          = 16 << 20
	maxTotalFileBytes     = 64 << 20
)

var packageFiles = map[string]struct{}{
	"api.json":      {},
	"models.json":   {},
	"catalog.json":  {},
	"schema.json":   {},
	"manifest.json": {},
}

type Release struct {
	Version   string
	Tarball   string
	Integrity string
}

type Package struct {
	Release  Release
	Manifest Manifest
	Files    map[string][]byte
}

type Manifest struct {
	Version       string                  `json:"version"`
	SchemaVersion int                     `json:"schema_version"`
	GeneratedAt   string                  `json:"generated_at"`
	Source        ManifestSource          `json:"source"`
	Files         map[string]ManifestFile `json:"files"`
}

type ManifestSource struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

type ManifestFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Resolver struct {
	Registry   string
	HTTPClient *http.Client
}

func (r Resolver) Resolve(ctx context.Context, version string) (Release, error) {
	if version == "" {
		return Release{}, errors.New("modellink: package version cannot be empty")
	}
	registry := strings.TrimRight(r.Registry, "/")
	if registry == "" {
		registry = DefaultRegistry
	}
	client := r.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	endpoint := registry + "/" + url.PathEscape(PackageName) + "/" + url.PathEscape(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, fmt.Errorf("modellink: create registry request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "modellink-go")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("modellink: query registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("modellink: registry returned %s", resp.Status)
	}

	var metadata struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes))
	if err := decoder.Decode(&metadata); err != nil {
		return Release{}, fmt.Errorf("modellink: decode registry metadata: %w", err)
	}
	if metadata.Version == "" || metadata.Dist.Tarball == "" || metadata.Dist.Integrity == "" {
		return Release{}, errors.New("modellink: registry metadata is incomplete")
	}
	tarballURL, err := url.ParseRequestURI(metadata.Dist.Tarball)
	if err != nil {
		return Release{}, fmt.Errorf("modellink: invalid tarball URL: %w", err)
	}
	if tarballURL.Scheme != "https" && tarballURL.Scheme != "http" {
		return Release{}, errors.New("modellink: tarball URL must use HTTP or HTTPS")
	}
	return Release{
		Version:   metadata.Version,
		Tarball:   metadata.Dist.Tarball,
		Integrity: metadata.Dist.Integrity,
	}, nil
}

func (r Resolver) Download(ctx context.Context, release Release) (*Package, error) {
	client := r.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.Tarball, nil)
	if err != nil {
		return nil, fmt.Errorf("modellink: create tarball request: %w", err)
	}
	req.Header.Set("User-Agent", "modellink-go")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modellink: download package: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modellink: tarball returned %s", resp.Status)
	}

	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("modellink: read package: %w", err)
	}
	if len(archive) > maxArchiveBytes {
		return nil, errors.New("modellink: package archive exceeds size limit")
	}
	if err := verifyIntegrity(archive, release.Integrity); err != nil {
		return nil, err
	}
	files, err := extractFiles(archive)
	if err != nil {
		return nil, err
	}
	manifestBytes, ok := files["manifest.json"]
	if !ok {
		return nil, errors.New("modellink: package is missing manifest.json")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("modellink: decode manifest: %w", err)
	}
	if manifest.Version != release.Version {
		return nil, fmt.Errorf(
			"modellink: manifest version %q does not match release %q",
			manifest.Version,
			release.Version,
		)
	}
	if err := VerifyFiles(manifest, files); err != nil {
		return nil, err
	}
	return &Package{Release: release, Manifest: manifest, Files: files}, nil
}

func VerifyFiles(manifest Manifest, files map[string][]byte) error {
	for _, name := range []string{"api.json", "models.json", "catalog.json", "schema.json"} {
		entry, ok := manifest.Files[name]
		if !ok {
			return fmt.Errorf("modellink: manifest is missing %s", name)
		}
		contents, ok := files[name]
		if !ok {
			return fmt.Errorf("modellink: package is missing %s", name)
		}
		if int64(len(contents)) != entry.Size {
			return fmt.Errorf("modellink: %s size mismatch", name)
		}
		digest := sha256.Sum256(contents)
		if hex.EncodeToString(digest[:]) != entry.SHA256 {
			return fmt.Errorf("modellink: %s SHA-256 mismatch", name)
		}
	}
	return nil
}

func extractFiles(archive []byte) (map[string][]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("modellink: open package gzip stream: %w", err)
	}
	defer gzipReader.Close()

	files := make(map[string][]byte)
	var total int64
	tarchive := tar.NewReader(gzipReader)
	for {
		header, err := tarchive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("modellink: read package tar: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size < 0 || header.Size > maxFileBytes || total+header.Size > maxTotalFileBytes {
			return nil, fmt.Errorf("modellink: package entry %s exceeds size limit", header.Name)
		}
		total += header.Size
		clean := path.Clean(header.Name)
		if !strings.HasPrefix(clean, "package/") {
			continue
		}
		name := strings.TrimPrefix(clean, "package/")
		if _, wanted := packageFiles[name]; !wanted {
			continue
		}
		if _, duplicate := files[name]; duplicate {
			return nil, fmt.Errorf("modellink: duplicate package file %s", name)
		}
		contents, err := io.ReadAll(io.LimitReader(tarchive, header.Size+1))
		if err != nil {
			return nil, fmt.Errorf("modellink: read package file %s: %w", name, err)
		}
		if int64(len(contents)) != header.Size {
			return nil, fmt.Errorf("modellink: package file %s has invalid size", name)
		}
		files[name] = contents
	}
	return files, nil
}

func verifyIntegrity(contents []byte, integrity string) error {
	var selected string
	var priority int
	for _, token := range strings.Fields(integrity) {
		algorithm, _, ok := strings.Cut(token, "-")
		current := map[string]int{"sha256": 1, "sha384": 2, "sha512": 3}[algorithm]
		if ok && current > priority {
			selected = token
			priority = current
		}
	}
	if selected == "" {
		return errors.New("modellink: package integrity has no supported digest")
	}
	algorithm, encoded, _ := strings.Cut(selected, "-")
	expected, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("modellink: decode package integrity: %w", err)
	}
	var digest hash.Hash
	switch algorithm {
	case "sha256":
		digest = sha256.New()
	case "sha384":
		digest = sha512.New384()
	case "sha512":
		digest = sha512.New()
	default:
		return errors.New("modellink: unsupported package integrity algorithm")
	}
	_, _ = digest.Write(contents)
	if subtle.ConstantTimeCompare(digest.Sum(nil), expected) != 1 {
		return errors.New("modellink: package integrity mismatch")
	}
	return nil
}
