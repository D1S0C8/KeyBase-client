// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"fmt"
	"time"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfsblock"
	"github.com/keybase/kbfs/kbfscodec"
	"github.com/keybase/kbfs/tlf"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

// fileBlockGetter is a function that gets a block suitable for
// reading or writing, and also returns whether the block was already
// dirty.  It may be called from new goroutines, and must handle any
// required locks accordingly.
type fileBlockGetter func(context.Context, KeyMetadata, BlockPointer,
	path, blockReqType) (fblock *FileBlock, wasDirty bool, err error)

// dirtyBlockCacher writes dirty blocks to a cache.
type dirtyBlockCacher func(ptr BlockPointer, block Block) error

// fileData is a helper struct for accessing and manipulating data
// within a file.  It's meant for use within a single scope, not for
// long-term storage.  The caller must ensure goroutine-safety.
type fileData struct {
	file   path
	uid    keybase1.UID
	crypto cryptoPure
	kmd    KeyMetadata
	bsplit BlockSplitter
	getter fileBlockGetter
	cacher dirtyBlockCacher
	log    logger.Logger
}

func newFileData(file path, uid keybase1.UID, crypto cryptoPure,
	bsplit BlockSplitter, kmd KeyMetadata, getter fileBlockGetter,
	cacher dirtyBlockCacher, log logger.Logger) *fileData {
	return &fileData{
		file:   file,
		uid:    uid,
		crypto: crypto,
		bsplit: bsplit,
		kmd:    kmd,
		getter: getter,
		cacher: cacher,
		log:    log,
	}
}

// parentBlockAndChildIndex is a node on a path down the tree to a
// particular leaf node.  `pblock` is an indirect block corresponding
// to one of that leaf node's parents, and `childIndex` is an index
// into `pblock.IPtrs` to the next node along the path.
type parentBlockAndChildIndex struct {
	pblock     *FileBlock
	childIndex int
}

func (pbci parentBlockAndChildIndex) childIPtr() IndirectFilePtr {
	return pbci.pblock.IPtrs[pbci.childIndex]
}

func (fd *fileData) rootBlockPointer() BlockPointer {
	return fd.file.tailPointer()
}

// getFileBlockAtOffset returns the leaf block containing the given
// `off`, along with the set of indirect blocks leading to that leaf
// (if any).
func (fd *fileData) getFileBlockAtOffset(ctx context.Context,
	topBlock *FileBlock, off int64, rtype blockReqType) (
	ptr BlockPointer, parentBlocks []parentBlockAndChildIndex,
	block *FileBlock, nextBlockStartOff, startOff int64,
	wasDirty bool, err error) {
	// Find the block matching the offset, if it exists.
	ptr = fd.rootBlockPointer()
	block = topBlock
	nextBlockStartOff = -1
	startOff = 0

	if !topBlock.IsInd {
		// If it's not an indirect block, we just need to figure out
		// if it's dirty.
		_, wasDirty, err = fd.getter(ctx, fd.kmd, ptr, fd.file, rtype)
		if err != nil {
			return zeroPtr, nil, nil, 0, 0, false, err
		}
		return ptr, nil, block, nextBlockStartOff, startOff, wasDirty, nil
	}

	// Search until it's not an indirect block.
	for block.IsInd {
		nextIndex := len(block.IPtrs) - 1
		for i, iptr := range block.IPtrs {
			if iptr.Off == off {
				// Small optimization to avoid iterating past the correct ptr.
				nextIndex = i
				break
			} else if iptr.Off > off {
				// Use the previous block.  i can never be 0, because
				// the first ptr always has an offset at the beginning
				// of the range.
				nextIndex = i - 1
				break
			}
		}
		nextPtr := block.IPtrs[nextIndex]
		parentBlocks = append(parentBlocks,
			parentBlockAndChildIndex{block, nextIndex})
		startOff = nextPtr.Off
		// There is more to read if we ever took a path through a
		// ptr that wasn't the final ptr in its respective list.
		if nextIndex != len(block.IPtrs)-1 {
			nextBlockStartOff = block.IPtrs[nextIndex+1].Off
		}
		ptr = nextPtr.BlockPointer
		block, wasDirty, err = fd.getter(ctx, fd.kmd, ptr, fd.file, rtype)
		if err != nil {
			return zeroPtr, nil, nil, 0, 0, false, err
		}
	}

	return ptr, parentBlocks, block, nextBlockStartOff, startOff, wasDirty, nil
}

// getNextDirtyFileBlockAtOffset returns the next dirty leaf block
// with a starting offset that is equal or greater than the given
// `off`.  This assumes that any code that dirties a leaf block also
// dirties all of its parents, even if those parents haven't yet
// changed.  It can be used iteratively (by feeding
// `nextBlockStartOff` back in as `off`) to find all the dirty blocks
// of the file.  Note that there is no need to parallelize that
// process, since all the dirty blocks are guaranteed to be local.
func (fd *fileData) getNextDirtyFileBlockAtOffset(ctx context.Context,
	topBlock *FileBlock, off int64, rtype blockReqType,
	dirtyBcache DirtyBlockCache) (
	ptr BlockPointer, parentBlocks []parentBlockAndChildIndex,
	block *FileBlock, nextBlockStartOff, startOff int64,
	err error) {
	// Find the block matching the offset, if it exists.
	ptr = fd.rootBlockPointer()
	if !dirtyBcache.IsDirty(fd.file.Tlf, ptr, fd.file.Branch) {
		// The top block isn't dirty, so we know none of the leaves
		// are dirty.
		return zeroPtr, nil, nil, 0, 0, nil
	}

	block = topBlock
	nextBlockStartOff = -1
	startOff = 0
	// Search along paths of dirty blocks until we find a dirty leaf
	// block with an offset equal or greater than `off`.
	checkedPrevBlock := false
	for block.IsInd {
		index := -1
		for i, iptr := range block.IPtrs {
			if iptr.Off < off {
				continue
			}

			if iptr.Off == off {
				// No need to check the previous block if we align
				// exactly with `off`.
				checkedPrevBlock = true
			}

			// If we haven't checked the previous block yet, do so now
			// since it contains `off`.  We know i can never be 0
			// here, because the first ptr always has an offset at the
			// beginning of the range.
			if !checkedPrevBlock && dirtyBcache.IsDirty(
				fd.file.Tlf, block.IPtrs[i-1].BlockPointer, fd.file.Branch) {
				index = i - 1
				break
			}
			checkedPrevBlock = true

			// Now check the current block.
			if dirtyBcache.IsDirty(
				fd.file.Tlf, block.IPtrs[i].BlockPointer, fd.file.Branch) {
				index = i
				break
			}
		}

		if index == -1 {
			// There's no dirty block at or after `off`.
			return zeroPtr, nil, nil, 0, 0, nil
		}

		iptr := block.IPtrs[index]
		parentBlocks = append(parentBlocks,
			parentBlockAndChildIndex{block, index})
		startOff = iptr.Off
		// There is more to read if we ever took a path through a
		// ptr that wasn't the final ptr in its respective list.
		if index != len(block.IPtrs)-1 {
			nextBlockStartOff = block.IPtrs[index+1].Off
		}
		ptr = iptr.BlockPointer
		block, _, err = fd.getter(ctx, fd.kmd, ptr, fd.file, rtype)
		if err != nil {
			return zeroPtr, nil, nil, 0, 0, err
		}
	}

	return ptr, parentBlocks, block, nextBlockStartOff, startOff, nil
}

