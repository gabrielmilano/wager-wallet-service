// Package archtest verifica a regra de dependência entre camadas (ADR 0001):
// domain não conhece ninguém, app não conhece adapter e só bootstrap (e cmd/)
// conhece o Fx.
package archtest

import (
	"errors"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/gabrielmilano/wager-wallet-service"

// rule proíbe que os pacotes sob layer importem qualquer pacote sob os
// prefixos de forbidden. Prefixos casam por segmento de caminho inteiro.
type rule struct {
	layer     string
	forbidden []string
}

// Infraestrutura que só a camada adapter (e a composição) pode conhecer.
var infra = []string{
	"go.uber.org/fx",
	"github.com/jackc",
	"github.com/aws",
	"github.com/golang-migrate",
	"net/http",
	"database/sql",
}

var rules = []rule{
	{
		layer: module + "/internal/domain",
		forbidden: append([]string{
			module + "/internal/app",
			module + "/internal/adapter",
			module + "/internal/platform",
			module + "/internal/bootstrap",
		}, infra...),
	},
	{
		layer: module + "/internal/app",
		forbidden: append([]string{
			module + "/internal/adapter",
			module + "/internal/bootstrap",
		}, infra...),
	},
	{
		layer: module + "/internal/platform",
		forbidden: []string{
			module + "/internal/domain",
			module + "/internal/app",
			module + "/internal/adapter",
			module + "/internal/bootstrap",
			"go.uber.org/fx",
		},
	},
	{
		layer: module + "/internal/adapter",
		forbidden: []string{
			module + "/internal/bootstrap",
			"go.uber.org/fx",
		},
	},
}

// under informa se path é prefix ou está abaixo dele
// ("internal/app" não casa com "internal/application").
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// violations devolve os imports de pkg que alguma regra proíbe.
func violations(pkg string, imports []string) []string {
	var found []string
	for _, r := range rules {
		if !under(pkg, r.layer) {
			continue
		}
		for _, imp := range imports {
			for _, f := range r.forbidden {
				if under(imp, f) {
					found = append(found, imp)
				}
			}
		}
	}
	return found
}

func TestViolations(t *testing.T) {
	tests := []struct {
		name    string
		pkg     string
		imports []string
		want    []string
	}{
		{"domain com stdlib", module + "/internal/domain/money", []string{"errors", "strconv"}, nil},
		{"domain importa domain", module + "/internal/domain/wallet", []string{module + "/internal/domain/money"}, nil},
		{"domain importa app", module + "/internal/domain/wallet", []string{module + "/internal/app/store"}, []string{module + "/internal/app/store"}},
		{"domain importa net/http", module + "/internal/domain/money", []string{"net/http"}, []string{"net/http"}},
		{"domain importa pgx", module + "/internal/domain/wager", []string{"github.com/jackc/pgx/v5"}, []string{"github.com/jackc/pgx/v5"}},
		{"app importa adapter", module + "/internal/app/wagering", []string{module + "/internal/adapter/postgres"}, []string{module + "/internal/adapter/postgres"}},
		{"app importa fx", module + "/internal/app/wagering", []string{"go.uber.org/fx"}, []string{"go.uber.org/fx"}},
		{"app importa domain", module + "/internal/app/wagering", []string{module + "/internal/domain/money"}, nil},
		{"adapter importa pgx", module + "/internal/adapter/postgres", []string{"github.com/jackc/pgx/v5"}, nil},
		{"adapter importa bootstrap", module + "/internal/adapter/httpapi", []string{module + "/internal/bootstrap"}, []string{module + "/internal/bootstrap"}},
		{"bootstrap importa fx", module + "/internal/bootstrap", []string{"go.uber.org/fx"}, nil},
		{"prefixo parcial não casa", module + "/internal/domain/money", []string{"net/httptest"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := violations(tt.pkg, tt.imports)
			if !slices.Equal(got, tt.want) {
				t.Errorf("violations(%q, %q) = %q, want %q", tt.pkg, tt.imports, got, tt.want)
			}
		})
	}
}

func TestLayerDependencies(t *testing.T) {
	root := moduleRoot(t)

	// Inclui arquivos com a build tag de integração, para que testes de
	// integração também respeitem as camadas.
	ctx := build.Default
	ctx.BuildTags = append(ctx.BuildTags, "integration")

	checked := 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		p, err := ctx.ImportDir(dir, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil
		}
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		pkg := module + "/" + filepath.ToSlash(rel)
		imports := slices.Concat(p.Imports, p.TestImports, p.XTestImports)
		for _, imp := range violations(pkg, imports) {
			t.Errorf("%s não pode importar %s", pkg, imp)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Protege contra um teste que passa por não ter encontrado nada.
	if checked == 0 {
		t.Fatal("nenhum pacote encontrado em internal/")
	}
}

// moduleRoot sobe a partir do diretório do teste até achar o go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod não encontrado")
		}
		dir = parent
	}
}
