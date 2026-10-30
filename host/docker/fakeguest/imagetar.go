package fakeguest

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type savedManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func (e *Engine) imageTarRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/images/get" && r.Method == http.MethodGet:
		names := r.URL.Query()["names"]
		all := e.images()
		var picked []fakeImage
		for _, n := range names {
			found := false
			for _, im := range all {
				for _, t := range im.RepoTags {
					if t == familiarRef(n) || im.ID == n || im.ID == imageID(n) {
						if !found {
							picked = append(picked, im)
						}
						found = true
					}
				}
			}
			if !found {
				fail(w, http.StatusNotFound, "reference does not exist: %s", n)
				return true
			}
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		tw := tar.NewWriter(w)
		var manifest []savedManifest
		at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
		for _, im := range picked {
			hex := strings.TrimPrefix(im.ID, "sha256:")
			layer := fmt.Sprintf("blobs/sha256/%s", hex)
			body := []byte(strings.Repeat(im.ID+"\n", (2<<20)/(len(im.ID)+1)))
			tw.WriteHeader(&tar.Header{Name: layer, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: at})
			tw.Write(body)
			manifest = append(manifest, savedManifest{Config: layer, RepoTags: im.RepoTags, Layers: []string{layer}})
		}
		m, _ := json.Marshal(manifest)
		tw.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(m)), ModTime: at})
		tw.Write(m)
		tw.Close()
	case path == "/images/load" && r.Method == http.MethodPost:
		var manifest []savedManifest
		tr := tar.NewReader(r.Body)
		var size int64
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				fail(w, http.StatusBadRequest, "the tar: %v", err)
				return true
			}
			if h.Name == "manifest.json" {
				if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
					fail(w, http.StatusBadRequest, "manifest.json: %v", err)
					return true
				}
				continue
			}
			n, _ := io.Copy(io.Discard, tr)
			size += n
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if manifest == nil {
			json.NewEncoder(w).Encode(map[string]any{"errorDetail": map[string]string{"message": "open /var/lib/docker/tmp/docker-import/manifest.json: no such file or directory"},
				"error": "open /var/lib/docker/tmp/docker-import/manifest.json: no such file or directory"})
			return true
		}
		e.mu.Lock()
		if e.extraImages == nil {
			e.extraImages = map[string]fakeImage{}
		}
		var lines []string
		for _, m := range manifest {
			if len(m.RepoTags) == 0 {
				continue
			}
			im := fakeImage{ID: imageID(m.RepoTags[0]), RepoTags: m.RepoTags, Size: size, Created: time.Now().Unix()}
			e.extraImages[im.ID] = im
			e.emit("image", "load", im.ID, map[string]string{"name": m.RepoTags[0]})
			lines = append(lines, "Loaded image: "+m.RepoTags[0])
		}
		e.mu.Unlock()
		for _, l := range lines {
			json.NewEncoder(w).Encode(map[string]string{"stream": l + "\n"})
		}
	default:
		return false
	}
	return true
}