// getLeafBlocksForOffsetRange fetches all the leaf ("direct") blocks
// that encompass the given offset range (half-inclusive) in the file.
// If `endOff` is -1, it returns blocks until reaching the end of the
// file.  Note the range could be made up of holes, meaning that the
// last byte of a direct block doesn't immediately precede the first
// byte of the subsequent block.  If `prefixOk` is true, the function
// will ignore context deadline errors and return whatever prefix of
// the data it could fetch within the deadine.  Return params:
//
//   * pathsFromRoot is a slice, ordered by file offset, of paths from
//     the root to each block that makes up the range.  If the path is
//     empty, it indicates that pblock is a direct block and has no
//     children.
//   * blocks: a map from block pointer to a data-containing leaf node
//     in the given range of offsets.
//   * nextBlockOff is the offset of the block that follows the last
//     block given in `pathsFromRoot`.  If `pathsFromRoot` contains
//     the last block among the children, nextBlockOff is -1.
func (fd *fileData) getLeafBlocksForOffsetRange(ctx context.Context,
	ptr BlockPointer, pblock *FileBlock, startOff, endOff int64,
	prefixOk bool) (pathsFromRoot [][]parentBlockAndChildIndex,
	blocks map[BlockPointer]*FileBlock, nextBlockOffset int64, err error) {
	if !pblock.IsInd {
		// Return a single empty path and a child map with only this
		// block in it, under the assumption that the caller already
		// checked the range for this block.
		return [][]parentBlockAndChildIndex{nil},
			map[BlockPointer]*FileBlock{ptr: pblock}, -1, nil
	}

	type resp struct {
		pathsFromRoot   [][]parentBlockAndChildIndex
		blocks          map[BlockPointer]*FileBlock
		nextBlockOffset int64
	}

	// Search all of the in-range child blocks, and their child
	// blocks, etc, in parallel.
	respChans := make([]<-chan resp, 0, len(pblock.IPtrs))
	eg, groupCtx := errgroup.WithContext(ctx)
	nextBlockOffsetThisLevel := int64(-1)
	for i, iptr := range pblock.IPtrs {
		// Some byte of this block is included in the left side of the
		// range if `startOff` is less than the largest byte offset in
		// the block.
		inRangeLeft := true
		if i < len(pblock.IPtrs)-1 {
			inRangeLeft = startOff < pblock.IPtrs[i+1].Off
		}
		if !inRangeLeft {
			continue
		}
		// Some byte of this block is included in the right side of
		// the range if `endOff` is bigger than the smallest byte
		// offset in the block (or if we're explicitly reading all the
		// data to the end).
		inRangeRight := endOff == -1 || endOff > iptr.Off
		if !inRangeRight {
			// This block is the first one past the offset range
			// amount the children.
			nextBlockOffsetThisLevel = iptr.Off
			break
		}

		ptr := iptr.BlockPointer
		childIndex := i
		respCh := make(chan resp, 1)
		respChans = append(respChans, respCh)
		eg.Go(func() error {
			block, _, err := fd.getter(
				groupCtx, fd.kmd, ptr, fd.file, blockReadParallel)
			if err != nil {
				return err
			}
			// Recurse down to the level of the child.
			pfr, blocks, nextBlockOffset, err := fd.getLeafBlocksForOffsetRange(
				groupCtx, ptr, block, startOff, endOff, prefixOk)
			if err != nil {
				return err
			}

			// Append self to the front of every path.
			var r resp
			for _, p := range pfr {
				newPath := append([]parentBlockAndChildIndex{{
					pblock:     pblock,
					childIndex: childIndex,
				}}, p...)
				r.pathsFromRoot = append(r.pathsFromRoot, newPath)
			}
			r.blocks = blocks
			r.nextBlockOffset = nextBlockOffset
			respCh <- r
			return nil
		})
	}

	err = eg.Wait()
	// If we are ok with just getting the prefix, don't treat a
	// deadline exceeded error as fatal.
	if prefixOk && err == context.DeadlineExceeded {
		err = nil
	}
	if err != nil {
		return nil, nil, 0, err
	}

	blocks = make(map[BlockPointer]*FileBlock)
	minNextBlockOffsetChild := int64(-1)
outer:
	for _, respCh := range respChans {
		select {
		case r := <-respCh:
			pathsFromRoot = append(pathsFromRoot, r.pathsFromRoot...)
			for ptr, block := range r.blocks {
				blocks[ptr] = block
			}
			// We want to find the leftmost block offset that's to the
			// right of the range, the one immediately following the
			// end of the range.
			if r.nextBlockOffset != -1 &&
				(minNextBlockOffsetChild == -1 ||
					r.nextBlockOffset < minNextBlockOffsetChild) {
				minNextBlockOffsetChild = r.nextBlockOffset
			}
		default:
			// There should always be a response ready in every
			// channel, unless prefixOk is true.
			if prefixOk {
				break outer
			} else {
				panic("No response ready when !prefixOk")
			}
		}
	}

	// If this level has no offset, or one of the children has an
	// offset that's smaller than the one at this level, use the child
	// offset instead.
	if nextBlockOffsetThisLevel == -1 {
		nextBlockOffset = minNextBlockOffsetChild
	} else if minNextBlockOffsetChild != -1 &&
		minNextBlockOffsetChild < nextBlockOffsetThisLevel {
		nextBlockOffset = minNextBlockOffsetChild
	} else {
		nextBlockOffset = nextBlockOffsetThisLevel
	}

	return pathsFromRoot, blocks, nextBlockOffset, nil
}

func (fd *fileData) getIndirectBlocksForOffsetRange(ctx context.Context,
	pblock *FileBlock, startOff, endOff int64) (
	pathsFromRoot [][]parentBlockAndChildIndex, err error) {
	// TODO: once we have an IsDirect flag, we can avoid fetching the
	// leaf blocks.
	pfr, _, _, err := fd.getLeafBlocksForOffsetRange(
		ctx, fd.rootBlockPointer(), pblock, startOff, endOff, false)
	if err != nil {
		return nil, err
	}

	return pfr, nil
}

