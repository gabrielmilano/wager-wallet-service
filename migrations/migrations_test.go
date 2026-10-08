package migrations

import (
	"io/fs"
	"regexp"
	"testing"
)

var fileName = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)

// TestEveryUpHasDown garante que toda migration pode ser revertida e que os
// nomes seguem o padrão esperado pelo golang-migrate.
func TestEveryUpHasDown(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}

	type pair struct{ up, down bool }
	versions := map[string]*pair{}
	for _, e := range entries {
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("nome fora do padrão NNNNNN_nome.(up|down).sql: %s", e.Name())
			continue
		}
		key := m[1] + "_" + m[2]
		if versions[key] == nil {
			versions[key] = &pair{}
		}
		if m[3] == "up" {
			versions[key].up = true
		} else {
			versions[key].down = true
		}
	}

	if len(versions) == 0 {
		t.Fatal("nenhuma migration embutida")
	}
	for v, p := range versions {
		if !p.up || !p.down {
			t.Errorf("%s: up=%v down=%v; toda migration precisa dos dois", v, p.up, p.down)
		}
	}
}
