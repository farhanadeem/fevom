package k3spayload

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var assetName = regexp.MustCompile(`embed/([0-9a-f]{64})\.tar\.zst`)

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

func PayloadHash(wrapper string) (string, error) {
	f, err := os.Open(wrapper)
	if err != nil {
		return "", err
	}
	defer f.Close()
	names := map[string]bool{}
	const window, overlap = 4 << 20, 128
	buf := make([]byte, window+overlap)
	carry := 0
	for {
		n, err := io.ReadFull(f, buf[carry:])
		chunk := buf[:carry+n]
		for _, m := range assetName.FindAllSubmatch(chunk, -1) {
			names[string(m[1])] = true
		}
		if err != nil {
			break
		}
		carry = copy(buf, chunk[len(chunk)-overlap:])
	}
	if len(names) != 1 {
		return "", fmt.Errorf("%s names %d k3s payloads (embed/HASH.tar.zst), not one", wrapper, len(names))
	}
	for h := range names {
		return h, nil
	}
	return "", nil
}

func findAsset(r io.ReaderAt, size int64, hash string) (int64, int64, error) {
	sr := io.NewSectionReader(r, 0, size)
	br := bufio.NewReaderSize(sr, 1<<20)
	var pos int64
	var tail []byte
	for {
		chunk := make([]byte, 1<<20)
		n, err := io.ReadFull(br, chunk)
		chunk = append(tail, chunk[:n]...)
		base := pos - int64(len(tail))
		for off := 0; ; {
			i := bytes.Index(chunk[off:], zstdMagic)
			if i < 0 {
				break
			}
			start := base + int64(off+i)
			off += i + 1
			if end, ok := chainHashes(r, size, start, hash); ok {
				return start, end, nil
			}
		}
		pos += int64(n)
		if err != nil {
			break
		}
		tail = append([]byte(nil), chunk[len(chunk)-3:]...)
	}
	return 0, 0, fmt.Errorf("no zstd stream in the binary hashes to %s", hash)
}

func chainHashes(r io.ReaderAt, size, start int64, hash string) (int64, bool) {
	h := sha256.New()
	end := start
	for {
		n, err := frameLen(r, size, end)
		if err != nil {
			return 0, false
		}
		if _, err := io.Copy(h, io.NewSectionReader(r, end, n)); err != nil {
			return 0, false
		}
		end += n
		if hex.EncodeToString(h.Sum(nil)) == hash {
			return end, true
		}
		var m [4]byte
		if end+4 > size {
			return 0, false
		}
		if _, err := r.ReadAt(m[:], end); err != nil || !bytes.Equal(m[:], zstdMagic) {
			return 0, false
		}
	}
}

func frameLen(r io.ReaderAt, size, off int64) (int64, error) {
	var hdr [18]byte
	if off+6 > size {
		return 0, errors.New("no frame")
	}
	n, _ := r.ReadAt(hdr[:], off)
	if n < 6 || binary.LittleEndian.Uint32(hdr[:]) != 0xFD2FB528 {
		return 0, errors.New("no frame")
	}
	fhd := hdr[4]
	fcsFlag := fhd >> 6
	single := fhd&0x20 != 0
	checksum := fhd&0x04 != 0
	didFlag := fhd & 3
	if fhd&0x08 != 0 {
		return 0, errors.New("reserved bit")
	}
	p := int64(5)
	if !single {
		p++
	}
	p += []int64{0, 1, 2, 4}[didFlag]
	fcs := []int64{0, 2, 4, 8}[fcsFlag]
	if fcsFlag == 0 && single {
		fcs = 1
	}
	p += fcs
	for {
		var bh [3]byte
		if off+p+3 > size {
			return 0, errors.New("short")
		}
		if _, err := r.ReadAt(bh[:], off+p); err != nil {
			return 0, err
		}
		h := uint32(bh[0]) | uint32(bh[1])<<8 | uint32(bh[2])<<16
		p += 3
		last := h&1 != 0
		typ := (h >> 1) & 3
		bsize := int64(h >> 3)
		switch typ {
		case 0, 2:
			p += bsize
		case 1:
			p++
		case 3:
			return 0, errors.New("reserved block")
		}
		if off+p > size {
			return 0, errors.New("short")
		}
		if last {
			break
		}
	}
	if checksum {
		p += 4
	}
	if off+p > size {
		return 0, errors.New("short")
	}
	return p, nil
}

type Extracted struct {
	Hash         string
	Compressed   int64
	Files, Links int
	CNILinks     int
}

func Extract(wrapper, dest, root string) (Extracted, error) {
	return ExtractWith(wrapper, dest, root, Options{Check: true})
}

type Options struct {
	Hash  string
	Check bool
}