// getByteSlicesInOffsetRange returns an ordered, continuous slice of
// byte ranges for the data described by the half-inclusive offset
// range `[startOff, endOff)`.  If `endOff` == -1, it returns data to
// the end of the file.  The caller is responsible for concatenating
// the data into a single buffer if desired. If `prefixOk` is true,
// the function will ignore context deadline errors and return
// whatever prefix of the data it could fetch within the deadine.
func (fd *fileData) getByteSlicesInOffsetRange(ctx context.Context,
	startOff, endOff int64, prefixOk bool) ([][]byte, error) {
	if startOff < 0 || endOff < -1 {
		return nil, fmt.Errorf("Bad offset range [%d, %d)", startOff, endOff)
	} else if endOff != -1 && endOff <= startOff {
		return nil, nil
	}

	topBlock, _, err := fd.getter(ctx, fd.kmd, fd.rootBlockPointer(),
		fd.file, blockRead)
	if err != nil {
		return nil, err
	}

	// Find all the indirect pointers to leaf blocks in the offset range.
	var iptrs []IndirectFilePtr
	firstBlockOff := int64(-1)
	endBlockOff := int64(-1)
	nextBlockOff := int64(-1)
	var blockMap map[BlockPointer]*FileBlock
	if topBlock.IsInd {
		var pfr [][]parentBlockAndChildIndex
		pfr, blockMap, nextBlockOff, err = fd.getLeafBlocksForOffsetRange(
			ctx, fd.rootBlockPointer(), topBlock, startOff, endOff, prefixOk)
		if err != nil {
			return nil, err
		}

		for i, p := range pfr {
			if len(p) == 0 {
				return nil, fmt.Errorf("Unexpected empty path to child for "+
					"file %v", fd.rootBlockPointer())
			}
			lowestAncestor := p[len(p)-1]
			iptr := lowestAncestor.childIPtr()
			iptrs = append(iptrs, iptr)
			if firstBlockOff < 0 {
				firstBlockOff = iptr.Off
			}
			if i == len(pfr)-1 {
				leafBlock := blockMap[iptr.BlockPointer]
				endBlockOff = iptr.Off + int64(len(leafBlock.Contents))
			}
		}
	} else {
		iptrs = []IndirectFilePtr{{
			BlockInfo: BlockInfo{BlockPointer: fd.rootBlockPointer()},
			Off:       0,
		}}
		firstBlockOff = 0
		endBlockOff = int64(len(topBlock.Contents))
		blockMap = map[BlockPointer]*FileBlock{fd.rootBlockPointer(): topBlock}
	}

	if len(iptrs) == 0 {
		return nil, nil
	}

	nRead := int64(0)
	n := endOff - startOff
	if endOff == -1 {
		n = endBlockOff - startOff
	}

	// Grab the relevant byte slices from each block described by the
	// indirect pointer, filling in holes as needed.
	var bytes [][]byte
	for _, iptr := range iptrs {
		block := blockMap[iptr.BlockPointer]
		blockLen := int64(len(block.Contents))
		nextByte := nRead + startOff
		toRead := n - nRead
		blockOff := iptr.Off
		lastByteInBlock := blockOff + blockLen

		if nextByte >= lastByteInBlock {
			if nextBlockOff > 0 {
				fill := nextBlockOff - nextByte
				if fill > toRead {
					fill = toRead
				}
				fd.log.CDebugf(ctx, "Read from hole: nextByte=%d "+
					"lastByteInBlock=%d fill=%d", nextByte, lastByteInBlock,
					fill)
				if fill <= 0 {
					fd.log.CErrorf(ctx,
						"Read invalid file fill <= 0 while reading hole")
					return nil, BadSplitError{}
				}
				bytes = append(bytes, make([]byte, fill))
				nRead += fill
				continue
			}
			return bytes, nil
		} else if toRead > lastByteInBlock-nextByte {
			toRead = lastByteInBlock - nextByte
		}

		// Check for holes in the middle of a file.
		if nextByte < blockOff {
			fill := blockOff - nextByte
			bytes = append(bytes, make([]byte, fill))
			nRead += fill
			nextByte += fill
			toRead -= fill
		}

		firstByteToRead := nextByte - blockOff
		bytes = append(bytes,
			block.Contents[firstByteToRead:toRead+firstByteToRead])
		nRead += toRead
	}

	// If we didn't complete the read and there's another block, then
	// we've hit another hole and need to add a fill.
	if nRead < n && nextBlockOff > 0 {
		toRead := n - nRead
		nextByte := nRead + startOff
		fill := nextBlockOff - nextByte
		if fill > toRead {
			fill = toRead
		}
		fd.log.CDebugf(ctx, "Read from hole at end of file: nextByte=%d "+
			"fill=%d", nextByte, fill)
		if fill <= 0 {
			fd.log.CErrorf(ctx,
				"Read invalid file fill <= 0 while reading hole")
			return nil, BadSplitError{}
		}
		bytes = append(bytes, make([]byte, fill))
	}
	return bytes, nil
}

// The amount that the read timeout is smaller than the global one.
const readTimeoutSmallerBy = 2 * time.Second

// read fills the `dest` buffer with data from the file, starting at
// `startOff`.  Returns the number of bytes copied.  If the read
// operation nears the deadline set in `ctx`, it returns as big a
// prefix as possible before reaching the deadline.
func (fd *fileData) read(ctx context.Context, dest []byte, startOff int64) (
	int64, error) {
	if len(dest) == 0 {
		return 0, nil
	}

	// If we have a large enough timeout add a temporary timeout that is
	// readTimeoutSmallerBy. Use that for reading so short reads get returned
	// upstream without triggering the global timeout.
	now := time.Now()
	deadline, haveTimeout := ctx.Deadline()
	if haveTimeout {
		rem := deadline.Sub(now) - readTimeoutSmallerBy
		if rem > 0 {
			var cancel func()
			ctx, cancel = context.WithTimeout(ctx, rem)
			defer cancel()
		}
	}

	bytes, err := fd.getByteSlicesInOffsetRange(ctx, startOff,
		startOff+int64(len(dest)), true)
	if err != nil {
		return 0, err
	}

	currLen := int64(0)
	for _, b := range bytes {
		bLen := int64(len(b))
		copy(dest[currLen:currLen+bLen], b)
		currLen += bLen
	}
	return currLen, nil
}

// getBytes returns a buffer containing data from the file, in the
// half-inclusive range `[startOff, endOff)`.  If `endOff` == -1, it
// returns data until the end of the file.
func (fd *fileData) getBytes(ctx context.Context, startOff, endOff int64) (
	data []byte, err error) {
	bytes, err := fd.getByteSlicesInOffsetRange(ctx, startOff, endOff, false)
	if err != nil {
		return nil, err
	}

	bufSize := 0
	for _, b := range bytes {
		bufSize += len(b)
	}
	data = make([]byte, bufSize)
	currLen := 0
	for _, b := range bytes {
		copy(data[currLen:currLen+len(b)], b)
		currLen += len(b)
	}

	return data, nil
}

// createIndirectBlock creates a new indirect block and pick a new id
// for the existing block, and use the existing block's ID for the new
// indirect block that becomes the parent.
func (fd *fileData) createIndirectBlock(
	ctx context.Context, df *dirtyFile, dver DataVer) (*FileBlock, error) {
	newID, err := fd.crypto.MakeTemporaryBlockID()
	if err != nil {
		return nil, err
	}
	fblock := &FileBlock{
		CommonBlock: CommonBlock{
			IsInd: true,
		},
		IPtrs: []IndirectFilePtr{
			{
				BlockInfo: BlockInfo{
					BlockPointer: BlockPointer{
						ID:      newID,
						KeyGen:  fd.kmd.LatestKeyGeneration(),
						DataVer: dver,
						Context: kbfsblock.MakeFirstContext(fd.uid),
					},
					EncodedSize: 0,
				},
				Off: 0,
			},
		},
	}

	fd.log.CDebugf(ctx, "Creating new level of indirection for file %v, "+
		"new block id for old top level is %v", fd.rootBlockPointer(), newID)

	// Mark the old block ID as not dirty, so that we will treat the
	// old block ID as newly dirtied in cacheBlockIfNotYetDirtyLocked.
	df.setBlockNotDirty(fd.rootBlockPointer())
	err = fd.cacher(fd.rootBlockPointer(), fblock)
	if err != nil {
		return nil, err
	}

	return fblock, nil
}

