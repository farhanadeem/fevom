package kubeconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

const Context = "fev"

var LockWait = 5 * time.Second

func DefaultPath(kubeconfigEnv, userHome string) string {
	for _, p := range filepath.SplitList(kubeconfigEnv) {
		if p != "" {
			return p
		}
	}
	return filepath.Join(userHome, ".kube", "config")
}

type Note struct {
	Path       string   `json:"path"`
	Restorable bool     `json:"restorable"`
	Existed    bool     `json:"existed,omitempty"`
	Before     []byte   `json:"before,omitempty"`
	Mode       uint32   `json:"mode,omitempty"`
	MadeDirs   []string `json:"made_dirs,omitempty"`
	Wrote      string   `json:"wrote"`
}

func NotePath(fevHome string) string {
	return filepath.Join(fevHome, "k8s", "kubeconfig.json")
}

func ReadNote(notePath string) (*Note, error) {
	data, err := os.ReadFile(notePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var n Note
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("%s: %w", notePath, err)
	}
	return &n, nil
}

func writeNote(notePath string, n *Note) error {
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(notePath), 0o700); err != nil {
		return err
	}
	return replace(notePath, append(data, '\n'), 0o600)
}

type Merged struct {
	Path    string
	Added   bool
	Current bool
}

func Merge(path, notePath, raw, server string) (Merged, error) {
	cluster, user, err := clusterOf(raw)
	if err != nil {
		return Merged{}, err
	}
	setScalar(cluster, "server", "https://"+server)
	made, err := mkdirs(filepath.Dir(path))
	if err != nil {
		return Merged{}, err
	}
	unlock, err := lock(path)
	if err != nil {
		return Merged{}, err
	}
	defer unlock()

	target, err := resolve(path)
	if err != nil {
		return Merged{}, err
	}
	before, err := os.ReadFile(target)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Merged{}, err
	}
	var mode fs.FileMode = 0o600
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	doc, root, err := parse(before)
	if err != nil {
		return Merged{}, fmt.Errorf("%s: %w", path, err)
	}
	had := named(seq(root, "contexts"), Context) != nil
	put(root, "clusters", "cluster", cluster)
	put(root, "users", "user", user)
	put(root, "contexts", "context", mapping("cluster", Context, "user", Context))
	if cur := scalar(root, "current-context"); cur == "" {
		setScalar(root, "current-context", Context)
	}
	out, err := encode(doc)
	if err != nil {
		return Merged{}, err
	}

	var note *Note
	if notePath != "" {
		note, _ = ReadNote(notePath)
		switch {
		case note == nil || note.Path != path:
			note = &Note{Path: path, Restorable: !had, Existed: existed, Mode: uint32(mode)}
			if !had && existed {
				note.Before = before
			}
		case note.Wrote != digest(before):
			note.Restorable, note.Before = false, nil
		}
	}
	if !existed {
		more, err := mkdirs(filepath.Dir(target))
		if err != nil {
			return Merged{}, err
		}
		made = append(made, more...)
	}
	if bytes.Equal(out, before) && existed {
	} else if err := replace(target, out, 0o600); err != nil {
		return Merged{}, err
	} else if mode != 0o600 {
		os.Chmod(target, 0o600)
	}
	if note != nil {
		if note.Restorable && !note.Existed && len(note.MadeDirs) == 0 {
			note.MadeDirs = made
		}
		note.Wrote = digest(out)
		if err := writeNote(notePath, note); err != nil {
			return Merged{}, err
		}
	}
	return Merged{Path: path, Added: !had, Current: scalar(root, "current-context") == Context}, nil
}

type Removed struct {
	Path     string
	Restored bool
	Nothing  bool
}

func Remove(notePath, path string) (Removed, error) {
	var note *Note
	if notePath != "" {
		n, err := ReadNote(notePath)
		if err != nil {
			return Removed{}, err
		}
		note = n
	}
	if note != nil && note.Path != "" {
		path = note.Path
	}
	if path == "" {
		return Removed{Nothing: true}, nil
	}
	r, err := remove(path, note)
	if err != nil {
		return r, err
	}
	if notePath != "" {
		if err := os.Remove(notePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return r, err
		}
	}
	return r, nil
}

func remove(path string, note *Note) (Removed, error) {
	r := Removed{Path: path}
	unlock, err := lock(path)
	if err != nil {
		return r, err
	}
	defer unlock()
	target, err := resolve(path)
	if err != nil {
		return r, err
	}
	now, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		r.Nothing = true
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if note != nil && note.Restorable && note.Wrote == digest(now) {
		r.Restored = true
		if !note.Existed {
			if err := os.Remove(target); err != nil {
				return r, err
			}
			unlock()
			for i := len(note.MadeDirs) - 1; i >= 0; i-- {
				if os.Remove(note.MadeDirs[i]) != nil {
					break
				}
			}
			return r, nil
		}
		mode := fs.FileMode(note.Mode)
		if mode == 0 {
			mode = 0o600
		}
		if err := replace(target, note.Before, mode); err != nil {
			return r, err
		}
		return r, os.Chmod(target, mode)
	}
	doc, root, err := parse(now)
	if err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	took := false
	for _, k := range []string{"clusters", "users", "contexts"} {
		took = drop(seq(root, k), Context) || took
	}
	if scalar(root, "current-context") == Context {
		setScalar(root, "current-context", "")
		took = true
	}
	if !took {
		r.Nothing = true
		return r, nil
	}
	out, err := encode(doc)
	if err != nil {
		return r, err
	}
	return r, replace(target, out, 0o600)
}

