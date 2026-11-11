package bolt

import (
	"path/filepath"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T, dir string) storage.Store {
		s, err := OpenWithOptions(filepath.Join(dir, "db.bolt"), Options{NoSync: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