// newRightBlock creates space for a new rightmost block, creating
// parent blocks and a new level of indirection in the tree as needed.
// If there's no new level of indirection, it modifies the blocks in
// `parentBlocks` to include the new right-most pointers
// (`parentBlocks` must consist of blocks copied for writing).  It
// also returns the set of parents pointing to the new block (whether
// or not there is a new level of indirection), and also returns any
// newly-dirtied block pointers.
//
// The new block is pointed to using offset `off`, and doesn't have to
// represent an append to the end of the file.  In particular, if
// `off` is less than the offset of its leftmost neighbor, it's the
// caller's responsibility to move the new right block into the
// correct place in the tree (e.g., using `shiftBlocksToFillHole()`).
func (fd *fileData) newRightBlock(
	ctx context.Context, parentBlocks []parentBlockAndChildIndex, off int64,
	df *dirtyFile, dver DataVer) (
	[]parentBlockAndChildIndex, []BlockPointer, error) {
	// Find the lowest block that can accommodate a new right block.
	lowestAncestorWithRoom := -1
	for i := len(parentBlocks) - 1; i >= 0; i-- {
		pb := parentBlocks[i]
		if len(pb.pblock.IPtrs) < fd.bsplit.MaxPtrsPerBlock() {
			lowestAncestorWithRoom = i
			break
		}
	}

	var newTopBlock *FileBlock
	var newDirtyPtrs []BlockPointer
	if lowestAncestorWithRoom < 0 {
		// Create a new level of indirection at the top.
		var err error
		newTopBlock, err = fd.createIndirectBlock(ctx, df, dver)
		if err != nil {
			return nil, nil, err
		}

		// The old top block needs to be cached under its new ID if it
		// was indirect.
		if len(parentBlocks) > 0 {
			ptr := newTopBlock.IPtrs[0].BlockPointer
			err = fd.cacher(ptr, parentBlocks[0].pblock)
			if err != nil {
				return nil, nil, err
			}
			newDirtyPtrs = append(newDirtyPtrs, ptr)
		}

		parentBlocks = append([]parentBlockAndChildIndex{{newTopBlock, 0}},
			parentBlocks...)
		lowestAncestorWithRoom = 0
	}
	rightParentBlocks := make([]parentBlockAndChildIndex, len(parentBlocks))

	fd.log.CDebugf(ctx, "Making new right block at off %d for file %v, "+
		"lowestAncestor at level %d", off, fd.rootBlockPointer(),
		lowestAncestorWithRoom)

	// Make a new right block for every parent, starting with the
	// lowest ancestor with room.  Note that we're not iterating over
	// the actual parent blocks here; we're only using its length to
	// figure out how many levels need new blocks.
	pblock := parentBlocks[lowestAncestorWithRoom].pblock
	for i := lowestAncestorWithRoom; i < len(parentBlocks); i++ {
		newRID, err := fd.crypto.MakeTemporaryBlockID()
		if err != nil {
			return nil, nil, err
		}

		newPtr := BlockPointer{
			ID:      newRID,
			KeyGen:  fd.kmd.LatestKeyGeneration(),
			DataVer: dver,
			Context: kbfsblock.MakeFirstContext(fd.uid),
		}

		fd.log.CDebugf(ctx, "New right block for file %d, level %d, ptr %v",
			fd.rootBlockPointer(), i, newPtr)

		pblock.IPtrs = append(pblock.IPtrs, IndirectFilePtr{
			BlockInfo: BlockInfo{
				BlockPointer: newPtr,
				EncodedSize:  0,
			},
			Off: off,
		})
		rightParentBlocks[i].pblock = pblock
		rightParentBlocks[i].childIndex = len(pblock.IPtrs) - 1

		rblock := &FileBlock{}
		if i != len(parentBlocks)-1 {
			rblock.IsInd = true
			pblock = rblock
		}

		err = fd.cacher(newPtr, rblock)
		if err != nil {
			return nil, nil, err
		}

		newDirtyPtrs = append(newDirtyPtrs, newPtr)
	}

	// All parents up to and including the lowest ancestor with room
	// will have to change, so mark them as dirty.
	ptr := fd.rootBlockPointer()
	for i := 0; i <= lowestAncestorWithRoom; i++ {
		pb := parentBlocks[i]
		if err := fd.cacher(ptr, pb.pblock); err != nil {
			return nil, nil, err
		}
		newDirtyPtrs = append(newDirtyPtrs, ptr)
		ptr = pb.childIPtr().BlockPointer
		rightParentBlocks[i].pblock = pb.pblock
		rightParentBlocks[i].childIndex = len(pb.pblock.IPtrs) - 1
	}

	return rightParentBlocks, newDirtyPtrs, nil
}

// shiftBlocksToFillHole should be called after newRightBlock when the
// offset for the new block is smaller than the size of the file.
// This happens when there is a hole in the file, and the user is now
// writing data into that hole.  This function moves the new block
// into the correct place, and rearranges all the indirect pointers in
// the file as needed.  It returns any block pointers that were
// dirtied in the process.
func (fd *fileData) shiftBlocksToFillHole(
	ctx context.Context, newTopBlock *FileBlock,
	parents []parentBlockAndChildIndex) (
	newDirtyPtrs []BlockPointer, err error) {

	// `parents` should represent the right side of the tree down to
	// the new rightmost indirect pointer, the offset of which should
	// match `newHoleStartOff`.  Keep swapping it with its sibling on
	// the left until its offset would be lower than that child's
	// offset.  If there are no children to the left, continue on with
	// the children in the cousin block to the left.  If we swap a
	// child between cousin blocks, we must update the offset in the
	// right cousin's parent block.  If *that* updated pointer is the
	// leftmost pointer in its parent block, update that one as well,
	// up to the root.
	//
	// We are guaranteed at least one level of indirection because
	// `newRightBlock` should have been called before
	// `shiftBlocksToFillHole`.
	immedParent := parents[len(parents)-1]
	currIndex := immedParent.childIndex
	newBlockStartOff := immedParent.childIPtr().Off

	fd.log.CDebugf(ctx, "Shifting block with offset %d for file %v into "+
		"position", newBlockStartOff, fd.rootBlockPointer())

	// Swap left as needed.
	for {
		var leftOff int64
		var newParents []parentBlockAndChildIndex
		if currIndex > 0 {
			leftOff = immedParent.pblock.IPtrs[currIndex-1].Off
		} else {
			// Construct the new set of parents for the shifted block,
			// by looking for the next left cousin.
			newParents = make([]parentBlockAndChildIndex, len(parents))
			copy(newParents, parents)
			var level int
			for level = len(newParents) - 2; level >= 0; level-- {
				// The parent at the level being evaluated has a left
				// sibling, so we use that sibling.
				if newParents[level].childIndex > 0 {
					break
				}
				// Keep going up until we find a way back down a left branch.
			}

			if level < 0 {
				// We are already all the way on the left, we're done!
				return newDirtyPtrs, nil
			}
			newParents[level].childIndex--

			// Walk back down, shifting the new parents into position.
			for ; level < len(newParents)-1; level++ {
				nextChildPtr := newParents[level].childIPtr()
				childBlock, _, err := fd.getter(
					ctx, fd.kmd, nextChildPtr.BlockPointer, fd.file, blockWrite)
				if err != nil {
					return nil, err
				}

				newParents[level+1].pblock = childBlock
				newParents[level+1].childIndex = len(childBlock.IPtrs) - 1
				leftOff = childBlock.IPtrs[len(childBlock.IPtrs)-1].Off
			}
		}

		// We're done!
		if leftOff < newBlockStartOff {
			return newDirtyPtrs, nil
		}

		// Otherwise, we need to swap the indirect file pointers.
		if currIndex > 0 {
			immedParent.pblock.IPtrs[currIndex-1],
				immedParent.pblock.IPtrs[currIndex] =
				immedParent.pblock.IPtrs[currIndex],
				immedParent.pblock.IPtrs[currIndex-1]
			currIndex--
			continue
		}

		// Swap block pointers across cousins at the lowest level of
		// indirection.
		newImmedParent := newParents[len(newParents)-1]
		newCurrIndex := len(newImmedParent.pblock.IPtrs) - 1
		newImmedParent.pblock.IPtrs[newCurrIndex],
			immedParent.pblock.IPtrs[currIndex] =
			immedParent.pblock.IPtrs[currIndex],
			newImmedParent.pblock.IPtrs[newCurrIndex]

		// Cache the new immediate parent as dirty.
		if len(newParents) > 1 {
			i := len(newParents) - 2
			iptr := newParents[i].childIPtr()
			if err := fd.cacher(
				iptr.BlockPointer, newImmedParent.pblock); err != nil {
				return nil, err
			}
			newDirtyPtrs = append(newDirtyPtrs, iptr.BlockPointer)
		}

		// Now we need to update the parent offsets on the right side,
		// all the way up to the common ancestor (which is the one
		// with the one that doesn't have a childIndex of 0).  (We
		// don't need to update the left side, since the offset of the
		// new right-most block on that side doesn't affect the
		// incoming indirect pointer offset, which already points to
		// the left side of that branch.)
		newRightOff := immedParent.pblock.IPtrs[currIndex].Off
		for level := len(parents) - 2; level >= 0; level-- {
			// Cache the block below this level, which was just
			// modified.
			childPtr := parents[level].childIPtr()
			if err := fd.cacher(childPtr.BlockPointer,
				parents[level+1].pblock); err != nil {
				return nil, err
			}
			newDirtyPtrs = append(newDirtyPtrs, childPtr.BlockPointer)

			// If we've reached a level where the child indirect
			// offset wasn't affected, we're done.  If not, update the
			// offset at this level and move up the tree.
			if parents[level+1].childIndex > 0 {
				break
			}
			index := parents[level].childIndex
			parents[level].pblock.IPtrs[index].Off = newRightOff
		}
		immedParent = newImmedParent
		currIndex = newCurrIndex
		parents = newParents
	}
	// The loop above must exit via one of the returns.
}

