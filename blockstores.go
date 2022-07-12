package main

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ipfs/go-cid"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipld/go-car/v2/blockstore"
)

type ClosableBlockstore interface {
	bstore.Blockstore
	io.Closer
}

var ErrNotFound = errors.New("not found")

func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// ReadWriteBlockstores tracks open ReadWrite CAR blockstores.
type ReadWriteBlockstores struct {
	mu     sync.RWMutex
	stores map[string]*blockstore.ReadWrite
}

func NewReadWriteBlockstores() *ReadWriteBlockstores {
	return &ReadWriteBlockstores{
		stores: make(map[string]*blockstore.ReadWrite),
	}
}

func (r *ReadWriteBlockstores) Get(key string) (*blockstore.ReadWrite, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if bs, ok := r.stores[key]; ok {
		return bs, nil
	}
	return nil, fmt.Errorf("could not get blockstore for key %s: %w", key, ErrNotFound)
}

func (r *ReadWriteBlockstores) GetOrOpen(key string, path string, rootCid cid.Cid) (*blockstore.ReadWrite, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if bs, ok := r.stores[key]; ok {
		return bs, nil
	}

	bs, err := blockstore.OpenReadWrite(path, []cid.Cid{rootCid}, blockstore.UseWholeCIDs(true))
	if err != nil {
		return nil, fmt.Errorf("failed to create read-write blockstore: %w", err)
	}
	r.stores[key] = bs
	return bs, nil
}

func (r *ReadWriteBlockstores) Untrack(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if bs, ok := r.stores[key]; ok {
		// If the blockstore has already been finalized, calling Finalize again
		// will return an error. For our purposes it's simplest if Finalize is
		// idempotent so we just ignore any error.
		_ = bs.Finalize()
	}

	delete(r.stores, key)
	return nil
}
