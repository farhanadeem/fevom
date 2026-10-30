package daemon

const (
	MethodContextsList      = "contexts.list"
	MethodContextContainers = "context.containers"
	MethodContextImages     = "context.images"
)

type ContextRef struct {
	Context string `json:"context"`
}

type RemoteContext struct {
	Name        string `json:"name"`
	Host        string `json:"host,omitempty"`
	Current     bool   `json:"current,omitempty"`
	Description string `json:"description,omitempty"`
}

type RemoteContexts struct {
	Contexts []RemoteContext `json:"contexts"`
}

type RemoteContainer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status,omitempty"`
}

type RemoteContainers struct {
	Containers []RemoteContainer `json:"containers"`
}

type RemoteImage struct {
	ID       string   `json:"id"`
	RepoTags []string `json:"repo_tags,omitempty"`
	Size     int64    `json:"size,omitempty"`
	Created  int64    `json:"created,omitempty"`
}

type RemoteImages struct {
	Images []RemoteImage `json:"images"`
}
