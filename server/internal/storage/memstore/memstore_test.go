package memstore

import (
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/storagetest"
)

func TestStoreContract(t *testing.T) {
	storagetest.Run(t, func(*testing.T) storage.Store { return New() })
}
