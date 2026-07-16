package app

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/julieta/minidock/internal/runtime"
)

type repositoryBrowserEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Repository bool   `json:"repository"`
}

type repositoryBrowserData struct {
	Current string                   `json:"current"`
	Parent  string                   `json:"parent,omitempty"`
	Entries []repositoryBrowserEntry `json:"entries"`
}

type repositoryReferencesData struct {
	References []string `json:"references"`
}

// browseLocalRepositories exposes directories, never files, below the single
// host path explicitly granted to MiniDock. Every navigation step resolves
// symlinks before it is returned so a symlink cannot escape that boundary.
func (a *App) browseLocalRepositories(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	root := a.config.LocalRepositoriesPath
	if root == "" {
		http.Error(w, "local repository browsing is not configured", http.StatusServiceUnavailable)
		return
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		http.Error(w, "local repository root is unavailable", http.StatusServiceUnavailable)
		return
	}
	relative := strings.TrimPrefix(filepath.Clean("/"+r.URL.Query().Get("path")), "/")
	if relative == "." {
		relative = ""
	}
	current, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil || !withinRepositoryRoot(root, current) {
		http.NotFound(w, r)
		return
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := repositoryBrowserData{Current: "file://" + current, Entries: []repositoryBrowserEntry{}}
	if current != root {
		data.Parent = filepath.ToSlash(strings.TrimPrefix(filepath.Dir(current), root+string(os.PathSeparator)))
	}
	for _, entry := range entries {
		// Hidden directories are rarely deployment sources and make a user's
		// home directory difficult to navigate, so omit them from the listing.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		path, err := filepath.EvalSymlinks(filepath.Join(current, entry.Name()))
		if err != nil || !withinRepositoryRoot(root, path) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		_, gitErr := os.Stat(filepath.Join(path, ".git"))
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(os.PathSeparator)))
		data.Entries = append(data.Entries, repositoryBrowserEntry{Name: entry.Name(), Path: rel, Repository: gitErr == nil})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func (a *App) localRepositoryReferences(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	path, ok := a.localRepositoryPath(r.URL.Query().Get("path"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		http.Error(w, "folder is not a Git repository", http.StatusBadRequest)
		return
	}
	output, err := exec.Command("git", "-C", path, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/tags").Output()
	if err != nil {
		http.Error(w, "could not read Git references", http.StatusServiceUnavailable)
		return
	}
	references := strings.Fields(string(output))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(repositoryReferencesData{References: references})
}

func (a *App) detectLocalRuntime(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	path, ok := a.localRepositoryPath(r.URL.Query().Get("path"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runtime.Detect(path))
}

func (a *App) createLocalRepositoryFolder(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	var request struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
		http.Error(w, "invalid folder request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		http.Error(w, "invalid folder name", http.StatusBadRequest)
		return
	}
	root, err := filepath.EvalSymlinks(a.config.LocalRepositoriesPath)
	if err != nil {
		http.Error(w, "local repository root is unavailable", http.StatusServiceUnavailable)
		return
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Clean("/"+strings.TrimPrefix(request.Path, "/"))))
	if err != nil || !withinRepositoryRoot(root, parent) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(parent, name)
	if !withinRepositoryRoot(root, path) {
		http.Error(w, "invalid folder path", http.StatusBadRequest)
		return
	}
	if err := os.Mkdir(path, 0700); err != nil {
		http.Error(w, "could not create folder", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": filepath.ToSlash(strings.TrimPrefix(path, root+string(os.PathSeparator)))})
}

func (a *App) validateLocalRuntime(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	path, ok := a.localRepositoryPath(r.URL.Query().Get("path"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	kind := r.URL.Query().Get("type")
	missing := runtime.Validate(path, kind)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"valid":   len(missing) == 0,
		"missing": missing,
	})
}

func (a *App) localRepositoryPath(relative string) (string, bool) {
	root, err := filepath.EvalSymlinks(a.config.LocalRepositoriesPath)
	if err != nil {
		return "", false
	}
	relative = strings.TrimPrefix(filepath.Clean("/"+relative), "/")
	path, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	return path, err == nil && withinRepositoryRoot(root, path)
}

func withinRepositoryRoot(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}