// markParentsDirty caches all the blocks in `parentBlocks` as dirty,
// and returns the dirtied block pointers as well as any block infos
// with non-zero encoded sizes that will now need to be unreferenced.
func (fd *fileData) markParentsDirty(ctx context.Context,
	parentBlocks []parentBlockAndChildIndex) (
	dirtyPtrs []BlockPointer, unrefs []BlockInfo, err error) {
	parentPtr := fd.rootBlockPointer()
	for _, pb := range parentBlocks {
		if err := fd.cacher(parentPtr, pb.pblock); err != nil {
			return nil, unrefs, err
		}
		dirtyPtrs = append(dirtyPtrs, parentPtr)
		parentPtr = pb.childIPtr().BlockPointer

		// Remember the size of each newly-dirtied child.
		if pb.childIPtr().EncodedSize != 0 {
			unrefs = append(unrefs, pb.childIPtr().BlockInfo)
			pb.pblock.IPtrs[pb.childIndex].EncodedSize = 0
		}
	}
	return dirtyPtrs, unrefs, nil
}

// write sets the given data and the given offset within the file,
// making new blocks and new levels of indirection as needed. Return
// params:
// * newDe: a new directory entry with the EncodedSize cleared if the file
//   was extended.
// * dirtyPtrs: a slice of the BlockPointers that have been dirtied during
//   the write.  This includes any interior indirect blocks that may not
//   have been changed yet, but which will need to change as part of the
//   sync process because of leaf node changes below it.
// * unrefs: a slice of BlockInfos that must be unreferenced as part of an
//   eventual sync of this write.  May be non-nil even if err != nil.
// * newlyDirtiedChildBytes is the total amount of block data dirtied by this
//   write, including the entire size of blocks that have had at least one
//   byte dirtied.  As above, it may be non-zero even if err != nil.
// * bytesExtended is the number of bytes the length of the file has been
//   extended as part of this write.
func (fd *fileData) write(ctx context.Context, data []byte, off int64,
	topBlock *FileBlock, oldDe DirEntry, df *dirtyFile) (
	newDe DirEntry, dirtyPtrs []BlockPointer, unrefs []BlockInfo,
	newlyDirtiedChildBytes int64, bytesExtended int64, err error) {
	n := int64(len(data))
	nCopied := int64(0)
	oldSizeWithoutHoles := oldDe.Size
	newDe = oldDe

	fd.log.CDebugf(ctx, "Writing %d bytes at off %d", n, off)

	dirtyMap := make(map[BlockPointer]bool)
	for nCopied < n {
		ptr, parentBlocks, block, nextBlockOff, startOff, wasDirty, err :=
			fd.getFileBlockAtOffset(ctx, topBlock, off+nCopied, blockWrite)
		if err != nil {
			return newDe, nil, unrefs, newlyDirtiedChildBytes, 0, err
		}

		oldLen := len(block.Contents)

		// Take care not to write past the beginning of the next block
		// by using max.
		max := len(data)
		if nextBlockOff > 0 {
			if room := int(nextBlockOff - off); room < max {
				max = room
			}
		}
		oldNCopied := nCopied
		nCopied += fd.bsplit.CopyUntilSplit(
			block, nextBlockOff < 0, data[nCopied:max], off+nCopied-startOff)

		// If we need another block but there are no more, then make one.
		switchToIndirect := false
		if nCopied < n {
			needExtendFile := nextBlockOff < 0
			needFillHole := off+nCopied < nextBlockOff
			newBlockOff := startOff + int64(len(block.Contents))
			if nCopied == 0 {
				if newBlockOff < off {
					// We are writing somewhere inside a hole, not right
					// at the start of it; all we have done so far it
					// reached the end of an existing block (possibly
					// zero-filling it out to its capacity).  Make sure
					// the next block starts right at the offset we care
					// about.
					newBlockOff = off
				}
			} else if newBlockOff != off+nCopied {
				return newDe, nil, unrefs, newlyDirtiedChildBytes, 0,
					fmt.Errorf("Copied %d bytes, but newBlockOff=%d does not "+
						"match off=%d plus new bytes",
						nCopied, newBlockOff, off)
			}
			var rightParents []parentBlockAndChildIndex
			if needExtendFile || needFillHole {
				// Make a new right block and update the parent's
				// indirect block list, adding a level of indirection
				// if needed.  If we're just filling a hole, the block
				// will end up all the way to the right of the range,
				// and its offset will be smaller than the block to
				// its left -- we'll fix that up below.
				var newDirtyPtrs []BlockPointer
				fd.log.CDebugf(ctx, "Making new right block at nCopied=%d, "+
					"newBlockOff=%d", nCopied, newBlockOff)
				wasIndirect := topBlock.IsInd
				rightParents, newDirtyPtrs, err = fd.newRightBlock(
					ctx, parentBlocks, newBlockOff, df,
					DefaultNewBlockDataVersion(false))
				if err != nil {
					return newDe, nil, unrefs, newlyDirtiedChildBytes, 0, err
				}
				topBlock = rightParents[0].pblock
				for _, p := range newDirtyPtrs {
					dirtyMap[p] = true
				}
				if topBlock.IsInd != wasIndirect {
					// The whole direct data block needs to be
					// re-uploaded as a child block with a new block
					// pointer, so below we'll need to track the dirty
					// bytes of the direct block and cache the block
					// as dirty.  (Note that currently we don't track
					// dirty bytes for indirect blocks.)
					switchToIndirect = true
					ptr = topBlock.IPtrs[0].BlockPointer
				}
			}
			// If we're filling a hole, swap the new right block into
			// the hole and shift everything else over.
			if needFillHole {
				newDirtyPtrs, err := fd.shiftBlocksToFillHole(
					ctx, topBlock, rightParents)
				if err != nil {
					return newDe, nil, unrefs, newlyDirtiedChildBytes, 0, err
				}
				for _, p := range newDirtyPtrs {
					dirtyMap[p] = true
				}
				if oldSizeWithoutHoles == oldDe.Size {
					// For the purposes of calculating the newly-dirtied
					// bytes for the deferral calculation, disregard the
					// existing "hole" in the file.
					oldSizeWithoutHoles = uint64(newBlockOff)
				}
			}
		}

		// Nothing was copied, no need to dirty anything.  This can
		// happen when trying to append to the contents of the file
		// (i.e., either to the end of the file or right before the
		// "hole"), and the last block is already full.
		if nCopied == oldNCopied && !switchToIndirect {
			continue
		}

		// Only in the last block does the file size grow.
		if oldLen != len(block.Contents) && nextBlockOff < 0 {
			newDe.EncodedSize = 0
			// update the file info
			newDe.Size += uint64(len(block.Contents) - oldLen)
		}

		// Calculate the amount of bytes we've newly-dirtied as part
		// of this write.
		newlyDirtiedChildBytes += int64(len(block.Contents))
		if wasDirty {
			newlyDirtiedChildBytes -= int64(oldLen)
		}

		newDirtyPtrs, newUnrefs, err := fd.markParentsDirty(ctx, parentBlocks)
		unrefs = append(unrefs, newUnrefs...)
		if err != nil {
			return newDe, nil, unrefs, newlyDirtiedChildBytes, 0, err
		}
		for _, p := range newDirtyPtrs {
			dirtyMap[p] = true
		}

		// keep the old block ID while it's dirty
		if err = fd.cacher(ptr, block); err != nil {
			return newDe, nil, unrefs, newlyDirtiedChildBytes, 0, err
		}
		dirtyMap[ptr] = true
	}

	if topBlock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any write to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the dirtyFiles map.
		if err = fd.cacher(fd.rootBlockPointer(), topBlock); err != nil {
			return newDe, nil, unrefs, newlyDirtiedChildBytes, 0, err
		}
		dirtyMap[fd.rootBlockPointer()] = true
	}

	lastByteWritten := off + int64(len(data)) // not counting holes
	bytesExtended = 0
	if lastByteWritten > int64(oldSizeWithoutHoles) {
		bytesExtended = lastByteWritten - int64(oldSizeWithoutHoles)
	}

	dirtyPtrs = make([]BlockPointer, 0, len(dirtyMap))
	for p := range dirtyMap {
		dirtyPtrs = append(dirtyPtrs, p)
	}

	return newDe, dirtyPtrs, unrefs, newlyDirtiedChildBytes, bytesExtended, nil
}