func Present(path string) (present, current bool) {
	target, err := resolve(path)
	if err != nil {
		return false, false
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return false, false
	}
	_, root, err := parse(data)
	if err != nil {
		return false, false
	}
	return named(seq(root, "contexts"), Context) != nil, scalar(root, "current-context") == Context
}

func Extract(path string) ([]byte, error) {
	target, err := resolve(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	_, root, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	doc, out, _ := parse(nil)
	for _, k := range []string{"clusters", "users", "contexts"} {
		n := named(seq(root, k), Context)
		if n == nil {
			return nil, fmt.Errorf("%s has no %s named %s: the cluster is off, or its context was removed", path, strings.TrimSuffix(k, "s"), Context)
		}
		seq(out, k).Content = append(seq(out, k).Content, n)
	}
	setScalar(out, "current-context", Context)
	return encode(doc)
}

func lock(path string) (func(), error) {
	name := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(LockWait)
	wait := 10 * time.Millisecond
	for {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			done := false
			return func() {
				if !done {
					done = true
					os.Remove(name)
				}
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held: a kubectl is writing the kubeconfig, or one that ended without removing its lock left it there (remove it if no kubectl runs)", name)
		}
		time.Sleep(wait)
		wait = min(2*wait, 50*time.Millisecond)
	}
}

func resolve(path string) (string, error) {
	p := path
	for i := 0; i < 40; i++ {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		to, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(to) {
			to = filepath.Join(filepath.Dir(p), to)
		}
		p = to
	}
	return "", fmt.Errorf("%s: too many links", path)
}

func replace(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".fev-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := unix.Fsync(int(f.Fd())); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func mkdirs(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append([]string{d}, missing...)
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, d := range missing {
		if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return missing, nil
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func parse(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, nil, err
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("not a kubeconfig: its top is not a mapping")
	}
	root := doc.Content[0]
	root.Style = 0
	if scalar(root, "apiVersion") == "" {
		setScalar(root, "apiVersion", "v1")
	}
	if scalar(root, "kind") == "" {
		setScalar(root, "kind", "Config")
	}
	for _, k := range []string{"clusters", "contexts", "users"} {
		seq(root, k)
	}
	return &doc, root, nil
}

func encode(doc *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(doc); err != nil {
		return nil, err
	}
	e.Close()
	return b.Bytes(), nil
}

func value(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalar(m *yaml.Node, key string) string {
	if v := value(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func str(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

func setScalar(m *yaml.Node, key, v string) {
	if n := value(m, key); n != nil {
		*n = *str(v)
		return
	}
	m.Content = append(m.Content, str(key), str(v))
}

func setNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, str(key), v)
}

func seq(m *yaml.Node, key string) *yaml.Node {
	v := value(m, key)
	if v != nil && v.Kind == yaml.SequenceNode {
		v.Style = 0
		return v
	}
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	setNode(m, key, n)
	return n
}

func mapping(kv ...string) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		m.Content = append(m.Content, str(kv[i]), str(kv[i+1]))
	}
	return m
}

func named(s *yaml.Node, name string) *yaml.Node {
	for _, e := range s.Content {
		if scalar(e, "name") == name {
			return e
		}
	}
	return nil
}

func put(root *yaml.Node, key, field string, v *yaml.Node) {
	s := seq(root, key)
	e := mapping("name", Context)
	setNode(e, field, v)
	for i, x := range s.Content {
		if scalar(x, "name") == Context {
			s.Content[i] = e
			return
		}
	}
	s.Content = append(s.Content, e)
}

func drop(s *yaml.Node, name string) bool {
	kept := s.Content[:0]
	took := false
	for _, e := range s.Content {
		if scalar(e, "name") == name {
			took = true
			continue
		}
		kept = append(kept, e)
	}
	s.Content = kept
	return took
}

func clusterOf(raw string) (cluster, user *yaml.Node, err error) {
	_, root, err := parse([]byte(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("the cluster's kubeconfig: %w", err)
	}
	cs, us := seq(root, "clusters").Content, seq(root, "users").Content
	if len(cs) == 0 || len(us) == 0 {
		return nil, nil, errors.New("the cluster's kubeconfig has no cluster or no user")
	}
	cluster, user = value(cs[0], "cluster"), value(us[0], "user")
	if cluster == nil || cluster.Kind != yaml.MappingNode || user == nil || user.Kind != yaml.MappingNode {
		return nil, nil, errors.New("the cluster's kubeconfig has no cluster or no user")
	}
	return cluster, user, nil
}
