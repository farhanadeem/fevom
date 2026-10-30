package debugstore

import (
	"embed"

	"fev/proto/contract"
)

const (
	StoreDir    = "/nix/store"
	ProfileDir  = "/nix/var/fev/profile"
	RunDir      = "/nix/var/fev/run"
	EtcDir      = "/nix/var/fev/etc"
	StoreSocket = RunDir + "/store.sock"
)

const (
	CacheURL       = "https://cache.nixos.org"
	CacheName      = "cache.nixos.org-1"
	CachePublicKey = "6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY="
	CacheKey       = CacheName + ":" + CachePublicKey
)

const (
	MaxNarBytes     = int64(2) << 30
	MaxClosurePaths = 4096
	MaxCandidates   = 20
	MaxNarDepth     = 256
)

const FingerprintForm = "1;PATH;NARHASH;NARSIZE;REFS"

const (
	MethodLookup   = "store.lookup"
	MethodInstall  = "store.install"
	MethodRemove   = "store.remove"
	MethodList     = "store.list"
	MethodSearch   = "store.search"
	MethodGC       = "store.gc"
	NotifyProgress = "store.progress"
)

const (
	PhaseResolve = "resolve"
	PhaseFetch   = "fetch"
	PhaseUnpack  = "unpack"
)

type LookupParams struct {
	Command string `json:"command"`
}

type Lookup struct {
	Candidates []Candidate `json:"candidates,omitempty"`
}

type Candidate struct {
	Attr      string `json:"attr"`
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	StorePath string `json:"store_path,omitempty"`
	License   string `json:"license,omitempty"`
	Unfree    bool   `json:"unfree,omitempty"`
	Pick      bool   `json:"pick,omitempty"`
}

type InstallParams struct {
	Names []string `json:"names"`
}

type Installed struct {
	Installed []Package `json:"installed,omitempty"`
	Commands  []string  `json:"commands,omitempty"`
}

type RemoveParams struct {
	Names []string `json:"names,omitempty"`
}

type Removed struct {
	Removed    []string `json:"removed,omitempty"`
	FreedBytes int64    `json:"freed_bytes"`
}

type ListResult struct {
	Packages []Package `json:"packages,omitempty"`
}

type SearchParams struct {
	Text  string `json:"text"`
	Limit int    `json:"limit,omitempty"`
}

type SearchResult struct {
	Matches []Match `json:"matches,omitempty"`
}

type Match struct {
	Command string `json:"command"`
	Attr    string `json:"attr"`
	Name    string `json:"name"`
	License string `json:"license,omitempty"`
	Unfree  bool   `json:"unfree,omitempty"`
}

type Package struct {
	Name      string   `json:"name"`
	Attr      string   `json:"attr,omitempty"`
	StorePath string   `json:"store_path"`
	Commands  []string `json:"commands,omitempty"`
	Default   bool     `json:"default,omitempty"`
}

type Progress struct {
	Phase      string `json:"phase"`
	DoneBytes  int64  `json:"done_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	PathsDone  int    `json:"paths_done"`
	PathsTotal int    `json:"paths_total"`
}

var Schema = map[string]contract.Types{
	MethodLookup:   {LookupParams{}, Lookup{}},
	MethodInstall:  {InstallParams{}, Installed{}},
	MethodRemove:   {RemoveParams{}, Removed{}},
	MethodList:     {Empty{}, ListResult{}},
	MethodSearch:   {SearchParams{}, SearchResult{}},
	MethodGC:       {RemoveParams{}, Removed{}},
	NotifyProgress: {Progress{}, nil},
}

type Empty struct{}

//go:embed fixtures/*.json
var fixtureFiles embed.FS

type Fixture = contract.Fixture

func Fixtures() ([]Fixture, error) { return contract.Load(fixtureFiles, "fixtures") }

func Conforms(data []byte, zero any) error { return contract.Conforms(data, zero) }