// truncateExtend increases file size to the given size by appending
// a "hole" to the file. Return params:
// * newDe: a new directory entry with the EncodedSize cleared.
// * dirtyPtrs: a slice of the BlockPointers that have been dirtied during
//   the truncate.
func (fd *fileData) truncateExtend(ctx context.Context, size uint64,
	topBlock *FileBlock, parentBlocks []parentBlockAndChildIndex,
	oldDe DirEntry, df *dirtyFile) (
	newDe DirEntry, dirtyPtrs []BlockPointer, err error) {
	fd.log.CDebugf(ctx, "truncateExtend: extending file %v to size %d",
		fd.rootBlockPointer(), size)
	switchToIndirect := !topBlock.IsInd
	oldTopBlock := topBlock
	if switchToIndirect {
		fd.log.CDebugf(ctx, "truncateExtend: making block indirect %v",
			fd.rootBlockPointer())
	}

	rightParents, newDirtyPtrs, err := fd.newRightBlock(
		ctx, parentBlocks, int64(size), df,
		DefaultNewBlockDataVersion(true))
	if err != nil {
		return DirEntry{}, nil, err
	}
	topBlock = rightParents[0].pblock

	if switchToIndirect {
		topBlock.IPtrs[0].Holes = true
		err = fd.cacher(topBlock.IPtrs[0].BlockPointer, oldTopBlock)
		if err != nil {
			return DirEntry{}, nil, err
		}
		dirtyPtrs = append(dirtyPtrs, topBlock.IPtrs[0].BlockPointer)
		fd.log.CDebugf(ctx, "truncateExtend: new zero data block %v",
			topBlock.IPtrs[0].BlockPointer)
	}
	dirtyPtrs = append(dirtyPtrs, newDirtyPtrs...)
	newDe = oldDe
	newDe.EncodedSize = 0
	// update the file info
	newDe.Size = size

	// Mark all for presence of holes, one would be enough,
	// but this is more robust and easy.
	for i := range topBlock.IPtrs {
		topBlock.IPtrs[i].Holes = true
	}
	// Always make the top block dirty, so we will sync its
	// indirect blocks.  This has the added benefit of ensuring
	// that any write to a file while it's being sync'd will be
	// deferred, even if it's to a block that's not currently
	// being sync'd, since this top-most block will always be in
	// the fileBlockStates map.
	err = fd.cacher(fd.rootBlockPointer(), topBlock)
	if err != nil {
		return DirEntry{}, nil, err
	}
	dirtyPtrs = append(dirtyPtrs, fd.rootBlockPointer())
	return newDe, dirtyPtrs, nil
}

// truncateShrink shrinks the file to the given size. Return params:
// * newDe: a new directory entry with the EncodedSize cleared if the file
//   shrunk.
// * dirtyPtrs: a slice of the BlockPointers that have been dirtied during
//   the truncate.  This includes any interior indirect blocks that may not
//   have been changed yet, but which will need to change as part of the
//   sync process because of leaf node changes below it.
// * unrefs: a slice of BlockInfos that must be unreferenced as part of an
//   eventual sync of this write.  May be non-nil even if err != nil.
// * newlyDirtiedChildBytes is the total amount of block data dirtied by this
//   truncate, including the entire size of blocks that have had at least one
//   byte dirtied.  As above, it may be non-zero even if err != nil.
func (fd *fileData) truncateShrink(ctx context.Context, size uint64,
	topBlock *FileBlock, oldDe DirEntry) (
	newDe DirEntry, dirtyPtrs []BlockPointer, unrefs []BlockInfo,
	newlyDirtiedChildBytes int64, err error) {
	iSize := int64(size) // TODO: deal with overflow

	ptr, parentBlocks, block, nextBlockOff, startOff, wasDirty, err :=
		fd.getFileBlockAtOffset(ctx, topBlock, iSize, blockWrite)
	if err != nil {
		return DirEntry{}, nil, nil, 0, err
	}

	oldLen := len(block.Contents)
	// We need to delete some data (and possibly entire blocks).  Note
	// we make a new slice and copy data in order to make sure the
	// data being truncated can be fully garbage-collected.
	block.Contents = append([]byte(nil), block.Contents[:iSize-startOff]...)

	newlyDirtiedChildBytes = int64(len(block.Contents))
	if wasDirty {
		newlyDirtiedChildBytes -= int64(oldLen) // negative
	}

	// Need to mark the parents dirty before calling
	// `getIndirectBlocksForOffsetRange`, so that function will see
	// the new copies when fetching the blocks.
	newDirtyPtrs, newUnrefs, err := fd.markParentsDirty(ctx, parentBlocks)
	unrefs = append(unrefs, newUnrefs...)
	if err != nil {
		return DirEntry{}, nil, unrefs, newlyDirtiedChildBytes, err
	}
	dirtyMap := make(map[BlockPointer]bool)
	for _, p := range newDirtyPtrs {
		dirtyMap[p] = true
	}

	if nextBlockOff > 0 {
		// TODO: remove any unnecessary levels of indirection if the
		// number of leaf nodes shrinks significantly (KBFS-1824).

		// Get all paths to any leaf nodes following the new
		// right-most block, since those blocks need to be
		// unreferenced, and their parents need to be modified or
		// unreferenced.
		pfr, err := fd.getIndirectBlocksForOffsetRange(
			ctx, topBlock, nextBlockOff, -1)
		if err != nil {
			return DirEntry{}, nil, nil, 0, err
		}

		// A map from a pointer to an indirect block -> that block's
		// original set of block pointers, before they are truncated
		// in the loop below.  It also tracks which pointed-to blocks
		// have already been processed.
		savedChildPtrs := make(map[BlockPointer][]IndirectFilePtr)
		for _, path := range pfr {
			// parentInfo points to pb.pblock in the loop below.  The
			// initial case is the top block, for which we need to
			// fake a block info using just the root block pointer.
			parentInfo := BlockInfo{BlockPointer: fd.rootBlockPointer()}
			leftMost := true
			for i, pb := range path {
				ptrs := savedChildPtrs[parentInfo.BlockPointer]
				if ptrs == nil {
					// Process each block exactly once, removing all
					// now-unnecessary indirect pointers (but caching
					// that list so we can still walk the tree on the
					// next iterations).
					pblock := pb.pblock
					ptrs = pblock.IPtrs
					savedChildPtrs[parentInfo.BlockPointer] = ptrs

					// Remove the first child iptr and everything
					// following it if all the child indices below
					// this level are 0.
					removeStartingFromIndex := pb.childIndex
					for j := i + 1; j < len(path); j++ {
						if path[j].childIndex > 0 {
							removeStartingFromIndex++
							break
						}
					}

					// If we remove iptr 0, this block can be
					// unreferenced (unless it's on the left-most edge
					// of the tree, in which case we keep it around
					// for now -- see above TODO).
					if pb.childIndex == 0 && !leftMost {
						if parentInfo.EncodedSize != 0 {
							unrefs = append(unrefs, parentInfo)
						}
					} else if removeStartingFromIndex < len(pblock.IPtrs) {
						// Make sure we're modifying a copy of the
						// block by fetching it again with blockWrite.
						// We do this instead of calling DeepCopy in
						// case the a copy of the block has already
						// been made and put into the dirty
						// cache. (e.g., in a previous iteration of
						// this loop).
						pblock, _, err = fd.getter(
							ctx, fd.kmd, parentInfo.BlockPointer, fd.file,
							blockWrite)
						if err != nil {
							return DirEntry{}, nil, nil,
								newlyDirtiedChildBytes, err
						}
						pblock.IPtrs = pblock.IPtrs[:removeStartingFromIndex]
						err = fd.cacher(parentInfo.BlockPointer, pblock)
						if err != nil {
							return DirEntry{}, nil, nil,
								newlyDirtiedChildBytes, err
						}
						dirtyMap[parentInfo.BlockPointer] = true
					}
				}

				// Down to the next level.  If we've hit the leaf
				// level, unreference the block.
				parentInfo = ptrs[pb.childIndex].BlockInfo
				if i == len(path)-1 && parentInfo.EncodedSize != 0 {
					unrefs = append(unrefs, parentInfo)
				} else if pb.childIndex > 0 {
					leftMost = false
				}
			}
		}
	}

	if topBlock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any truncate to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the dirtyFiles map.
		if err = fd.cacher(fd.rootBlockPointer(), topBlock); err != nil {
			return DirEntry{}, nil, nil, newlyDirtiedChildBytes, err
		}
		dirtyMap[fd.rootBlockPointer()] = true
	}

	newDe = oldDe
	newDe.EncodedSize = 0
	newDe.Size = size

	// Keep the old block ID while it's dirty.
	if err = fd.cacher(ptr, block); err != nil {
		return DirEntry{}, nil, nil, newlyDirtiedChildBytes, err
	}
	dirtyMap[ptr] = true

	dirtyPtrs = make([]BlockPointer, 0, len(dirtyMap))
	for p := range dirtyMap {
		dirtyPtrs = append(dirtyPtrs, p)
	}

	return newDe, dirtyPtrs, unrefs, newlyDirtiedChildBytes, nil
}

