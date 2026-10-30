package fakeguest

import (
	"encoding/json"
	"net/http"
)

const (
	AuthUser     = "fev-user"
	AuthPassword = "s3cret-word"
)

func (e *Engine) auth(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	e.mu.Lock()
	e.authSeen = append(e.authSeen, p.ServerAddress+" "+p.Username+":"+p.Password)
	e.mu.Unlock()
	if p.Username != AuthUser || p.Password != AuthPassword {
		fail(w, http.StatusUnauthorized, "Get \"https://%s/v2/\": unauthorized: incorrect username or password", p.ServerAddress)
		return
	}
	reply(w, map[string]string{"Status": "Login Succeeded"})
}

func (e *Engine) logins(w http.ResponseWriter) {
	e.mu.Lock()
	seen := append([]string{}, e.authSeen...)
	e.mu.Unlock()
	reply(w, seen)
}
