package blockstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/exp/mmap"

	"github.com/multiformats/go-varint"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/ipld/go-car/v2/index"
	"github.com/ipld/go-car/v2/internal/carv1"
	"github.com/ipld/go-car/v2/internal/carv1/util"
	internalio "github.com/ipld/go-car/v2/internal/io"
)

var _ blockstore.Blockstore = (*ReadOnly)(nil)

// ReadOnly provides a read-only CAR Block Store.
type ReadOnly struct {
	// mu allows ReadWrite to be safe for concurrent use.
	// It's in ReadOnly so that read operations also grab read locks,
	// given that ReadWrite embeds ReadOnly for methods like Get and Has.
	//
	// The main fields guarded by the mutex are the index and the underlying writers.
	// For simplicity, the entirety of the blockstore methods grab the mutex.
	mu sync.RWMutex

	// The backing containing the CAR in v1 format.
	backing io.ReaderAt
	// The CAR v1 content index.
	idx index.Index

	// If we called carv2.NewReaderMmap, remember to close it too.
	carv2Closer io.Closer

	ropts carv2.ReadOptions
}

// UseWholeCIDs is a read option which makes a CAR blockstore identify blocks by
// whole CIDs, and not just their multihashes. The default is to use
// multihashes, which matches the current semantics of go-ipfs-blockstore v1.
//
// Enabling this option affects a number of methods, including read-only ones:
//
// • Get, Has, and HasSize will only return a block
// only if the entire CID is present in the CAR file.
//
// • DeleteBlock will delete a block only when the entire CID matches.
//
// • AllKeysChan will return the original whole CIDs, instead of with their
// multicodec set to "raw" to just provide multihashes.
//
// • If AllowDuplicatePuts isn't set,
// Put and PutMany will deduplicate by the whole CID,
// allowing different CIDs with equal multihashes.
//
// Note that this option only affects the blockstore, and is ignored by the root
// go-car/v2 package.
func UseWholeCIDs(enable bool) carv2.ReadOption {
	return func(o *carv2.ReadOptions) {
		// TODO: update methods like Get, Has, and AllKeysChan to obey this.
		o.BlockstoreUseWholeCIDs = enable
	}
}

// NewReadOnly creates a new ReadOnly blockstore from the backing with a optional index as idx.
// This function accepts both CAR v1 and v2 backing.
// The blockstore is instantiated with the given index if it is not nil.
//
// Otherwise:
// * For a CAR v1 backing an index is generated.
// * For a CAR v2 backing an index is only generated if Header.HasIndex returns false.
//
// There is no need to call ReadOnly.Close on instances returned by this function.
func NewReadOnly(backing io.ReaderAt, idx index.Index, opts ...carv2.ReadOption) (*ReadOnly, error) {
	b := &ReadOnly{}
	for _, opt := range opts {
		opt(&b.ropts)
	}

	version, err := readVersion(backing)
	if err != nil {
		return nil, err
	}
	switch version {
	case 1:
		if idx == nil {
			if idx, err = generateIndex(backing, opts...); err != nil {
				return nil, err
			}
		}
		b.backing = backing
		b.idx = idx
		return b, nil
	case 2:
		v2r, err := carv2.NewReader(backing, opts...)
		if err != nil {
			return nil, err
		}
		if idx == nil {
			if v2r.Header.HasIndex() {
				idx, err = index.ReadFrom(v2r.IndexReader())
				if err != nil {
					return nil, err
				}
			} else if idx, err = generateIndex(v2r.CarV1Reader(), opts...); err != nil {
				return nil, err
			}
		}
		b.backing = v2r.CarV1Reader()
		b.idx = idx
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported car version: %v", version)
	}
}

func readVersion(at io.ReaderAt) (uint64, error) {
	var rr io.Reader
	switch r := at.(type) {
	case io.Reader:
		rr = r
	default:
		rr = internalio.NewOffsetReadSeeker(r, 0)
	}
	return carv2.ReadVersion(rr)
}

func generateIndex(at io.ReaderAt, opts ...carv2.ReadOption) (index.Index, error) {
	var rs io.ReadSeeker
	switch r := at.(type) {
	case io.ReadSeeker:
		rs = r
	default:
		rs = internalio.NewOffsetReadSeeker(r, 0)
	}
	return carv2.GenerateIndex(rs, opts...)
}

// OpenReadOnly opens a read-only blockstore from a CAR file (either v1 or v2), generating an index if it does not exist.
// Note, the generated index if the index does not exist is ephemeral and only stored in memory.
// See car.GenerateIndex and Index.Attach for persisting index onto a CAR file.
func OpenReadOnly(path string, opts ...carv2.ReadOption) (*ReadOnly, error) {
	f, err := mmap.Open(path)
	if err != nil {
		return nil, err
	}

	robs, err := NewReadOnly(f, nil, opts...)
	if err != nil {
		return nil, err
	}
	robs.carv2Closer = f

	return robs, nil
}