func ExtractWith(wrapper, dest, root string, o Options) (Extracted, error) {
	hash := o.Hash
	if hash == "" {
		var err error
		if hash, err = PayloadHash(wrapper); err != nil {
			return Extracted{}, err
		}
	}
	out := Extracted{Hash: hash}
	data := filepath.Join(dest, "data")
	dir := filepath.Join(data, hash)
	if Verified(dest, hash) {
		return out, nil
	}
	f, err := os.Open(wrapper)
	if err != nil {
		return out, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	start, end, err := findAsset(f, st.Size(), hash)
	if err != nil {
		return out, err
	}
	out.Compressed = end - start
	if hash == "" || len(hash) != 64 {
		return out, fmt.Errorf("the payload's hash %q", hash)
	}
	tmp := dir + "-tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return out, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return out, err
	}
	if err := untar(io.NewSectionReader(f, start, end-start), tmp); err != nil {
		os.RemoveAll(tmp)
		return out, err
	}
	if o.Check {
		if out.Files, out.Links, err = verify(filepath.Join(tmp, "bin")); err != nil {
			os.RemoveAll(tmp)
			return out, err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return out, err
	}
	gdir := filepath.Join(root, "data", hash)
	if err := replaceLink(gdir, filepath.Join(data, "current")); err != nil {
		return out, err
	}
	cni := filepath.Join(data, "cni")
	if err := os.RemoveAll(cni); err != nil {
		return out, err
	}
	if err := os.MkdirAll(cni, 0o755); err != nil {
		return out, err
	}
	gcni := filepath.Join(gdir, "bin", "cni")
	if err := os.Symlink(gcni, filepath.Join(cni, "cni")); err != nil {
		return out, err
	}
	ents, err := os.ReadDir(filepath.Join(dir, "bin"))
	if err != nil {
		return out, err
	}
	for _, ent := range ents {
		if ent.Type()&os.ModeSymlink == 0 {
			continue
		}
		if t, err := os.Readlink(filepath.Join(dir, "bin", ent.Name())); err == nil && t == "cni" {
			if err := os.Symlink(gcni, filepath.Join(cni, ent.Name())); err != nil {
				return out, err
			}
			out.CNILinks++
		}
	}
	if err := os.WriteFile(filepath.Join(dir, verifiedMark), []byte(hash+"\n"), 0o644); err != nil {
		return out, err
	}
	if ents, err := os.ReadDir(data); err == nil {
		for _, e := range ents {
			if n := e.Name(); e.IsDir() && n != hash && n != "cni" && len(strings.TrimSuffix(n, "-tmp")) == 64 {
				os.RemoveAll(filepath.Join(data, n))
			}
		}
	}
	return out, nil
}

const verifiedMark = ".fev-verified"

func Check(dest, hash string) (files, links int, err error) {
	dir := filepath.Join(dest, "data", hash)
	if files, links, err = verify(filepath.Join(dir, "bin")); err != nil {
		os.Remove(filepath.Join(dir, verifiedMark))
	}
	return files, links, err
}

func Verified(dest, hash string) bool {
	b, err := os.ReadFile(filepath.Join(dest, "data", hash, verifiedMark))
	if err != nil || strings.TrimSpace(string(b)) != hash {
		return false
	}
	_, err = os.Stat(filepath.Join(dest, "data", hash, "bin", "k3s"))
	return err == nil
}

func replaceLink(target, link string) error {
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func untar(r io.Reader, dir string) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for {
		f, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if f.Name == "" || strings.Contains(f.Name, `\`) || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "../") || f.Name == ".." {
			return fmt.Errorf("invalid name %q", f.Name)
		}
		abs := filepath.Join(dir, filepath.FromSlash(f.Name))
		mode := f.FileInfo().Mode()
		switch {
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return err
			}
			wf, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode.Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(wf, tr)
			if cerr := wf.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if n != f.Size {
				return fmt.Errorf("%s: %d of %d bytes", abs, n, f.Size)
			}
			if err := os.Chmod(abs, mode.Perm()); err != nil {
				return err
			}
		case mode.IsDir():
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}
		case f.Typeflag == tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(f.Linkname, abs); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported kind %v", f.Name, mode)
		}
	}
}

func verify(bin string) (int, int, error) {
	sums, err := fields(filepath.Join(bin, ".sha256sums"), 1, 0)
	if err != nil {
		return 0, 0, err
	}
	links, err := fields(filepath.Join(bin, ".links"), 0, 1)
	if err != nil {
		return 0, 0, err
	}
	if len(sums) == 0 || len(links) == 0 {
		return 0, 0, errors.New("empty .sha256sums or .links")
	}
	var bad []string
	for _, name := range sorted(sums) {
		f, err := os.Open(filepath.Join(bin, name))
		if err != nil {
			bad = append(bad, name)
			continue
		}
		h := sha256.New()
		io.Copy(h, f)
		f.Close()
		if hex.EncodeToString(h.Sum(nil)) != sums[name] {
			bad = append(bad, name)
		}
	}
	for _, name := range sorted(links) {
		if t, _ := os.Readlink(filepath.Join(bin, name)); t != links[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		return 0, 0, fmt.Errorf("k3s's payload failed its own check for %s", strings.Join(bad, ", "))
	}
	return len(sums), len(links), nil
}

func fields(name string, key, val int) (map[string]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		w := strings.Fields(s.Text())
		if len(w) == 0 {
			continue
		}
		if len(w) <= key || len(w) <= val {
			return nil, fmt.Errorf("%s: short line %q", name, s.Text())
		}
		m[w[key]] = w[val]
	}
	return m, s.Err()
}

func sorted(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}
