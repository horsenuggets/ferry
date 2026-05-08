package client

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Chunker reads a local file and yields fixed-size byte windows. Supports
// SeekTo for resume after a 409 / HEAD-discovered offset.
//
// Not safe for concurrent use; the upload loop is single-threaded.
type Chunker struct {
	f         *os.File
	size      int64
	chunkSize int64
	offset    int64 // bytes already consumed by Next() returns
}

// NewChunker opens path read-only and prepares a chunker for chunkSize-byte
// PATCH bodies. Caller must Close.
func NewChunker(path string, chunkSize int64) (*Chunker, error) {
	if chunkSize < 1 {
		return nil, fmt.Errorf("chunk size must be >= 1, got %d", chunkSize)
	}
	if chunkSize > MaxChunkSizeBytes {
		return nil, fmt.Errorf("chunk size %d exceeds max %d", chunkSize, MaxChunkSizeBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat file: %w", err)
	}
	return &Chunker{
		f:         f,
		size:      st.Size(),
		chunkSize: chunkSize,
	}, nil
}

// Size returns the total file size in bytes.
func (c *Chunker) Size() int64 { return c.size }

// Offset returns the byte offset of the next Next() call.
func (c *Chunker) Offset() int64 { return c.offset }

// ChunkSize returns the configured chunk size.
func (c *Chunker) ChunkSize() int64 { return c.chunkSize }

// Done reports whether all bytes have been consumed.
func (c *Chunker) Done() bool { return c.offset >= c.size }

// SeekTo repositions the chunker to absolute file offset n. Used to resume
// after a 409 / HEAD response told us the server is at a different offset.
func (c *Chunker) SeekTo(n int64) error {
	if n < 0 || n > c.size {
		return fmt.Errorf("seek out of range: %d (size=%d)", n, c.size)
	}
	if _, err := c.f.Seek(n, io.SeekStart); err != nil {
		return fmt.Errorf("seek file: %w", err)
	}
	c.offset = n
	return nil
}

// ReaderAt returns an io.Reader that yields exactly length bytes starting
// at absolute file offset start. The chunker's logical offset is updated to
// start so subsequent SeekTo / Done calls reflect the new position.
//
// Used by the upload loop where each retry attempt needs to re-read from a
// known offset (the server's current Upload-Offset, possibly advanced by a
// previous attempt's partial write).
func (c *Chunker) ReaderAt(start, length int64) (io.Reader, error) {
	if start < 0 || start > c.size {
		return nil, fmt.Errorf("start out of range: %d (size=%d)", start, c.size)
	}
	if length < 0 || start+length > c.size {
		return nil, fmt.Errorf("length out of range: start=%d length=%d size=%d", start, length, c.size)
	}
	if _, err := c.f.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek file: %w", err)
	}
	c.offset = start
	return io.LimitReader(c.f, length), nil
}

// Next returns the next chunk: a bounded io.Reader, the absolute starting
// offset of those bytes in the file, and the byte length the reader will
// produce. Advances internal offset by length so subsequent Next calls move
// forward. Returns (_, _, _, io.EOF) if the file is fully consumed.
func (c *Chunker) Next() (io.Reader, int64, int64, error) {
	if c.offset >= c.size {
		return nil, 0, 0, io.EOF
	}
	remaining := c.size - c.offset
	length := c.chunkSize
	if remaining < length {
		length = remaining
	}
	startOffset := c.offset
	r := io.LimitReader(c.f, length)
	c.offset += length
	return r, startOffset, length, nil
}

// Close releases the underlying file. Safe to call multiple times.
func (c *Chunker) Close() error {
	if c.f == nil {
		return nil
	}
	err := c.f.Close()
	c.f = nil
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
