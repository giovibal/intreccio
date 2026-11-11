package intreccio

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	boltstore "github.com/giovibal/intreccio/internal/storage/bolt"
)

// TestMain lets the whole suite run on another storage engine:
//
//	INTRECCIO_TEST_ENGINE=bolt go test .
//
// Every OpenInMemory then opens a throwaway bbolt file instead of Badger, which
// checks that the query layers depend only on the storage.Store contract.
func TestMain(m *testing.M) {
	switch engine := os.Getenv("INTRECCIO_TEST_ENGINE"); engine {
	case "", "badger":
	case "bolt":
		openMemStore = openTempBolt
	default:
		fmt.Fprintf(os.Stderr, "unknown INTRECCIO_TEST_ENGINE %q (want badger or bolt)\n", engine)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func openTempBolt() (storage.Store, error) {
	dir, err := os.MkdirTemp("", "intreccio-bolt-*")
	if err != nil {
		return nil, err
	}
	s, err := boltstore.OpenWithOptions(filepath.Join(dir, "db.bolt"), boltstore.Options{NoSync: true})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return tempStore{Store: s, dir: dir}, nil
}

// tempStore removes its directory on Close.
type tempStore struct {
	*boltstore.Store
	dir string
}

func (s tempStore) Close() error {
	err := s.Store.Close()
	_ = os.RemoveAll(s.dir)
	return err
}