// split, if given an indirect top block of a file, checks whether any
// of the dirty leaf blocks in that file need to be split up
// differently (i.e., if the BlockSplitter is using
// fingerprinting-based boundaries).  It returns the set of blocks
// that now need to be unreferenced.
func (fd *fileData) split(ctx context.Context, id tlf.ID,
	dirtyBcache DirtyBlockCache, topBlock *FileBlock, df *dirtyFile) (
	unrefs []BlockInfo, err error) {
	if !topBlock.IsInd {
		return nil, nil
	}

	// For an indirect file:
	//   1) check if each dirty block is split at the right place.
	//   2) if it needs fewer bytes, prepend the extra bytes to the next
	//      block (making a new one if it doesn't exist), and the next block
	//      gets marked dirty
	//   3) if it needs more bytes, then use copyUntilSplit() to fetch bytes
	//      from the next block (if there is one), remove the copied bytes
	//      from the next block and mark it dirty
	//   4) Then go through once more, and ready and finalize each
	//      dirty block, updating its ID in the indirect pointer list
	off := int64(0)
	for off >= 0 {
		_, parentBlocks, block, nextBlockOff, startOff, err :=
			fd.getNextDirtyFileBlockAtOffset(
				ctx, topBlock, off, blockWrite, dirtyBcache)
		if err != nil {
			return unrefs, err
		}

		if block == nil {
			// No more dirty blocks.
			break
		}
		off = nextBlockOff // Will be -1 if there are no more blocks.

		splitAt := fd.bsplit.CheckSplit(block)
		switch {
		case splitAt == 0:
			continue
		case splitAt > 0:
			endOfBlock := startOff + int64(len(block.Contents))
			extraBytes := block.Contents[splitAt:]
			block.Contents = block.Contents[:splitAt]
			// put the extra bytes in front of the next block
			if nextBlockOff < 0 {
				// Need to make a new block.
				if _, _, err := fd.newRightBlock(
					ctx, parentBlocks, endOfBlock, df,
					DefaultNewBlockDataVersion(false)); err != nil {
					return unrefs, err
				}
			}
			rPtr, rParentBlocks, rblock, _, _, _, err :=
				fd.getFileBlockAtOffset(
					ctx, topBlock, endOfBlock, blockWrite)
			if err != nil {
				return unrefs, err
			}
			rblock.Contents = append(extraBytes, rblock.Contents...)
			if err = fd.cacher(rPtr, rblock); err != nil {
				return unrefs, err
			}
			endOfBlock = startOff + int64(len(block.Contents))

			// Mark the old rblock as unref'd.
			pb := rParentBlocks[len(rParentBlocks)-1]
			unrefs = append(unrefs, pb.childIPtr().BlockInfo)
			pb.pblock.IPtrs[pb.childIndex].EncodedSize = 0

			// Update parent pointer offsets as needed.
			for i := len(rParentBlocks) - 1; i >= 0; i-- {
				pb := rParentBlocks[i]
				pb.pblock.IPtrs[pb.childIndex].Off = endOfBlock
				// If this isn't the leftmost child at this level,
				// there's no need to update the parent.
				if pb.childIndex > 0 {
					break
				}
			}

			_, newUnrefs, err := fd.markParentsDirty(ctx, rParentBlocks)
			unrefs = append(unrefs, newUnrefs...)
			if err != nil {
				return unrefs, err
			}
			off = endOfBlock
		case splitAt < 0:
			if nextBlockOff < 0 {
				// End of the line.
				continue
			}

			endOfBlock := startOff + int64(len(block.Contents))
			rPtr, rParentBlocks, rblock, _, _, _, err :=
				fd.getFileBlockAtOffset(
					ctx, topBlock, endOfBlock, blockWrite)
			if err != nil {
				return unrefs, err
			}
			// Copy some of that block's data into this block.
			nCopied := fd.bsplit.CopyUntilSplit(block, false,
				rblock.Contents, int64(len(block.Contents)))
			rblock.Contents = rblock.Contents[nCopied:]
			endOfBlock = startOff + int64(len(block.Contents))

			// Make the old right block as unref'd.
			pb := rParentBlocks[len(rParentBlocks)-1]
			unrefs = append(unrefs, pb.childIPtr().BlockInfo)
			pb.pblock.IPtrs[pb.childIndex].EncodedSize = 0

			// For the right block, adjust offset or delete as needed.
			if len(rblock.Contents) > 0 {
				if err = fd.cacher(rPtr, rblock); err != nil {
					return unrefs, err
				}

				// Update parent pointer offsets as needed.
				for i := len(rParentBlocks) - 1; i >= 0; i-- {
					pb := rParentBlocks[i]
					pb.pblock.IPtrs[pb.childIndex].Off = endOfBlock
					// If this isn't the leftmost child at this level,
					// there's no need to update the parent.
					if pb.childIndex > 0 {
						break
					}
				}
			} else {
				// TODO: If we're down to just one leaf block at this
				// level, remove the layer of indirection (KBFS-1824).
				iptrs := pb.pblock.IPtrs
				pb.pblock.IPtrs =
					append(iptrs[:pb.childIndex], iptrs[pb.childIndex+1:]...)
			}

			// Mark all parents as dirty.
			_, newUnrefs, err := fd.markParentsDirty(ctx, rParentBlocks)
			unrefs = append(unrefs, newUnrefs...)
			if err != nil {
				return unrefs, err
			}

			off = endOfBlock
		}
	}
	return unrefs, nil
}

