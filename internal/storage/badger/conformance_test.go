package badger

import (
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T, dir string) storage.Store {
		s, err := OpenWithOptions(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
