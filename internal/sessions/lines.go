package sessions

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

const (
	maxLine = 64 << 20
	// maxZstdWindow bounds what one crafted file can make a worker allocate.
	maxZstdWindow = 64 << 20
)

// eachLine calls fn for each complete line in the bytes f holds now. The size
// is taken once, so a fast writer can never keep the read going. A last line
// with no newline is still being written, so it is counted and not parsed.
// line is only valid during the call to fn.
func eachLine(f *os.File, st *Stats, fn func(line []byte)) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return scanLines(io.LimitReader(f, info.Size()), st, fn)
}

// eachZstLine is eachLine for a compressed rollout.
func eachZstLine(f *os.File, st *Stats, fn func(line []byte)) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	z, err := zstd.NewReader(io.LimitReader(f, info.Size()), zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(maxZstdWindow))
	if err != nil {
		return err
	}
	defer z.Close()
	return scanLines(z, st, fn)
}

func scanLines(src io.Reader, st *Stats, fn func(line []byte)) error {
	r := bufio.NewReaderSize(src, 1<<20)
	var long []byte // holds a line that outgrew the reader's buffer
	dropping := false
	for {
		part, err := r.ReadSlice('\n')
		if !dropping && len(long)+len(part) > maxLine {
			st.OversizeLines++
			dropping = true
			long = long[:0]
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if !dropping {
				long = append(long, part...)
			}
			continue
		case errors.Is(err, io.EOF):
			if !dropping && len(long)+len(part) > 0 {
				st.PartialLastLines++
			}
			return nil
		case err != nil:
			return err
		}
		line := part
		if len(long) > 0 {
			long = append(long, part...)
			line = long
		}
		if line = bytes.TrimSpace(line); len(line) > 0 && !dropping {
			st.Lines++
			fn(line)
		}
		long = long[:0]
		dropping = false
	}
}
