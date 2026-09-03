package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/goroutined/modellink-go/internal/atomicfile"
)

const generatorVersion = "v0.22.0"

func main() {
	root, err := os.Getwd()
	check(err)
	temporary, err := os.MkdirTemp("", "modellink-go-schema-*")
	check(err)
	defer os.RemoveAll(temporary)

	generated := filepath.Join(temporary, "zz_types_generated.go")
	command := exec.Command(
		"go",
		"run",
		"github.com/atombender/go-jsonschema@"+generatorVersion,
		"--only-models",
		"--tags",
		"json",
		"--capitalization",
		"API",
		"--capitalization",
		"CN",
		"--capitalization",
		"ID",
		"--capitalization",
		"JSON",
		"--capitalization",
		"NPM",
		"--capitalization",
		"SHA",
		"--capitalization",
		"URL",
		"--package",
		"modellink",
		"--output",
		generated,
		schemaArgument(),
	)
	command.Dir = root
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	check(command.Run())

	source, err := os.ReadFile(generated)
	check(err)
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, generated, source, parser.ParseComments)
	check(err)
	check(replaceStructField(parsed, "ProviderModel", "Interleaved", &ast.StarExpr{
		X: ast.NewIdent("Interleaved"),
	}))
	check(replaceStructField(parsed, "Manifest", "SchemaVersion", ast.NewIdent("int")))

	var output bytes.Buffer
	check(format.Node(&output, files, parsed))
	check(atomicfile.Write(filepath.Join(root, "zz_types_generated.go"), output.Bytes(), 0o644))
}

// go-jsonschema parses its schema argument as a URL before treating it as a
// file. A Windows absolute path such as C:\\... is therefore misread as an
// unsupported URL scheme. Keep the argument relative and anchor it with
// command.Dir instead.
func schemaArgument() string {
	return filepath.Join("schema", "schema.json")
}

func replaceStructField(
	file *ast.File,
	structName string,
	fieldName string,
	replacement ast.Expr,
) error {
	for _, declaration := range file.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, specification := range generic.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != structName {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return fmt.Errorf("%s is not a struct", structName)
			}
			for _, field := range structure.Fields.List {
				if len(field.Names) == 1 && field.Names[0].Name == fieldName {
					field.Type = replacement
					return nil
				}
			}
			return fmt.Errorf("field %s.%s was not generated", structName, fieldName)
		}
	}
	return fmt.Errorf("struct %s was not generated", structName)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