// ready, if given an indirect top-block, readies all the dirty child
// blocks, and updates their block IDs in their parent block's list of
// indirect pointers.  It returns a map pointing from the new block
// info from any readied block to its corresponding old block pointer.
func (fd *fileData) ready(ctx context.Context, id tlf.ID, bcache BlockCache,
	dirtyBcache DirtyBlockCache, bops BlockOps, bps *blockPutState,
	topBlock *FileBlock, df *dirtyFile) (map[BlockInfo]BlockPointer, error) {
	if !topBlock.IsInd {
		return nil, nil
	}

	oldPtrs := make(map[BlockInfo]BlockPointer)
	// This will contain paths to all dirty leaf paths.  The final
	// entry index in each path will be the leaf node block itself
	// (with a -1 child index).
	var dirtyLeafPaths [][]parentBlockAndChildIndex

	// Gather all the paths to all dirty leaf blocks first.
	off := int64(0)
	for off >= 0 {
		_, parentBlocks, block, nextBlockOff, _, err :=
			fd.getNextDirtyFileBlockAtOffset(
				ctx, topBlock, off, blockWrite, dirtyBcache)
		if err != nil {
			return nil, err
		}

		if block == nil {
			// No more dirty blocks.
			break
		}
		off = nextBlockOff // Will be -1 if there are no more blocks.

		dirtyLeafPaths = append(dirtyLeafPaths,
			append(parentBlocks, parentBlockAndChildIndex{block, -1}))
	}

	// No dirty blocks means nothing to do.
	if len(dirtyLeafPaths) == 0 {
		return nil, nil
	}

	// Now starting from the leaf level, ready each block at each
	// level, and put the new BlockInfo into the parent block at the
	// level above.  At each level, only ready each block once. Don't
	// ready the root block though; the folderBranchOps Sync code will
	// do that.
	for level := len(dirtyLeafPaths[0]) - 1; level > 0; level-- {
		newPtrs := make(map[BlockPointer]bool)
		for i := 0; i < len(dirtyLeafPaths); i++ {
			// Ready the dirty block.
			pb := dirtyLeafPaths[i][level]

			parentPB := dirtyLeafPaths[i][level-1]
			ptr := parentPB.childIPtr().BlockPointer
			// If this is already a new pointer, skip it.
			if newPtrs[ptr] {
				continue
			}

			newInfo, _, readyBlockData, err := ReadyBlock(
				ctx, bcache, bops, fd.crypto, fd.kmd, pb.pblock, fd.uid)
			if err != nil {
				return nil, err
			}

			err = bcache.Put(
				newInfo.BlockPointer, id, pb.pblock, PermanentEntry)
			if err != nil {
				return nil, err
			}

			// Only the leaf level need to be tracked by the dirty file.
			var syncFunc func() error
			if level == len(dirtyLeafPaths[0])-1 {
				syncFunc = func() error { return df.setBlockSynced(ptr) }
			}

			bps.addNewBlock(
				newInfo.BlockPointer, pb.pblock, readyBlockData, syncFunc)

			parentPB.pblock.IPtrs[parentPB.childIndex].BlockInfo = newInfo
			oldPtrs[newInfo] = ptr
		}
	}
	return oldPtrs, nil
}

func (fd *fileData) getIndirectFileBlockInfosWithTopBlock(ctx context.Context,
	topBlock *FileBlock) (
	[]BlockInfo, error) {
	// TODO: handle multiple levels of indirection.
	if !topBlock.IsInd {
		return nil, nil
	}
	blockInfos := make([]BlockInfo, len(topBlock.IPtrs))
	for i, ptr := range topBlock.IPtrs {
		blockInfos[i] = ptr.BlockInfo
	}
	return blockInfos, nil
}

func (fd *fileData) getIndirectFileBlockInfos(ctx context.Context) (
	[]BlockInfo, error) {
	topBlock, _, err := fd.getter(
		ctx, fd.kmd, fd.rootBlockPointer(), fd.file, blockRead)
	if err != nil {
		return nil, err
	}
	return fd.getIndirectFileBlockInfosWithTopBlock(ctx, topBlock)
}

// findIPtrsAndClearSize looks for the given indirect pointer, and
// returns whether it could be found.  As a side effect, it also
// clears the encoded size for that indirect pointer.
func (fd *fileData) findIPtrsAndClearSize(topBlock *FileBlock,
	ptr BlockPointer) (found bool) {
	// TODO: handle multiple levels of indirection.  To make that
	// efficient, we may need to pass in more information about which
	// internal indirect pointers may have dirty child blocks.
	for i, iptr := range topBlock.IPtrs {
		if iptr.BlockPointer == ptr {
			topBlock.IPtrs[i].EncodedSize = 0
			return true
		}
	}
	return false
}

// deepCopy makes a complete copy of this file, deduping leaf blocks
// and making new random BlockPointers for all indirect blocks.  It
// returns the new top pointer of the copy, and all the new child
// pointers in the copy.
func (fd *fileData) deepCopy(ctx context.Context, codec kbfscodec.Codec,
	dataVer DataVer) (
	newTopPtr BlockPointer, allChildPtrs []BlockPointer, err error) {
	topBlock, _, err := fd.getter(ctx, fd.kmd, fd.rootBlockPointer(),
		fd.file, blockRead)
	if err != nil {
		return zeroPtr, nil, err
	}

	newTopBlock, err := topBlock.DeepCopy(codec)
	if err != nil {
		return zeroPtr, nil, err
	}
	newTopPtr = fd.rootBlockPointer()
	if topBlock.IsInd {
		newID, err := fd.crypto.MakeTemporaryBlockID()
		if err != nil {
			return zeroPtr, nil, err
		}
		newTopPtr = BlockPointer{
			ID:      newID,
			KeyGen:  fd.kmd.LatestKeyGeneration(),
			DataVer: dataVer,
			Context: kbfsblock.MakeFirstContext(fd.uid),
		}
	} else {
		newTopPtr.RefNonce, err = fd.crypto.MakeBlockRefNonce()
		if err != nil {
			return zeroPtr, nil, err
		}
		newTopPtr.SetWriter(fd.uid)
	}
	fd.log.CDebugf(ctx, "Deep copying file %s: %v -> %v",
		fd.file.tailName(), fd.rootBlockPointer(), newTopPtr)

	// Dup all of the leaf blocks.
	// TODO: deal with multiple levels of indirection.
	if topBlock.IsInd {
		for i, iptr := range topBlock.IPtrs {
			// Generate a new nonce for each one.
			iptr.RefNonce, err = fd.crypto.MakeBlockRefNonce()
			if err != nil {
				return zeroPtr, nil, err
			}
			iptr.SetWriter(fd.uid)
			newTopBlock.IPtrs[i] = iptr
			allChildPtrs = append(allChildPtrs, iptr.BlockPointer)
		}
	}

	if err = fd.cacher(newTopPtr, newTopBlock); err != nil {
		return zeroPtr, nil, err
	}

	return newTopPtr, allChildPtrs, nil
}

// undupChildrenInCopy takes a top block that's been copied via
// deepCopy(), and un-deduplicates all leaf children of the block.  It
// adds all child blocks to the provided `bps`, including both the
// ones that were deduplicated and the ones that weren't (TODO).  It
// returns the BlockInfos for all children, including ones that
// weren't de-duplicated.
func (fd *fileData) undupChildrenInCopy(ctx context.Context,
	bcache BlockCache, bops BlockOps, bps *blockPutState,
	topBlock *FileBlock) ([]BlockInfo, error) {
	if !topBlock.IsInd {
		return nil, nil
	}

	// TODO: support multiple levels of indirection.
	// TODO: parallelize
	blockInfos := make([]BlockInfo, len(topBlock.IPtrs))
	for i, iptr := range topBlock.IPtrs {
		if iptr.RefNonce == kbfsblock.ZeroRefNonce {
			// No need to un-dup mid-level indirect blocks.  TODO:
			// fetch the blocks and add them to the bps (only needed
			// for multiple levels of indirection).
			blockInfos = append(blockInfos, iptr.BlockInfo)
			continue
		}
		childBlock, _, err := fd.getter(ctx, fd.kmd, iptr.BlockPointer,
			fd.file, blockRead)
		if err != nil {
			return nil, err
		}

		newInfo, _, readyBlockData, err :=
			ReadyBlock(ctx, bcache, bops, fd.crypto, fd.kmd, childBlock, fd.uid)
		topBlock.IPtrs[i].BlockInfo = newInfo
		bps.addNewBlock(newInfo.BlockPointer, childBlock, readyBlockData, nil)
		blockInfos = append(blockInfos, newInfo)
	}
	return blockInfos, nil
}
