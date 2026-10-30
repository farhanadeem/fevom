package images

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

const maxWorkers = 4

const maxBlockOut = 64 << 20

type xzBlock struct {
	data, size int64
	out        int64
	dict       int64
}

var xzMagic = []byte{0xFD, '7', 'z', 'X', 'Z', 0}

func checkSize(id byte) int64 {
	switch {
	case id == 0:
		return 0
	case id <= 3:
		return 4
	case id <= 6:
		return 8
	case id <= 9:
		return 16
	case id <= 12:
		return 32
	default:
		return 64
	}
}

func scanBlocks(r io.ReaderAt, size int64) (blocks []xzBlock, ok bool) {
	if size < 24 {
		return nil, false
	}
	var head, foot [12]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, false
	}
	if _, err := r.ReadAt(foot[:], size-12); err != nil {
		return nil, false
	}
	if !bytes.Equal(head[:6], xzMagic) || head[6] != 0 || head[7]&0xF0 != 0 {
		return nil, false
	}
	if foot[10] != 'Y' || foot[11] != 'Z' || foot[8] != head[6] || foot[9] != head[7] {
		return nil, false
	}
	check := checkSize(head[7] & 0x0F)
	index := size - 12 - (int64(binary.LittleEndian.Uint32(foot[4:8]))+1)*4
	pos := int64(12)
	var hdr [1024]byte
	for pos < index {
		if _, err := r.ReadAt(hdr[:1], pos); err != nil || hdr[0] == 0 {
			return nil, false
		}
		hs := (int64(hdr[0]) + 1) * 4
		if pos+hs > index {
			return nil, false
		}
		if _, err := r.ReadAt(hdr[1:hs], pos+1); err != nil {
			return nil, false
		}
		flags := hdr[1]
		if flags&0x03 != 0 || flags&0x3C != 0 || flags&0x40 == 0 || flags&0x80 == 0 {
			return nil, false
		}
		p := bytes.NewReader(hdr[2 : hs-4])
		csize, err1 := binary.ReadUvarint(p)
		usize, err2 := binary.ReadUvarint(p)
		id, err3 := binary.ReadUvarint(p)
		props, err4 := binary.ReadUvarint(p)
		prop, err5 := p.ReadByte()
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || id != 0x21 || props != 1 || prop > 40 {
			return nil, false
		}
		if csize == 0 || usize == 0 || usize > maxBlockOut || int64(csize) > index {
			return nil, false
		}
		dict := int64(2|(prop&1)) << (prop/2 + 11)
		if prop == 40 {
			dict = 1<<32 - 1
		}
		b := xzBlock{data: pos + hs, size: int64(csize), out: int64(usize), dict: dict}
		pos = b.data + b.size + (4-b.size%4)%4 + check
		if pos > index {
			return nil, false
		}
		blocks = append(blocks, b)
	}
	if pos != index || len(blocks) == 0 {
		return nil, false
	}
	var ind [1]byte
	if _, err := r.ReadAt(ind[:], index); err != nil || ind[0] != 0 {
		return nil, false
	}
	return blocks, true
}

func decodeBlock(r io.ReaderAt, b xzBlock) ([]byte, error) {
	dict := min(b.dict, max(b.out, lzma.MinDictCap))
	in := make([]byte, b.size)
	if _, err := r.ReadAt(in, b.data); err != nil {
		return nil, err
	}
	lr, err := lzma.Reader2Config{DictCap: int(dict)}.NewReader2(bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	out := make([]byte, b.out)
	if _, err := io.ReadFull(lr, out); err != nil {
		return nil, fmt.Errorf("a block decodes to less than its header says: %w", err)
	}
	if n, err := lr.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		return nil, errors.New("a block decodes to more than its header says")
	}
	return out, nil
}

func decompressXZ(ctx context.Context, r io.ReaderAt, size int64, w io.Writer) (int64, bool, error) {
	blocks, ok := scanBlocks(r, size)
	if !ok {
		n, err := streamXZ(ctx, r, size, w)
		return n, false, err
	}
	n, err := parallelXZ(ctx, r, blocks, w, min(maxWorkers, runtime.GOMAXPROCS(0)))
	return n, true, err
}

func streamXZ(ctx context.Context, r io.ReaderAt, size int64, w io.Writer) (int64, error) {
	zr, err := xz.ReaderConfig{SingleStream: false}.NewReader(bufio.NewReaderSize(io.NewSectionReader(r, 0, size), 1<<20))
	if err != nil {
		return 0, err
	}
	return io.Copy(w, ctxReader{ctx, zr})
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func parallelXZ(ctx context.Context, r io.ReaderAt, blocks []xzBlock, w io.Writer, workers int) (int64, error) {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	results := make([]chan result, len(blocks))
	for i := range results {
		results[i] = make(chan result, 1)
	}
	slots := make(chan struct{}, workers)
	go func() {
		for i, b := range blocks {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				out, err := decodeBlock(r, b)
				results[i] <- result{out, err}
			}()
		}
	}()
	var total int64
	for i := range blocks {
		var res result
		select {
		case res = <-results[i]:
		case <-ctx.Done():
			return total, ctx.Err()
		}
		if res.err != nil {
			return total, fmt.Errorf("xz block %d of %d: %w", i+1, len(blocks), res.err)
		}
		n, err := w.Write(res.out)
		total += int64(n)
		<-slots
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
