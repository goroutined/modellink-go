package modellink

import (
	"fmt"
	"strconv"
	"strings"
)

// WarningCode identifies a non-fatal condition found while loading data.
type WarningCode string

const (
	// WarningSchemaSDKOutdated means the loaded data uses a newer Schema than
	// the one used to generate this SDK's public Go types.
	WarningSchemaSDKOutdated WarningCode = "schema_sdk_outdated"
	// WarningSchemaDataOutdated means the loaded data uses an older Schema than
	// the one embedded in this SDK.
	WarningSchemaDataOutdated WarningCode = "schema_data_outdated"
	// WarningSchemaMismatch means the two Schema hashes differ but their package
	// versions do not establish which copy is newer.
	WarningSchemaMismatch WarningCode = "schema_mismatch"
)

// Warning describes an actionable, non-fatal compatibility condition.
type Warning struct {
	Code    WarningCode
	Message string

	DataPackageVersion     string
	DataSchemaVersion      int
	DataSchemaSHA256       string
	EmbeddedPackageVersion string
	EmbeddedSchemaVersion  int
	EmbeddedSchemaSHA256   string
}

func schemaWarnings(manifest Manifest) []Warning {
	embedded := SchemaInfo()
	dataHash := manifest.Files.SchemaJSON.SHA256
	if manifest.SchemaVersion == embedded.SchemaVersion && dataHash == embedded.SchemaSHA256 {
		return nil
	}

	warning := Warning{
		DataPackageVersion:     manifest.Version,
		DataSchemaVersion:      manifest.SchemaVersion,
		DataSchemaSHA256:       dataHash,
		EmbeddedPackageVersion: embedded.PackageVersion,
		EmbeddedSchemaVersion:  embedded.SchemaVersion,
		EmbeddedSchemaSHA256:   embedded.SchemaSHA256,
	}
	switch {
	case manifest.SchemaVersion < embedded.SchemaVersion:
		warning.Code = WarningSchemaDataOutdated
		warning.Message = fmt.Sprintf(
			"ModelLink data %s uses Schema v%d, while this SDK embeds Schema v%d; update the data package with LoadLatest",
			manifest.Version,
			manifest.SchemaVersion,
			embedded.SchemaVersion,
		)
	case manifest.SchemaVersion > embedded.SchemaVersion:
		// snapshotFromPackage rejects this case before constructing a Snapshot.
		return nil
	default:
		switch comparePackageVersions(manifest.Version, embedded.PackageVersion) {
		case 1:
			warning.Code = WarningSchemaSDKOutdated
			warning.Message = fmt.Sprintf(
				"ModelLink data %s contains a newer compatible Schema than the SDK types generated from data %s; update github.com/goroutined/modellink-go",
				manifest.Version,
				embedded.PackageVersion,
			)
		case -1:
			warning.Code = WarningSchemaDataOutdated
			warning.Message = fmt.Sprintf(
				"ModelLink data %s contains an older Schema than the SDK types generated from data %s; update the data package with LoadLatest",
				manifest.Version,
				embedded.PackageVersion,
			)
		default:
			warning.Code = WarningSchemaMismatch
			warning.Message = fmt.Sprintf(
				"ModelLink data %s and this SDK declare Schema v%d but have different Schema hashes",
				manifest.Version,
				manifest.SchemaVersion,
			)
		}
	}
	return []Warning{warning}
}

func comparePackageVersions(left string, right string) int {
	leftVersion, leftOK := parsePackageVersion(left)
	rightVersion, rightOK := parsePackageVersion(right)
	if !leftOK || !rightOK {
		return 0
	}
	for index := range leftVersion.core {
		if leftVersion.core[index] < rightVersion.core[index] {
			return -1
		}
		if leftVersion.core[index] > rightVersion.core[index] {
			return 1
		}
	}
	if leftVersion.pre == rightVersion.pre {
		return 0
	}
	if leftVersion.pre == "" {
		return 1
	}
	if rightVersion.pre == "" {
		return -1
	}
	return comparePrerelease(leftVersion.pre, rightVersion.pre)
}

type packageVersion struct {
	core [3]int
	pre  string
}

func parsePackageVersion(value string) (packageVersion, bool) {
	value, _, _ = strings.Cut(value, "+")
	core, prerelease, _ := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return packageVersion{}, false
	}
	var parsed packageVersion
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return packageVersion{}, false
		}
		parsed.core[index] = number
	}
	parsed.pre = prerelease
	return parsed, true
}

func comparePrerelease(left string, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		if leftParts[index] == rightParts[index] {
			continue
		}
		leftNumber, leftNumeric := numericIdentifier(leftParts[index])
		rightNumber, rightNumeric := numericIdentifier(rightParts[index])
		switch {
		case leftNumeric && rightNumeric:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case leftParts[index] < rightParts[index]:
			return -1
		default:
			return 1
		}
	}
	if len(leftParts) < len(rightParts) {
		return -1
	}
	if len(leftParts) > len(rightParts) {
		return 1
	}
	return 0
}

func numericIdentifier(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
}
