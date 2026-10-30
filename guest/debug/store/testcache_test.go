package store

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type node struct {
	kind     byte
	contents []byte
	target   string
	children map[string]*node
}

func dir(children ...namedNode) *node {
	m := map[string]*node{}
	for _, c := range children {
		m[c.name] = c.n
	}
	return &node{kind: 'd', children: m}
}

type namedNode struct {
	name string
	n    *node
}

func file(name string, contents string) namedNode {
	return namedNode{name, &node{kind: 'f', contents: []byte(contents)}}
}
func exe(name string, contents string) namedNode {
	return namedNode{name, &node{kind: 'x', contents: []byte(contents)}}
}
func link(name, target string) namedNode {
	return namedNode{name, &node{kind: 's', target: target}}
}

func encodeNAR(root *node) []byte {
	var b strings.Builder
	ns := func(s string) {
		var l [8]byte
		binary.LittleEndian.PutUint64(l[:], uint64(len(s)))
		b.Write(l[:])
		b.WriteString(s)
		if p := (8 - len(s)%8) % 8; p > 0 {
			b.Write(make([]byte, p))
		}
	}
	var emit func(n *node)
	emit = func(n *node) {
		ns("(")
		ns("type")
		switch n.kind {
		case 'd':
			ns("directory")
			names := make([]string, 0, len(n.children))
			for k := range n.children {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, name := range names {
				ns("entry")
				ns("(")
				ns("name")
				ns(name)
				ns("node")
				emit(n.children[name])
				ns(")")
			}
		case 'f', 'x':
			ns("regular")
			if n.kind == 'x' {
				ns("executable")
				ns("")
			}
			ns("contents")
			var l [8]byte
			binary.LittleEndian.PutUint64(l[:], uint64(len(n.contents)))
			b.Write(l[:])
			b.Write(n.contents)
			if p := (8 - len(n.contents)%8) % 8; p > 0 {
				b.Write(make([]byte, p))
			}
		case 's':
			ns("symlink")
			ns("target")
			ns(n.target)
		}
		ns(")")
	}
	ns("nix-archive-1")
	emit(root)
	return []byte(b.String())
}

func rawNAR(b []byte) []byte { return b }

type cache struct {
	t          *testing.T
	srv        *httptest.Server
	priv       ed25519.PrivateKey
	keyName    string
	narinfos   map[string]string
	nars       map[string][]byte
	badSigKey  ed25519.PrivateKey
	badSigName string
}

func newCache(t *testing.T) *cache {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_ = pub
	bpub, bpriv, _ := ed25519.GenerateKey(nil)
	_ = bpub
	c := &cache{
		t:          t,
		priv:       priv,
		keyName:    "test-1",
		narinfos:   map[string]string{},
		nars:       map[string][]byte{},
		badSigKey:  bpriv,
		badSigName: "other-1",
	}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasSuffix(p, ".narinfo") {
			h := strings.TrimSuffix(p, ".narinfo")
			if ni, ok := c.narinfos[h]; ok {
				w.Write([]byte(ni))
				return
			}
		}
		if nar, ok := c.nars[p]; ok {
			w.Write(nar)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *cache) pubKey() key { return key{c.keyName, c.priv.Public().(ed25519.PublicKey)} }

type sigOpt int

const (
	sigGood sigOpt = iota
	sigForged
	sigOther
	sigNone
)

func (c *cache) add(hashName string, nar []byte, refs []string, opt sigOpt) string {
	h := hashOf(hashName)
	storePath := StoreDir + "/" + hashName
	sum := sha256.Sum256(nar)
	narHash := "sha256:" + nixBase32(sum[:])
	ni := c.narinfoText(storePath, hashName+".nar", "none", narHash, int64(len(nar)), refs)
	c.signInto(&ni, storePath, narHash, int64(len(nar)), refs, opt)
	c.narinfos[h] = ni
	c.nars[hashName+".nar"] = nar
	return storePath
}

func (c *cache) narinfoText(storePath, url, comp, narHash string, narSize int64, refs []string) string {
	var b strings.Builder
	b.WriteString("StorePath: " + storePath + "\n")
	b.WriteString("URL: " + url + "\n")
	b.WriteString("Compression: " + comp + "\n")
	b.WriteString("NarHash: " + narHash + "\n")
	b.WriteString("NarSize: " + strconv.FormatInt(narSize, 10) + "\n")
	b.WriteString("References: " + strings.Join(refs, " ") + "\n")
	return b.String()
}

func (c *cache) signInto(ni *string, storePath, narHash string, narSize int64, refs []string, opt sigOpt) {
	full := make([]string, len(refs))
	for i, r := range refs {
		full[i] = StoreDir + "/" + r
	}
	fp := "1;" + storePath + ";" + narHash + ";" + strconv.FormatInt(narSize, 10) + ";" + strings.Join(full, ",")
	switch opt {
	case sigGood:
		sig := ed25519.Sign(c.priv, []byte(fp))
		*ni += "Sig: " + c.keyName + ":" + base64.StdEncoding.EncodeToString(sig) + "\n"
	case sigForged:
		sig := ed25519.Sign(c.priv, []byte(fp+"tampered"))
		*ni += "Sig: " + c.keyName + ":" + base64.StdEncoding.EncodeToString(sig) + "\n"
	case sigOther:
		sig := ed25519.Sign(c.badSigKey, []byte(fp))
		*ni += "Sig: " + c.badSigName + ":" + base64.StdEncoding.EncodeToString(sig) + "\n"
	case sigNone:
	}
}

func (c *cache) addRaw(hash, narinfoText string, url string, nar []byte) {
	c.narinfos[hash] = narinfoText
	if url != "" {
		c.nars[url] = nar
	}
}

func newStore(t *testing.T, c *cache) *Store {
	root := t.TempDir()
	t.Cleanup(func() {
		filepath.WalkDir(root, func(q string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(q, 0o755)
			}
			return nil
		})
	})
	s, err := Open(root, c.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.key = c.pubKey()
	return s
}