func (b *ReadOnly) readBlock(idx int64) (cid.Cid, []byte, error) {
	bcid, data, err := util.ReadNode(internalio.NewOffsetReadSeeker(b.backing, idx))
	return bcid, data, err
}

// DeleteBlock is unsupported and always returns an error.
func (b *ReadOnly) DeleteBlock(_ cid.Cid) error {
	panic("called write method on a read-only blockstore")
}

// Has indicates if the store contains a block that corresponds to the given key.
func (b *ReadOnly) Has(key cid.Cid) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	offset, err := b.idx.Get(key)
	if errors.Is(err, index.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	uar := internalio.NewOffsetReadSeeker(b.backing, int64(offset))
	_, err = varint.ReadUvarint(uar)
	if err != nil {
		return false, err
	}
	_, c, err := cid.CidFromReader(uar)
	if err != nil {
		return false, err
	}
	return bytes.Equal(key.Hash(), c.Hash()), nil
}

// Get gets a block corresponding to the given key.
func (b *ReadOnly) Get(key cid.Cid) (blocks.Block, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	offset, err := b.idx.Get(key)
	if err != nil {
		if err == index.ErrNotFound {
			err = blockstore.ErrNotFound
		}
		return nil, err
	}
	entry, data, err := b.readBlock(int64(offset))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(key.Hash(), entry.Hash()) {
		return nil, blockstore.ErrNotFound
	}
	return blocks.NewBlockWithCid(data, key)
}

// GetSize gets the size of an item corresponding to the given key.
func (b *ReadOnly) GetSize(key cid.Cid) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	idx, err := b.idx.Get(key)
	if err != nil {
		return -1, err
	}
	rdr := internalio.NewOffsetReadSeeker(b.backing, int64(idx))
	sectionLen, err := varint.ReadUvarint(rdr)
	if err != nil {
		return -1, blockstore.ErrNotFound
	}
	cidLen, readCid, err := cid.CidFromReader(rdr)
	if err != nil {
		return 0, err
	}
	if !readCid.Equals(key) {
		return -1, blockstore.ErrNotFound
	}
	return int(sectionLen) - cidLen, err
}

// Put is not supported and always returns an error.
func (b *ReadOnly) Put(blocks.Block) error {
	panic("called write method on a read-only blockstore")
}

// PutMany is not supported and always returns an error.
func (b *ReadOnly) PutMany([]blocks.Block) error {
	panic("called write method on a read-only blockstore")
}

// AllKeysChan returns the list of keys in the CAR.
func (b *ReadOnly) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	// We release the lock when the channel-sending goroutine stops.
	b.mu.RLock()

	// TODO we may use this walk for populating the index, and we need to be able to iterate keys in this way somewhere for index generation. In general though, when it's asked for all keys from a blockstore with an index, we should iterate through the index when possible rather than linear reads through the full car.
	rdr := internalio.NewOffsetReadSeeker(b.backing, 0)
	header, err := carv1.ReadHeader(rdr)
	if err != nil {
		return nil, fmt.Errorf("error reading car header: %w", err)
	}
	headerSize, err := carv1.HeaderSize(header)
	if err != nil {
		return nil, err
	}

	// TODO: document this choice of 5, or use simpler buffering like 0 or 1.
	ch := make(chan cid.Cid, 5)

	// Seek to the end of header.
	if _, err = rdr.Seek(int64(headerSize), io.SeekStart); err != nil {
		return nil, err
	}

	go func() {
		defer b.mu.RUnlock()
		defer close(ch)

		for {
			length, err := varint.ReadUvarint(rdr)
			if err != nil {
				return // TODO: log this error
			}

			// Null padding; by default it's an error.
			if length == 0 {
				if b.ropts.ZeroLegthSectionAsEOF {
					break
				} else {
					return // TODO: log this error
					// return fmt.Errorf("carv1 null padding not allowed by default; see WithZeroLegthSectionAsEOF")
				}
			}

			thisItemForNxt := rdr.Offset()
			_, c, err := cid.CidFromReader(rdr)
			if err != nil {
				return // TODO: log this error
			}
			if _, err := rdr.Seek(thisItemForNxt+int64(length), io.SeekStart); err != nil {
				return // TODO: log this error
			}

			select {
			case ch <- c:
			case <-ctx.Done():
				// TODO: log ctx error
				return
			}
		}
	}()
	return ch, nil
}

// HashOnRead is currently unimplemented; hashing on reads never happens.
func (b *ReadOnly) HashOnRead(bool) {
	// TODO: implement before the final release?
}

// Roots returns the root CIDs of the backing CAR.
func (b *ReadOnly) Roots() ([]cid.Cid, error) {
	header, err := carv1.ReadHeader(internalio.NewOffsetReadSeeker(b.backing, 0))
	if err != nil {
		return nil, fmt.Errorf("error reading car header: %w", err)
	}
	return header.Roots, nil
}

// Close closes the underlying reader if it was opened by OpenReadOnly.
func (b *ReadOnly) Close() error {
	if b.carv2Closer != nil {
		return b.carv2Closer.Close()
	}
	return nil
}
