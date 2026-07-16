package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Detection is a non-executing inspection result. It only reads conventional
// project manifests, so opening a folder never runs package scripts or code.
type Detection struct {
	Type       string `json:"type"`
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`
}

type packageManifest struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Scripts         map[string]string `json:"scripts"`
}

func Detect(path string) Detection {
	if exists(filepath.Join(path, "Dockerfile")) {
		return Detection{"custom", "high", "El proyecto ya incluye un Dockerfile."}
	}
	if exists(filepath.Join(path, "go.mod")) {
		return Detection{"go", "high", "Se encontró go.mod."}
	}
	if exists(filepath.Join(path, "Cargo.toml")) {
		return Detection{"rust", "high", "Se encontró Cargo.toml."}
	}
	if exists(filepath.Join(path, "pom.xml")) || exists(filepath.Join(path, "build.gradle")) || exists(filepath.Join(path, "build.gradle.kts")) {
		return Detection{"java", "high", "Se encontró configuración Maven o Gradle."}
	}
	manifest, ok := readPackageManifest(filepath.Join(path, "package.json"))
	if !ok {
		return Detection{"custom", "low", "No se encontró un manifiesto reconocible; revisa la configuración manual."}
	}
	if dependency(manifest, "astro") {
		config := projectConfig(path, "astro.config.mjs", "astro.config.js", "astro.config.ts")
		if strings.Contains(config, "output: 'server'") || strings.Contains(config, `output: "server"`) || strings.Contains(config, "@astrojs/node") {
			return Detection{"astro_ssr", "high", "Astro SSR con adaptador Node detectado."}
		}
		return Detection{"astro_static", "high", "Astro estático detectado."}
	}
	if dependency(manifest, "nuxt") {
		config := projectConfig(path, "nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs")
		if strings.Contains(config, "ssr: false") || strings.Contains(config, "ssr:false") || scriptContains(manifest, "nuxt generate") {
			return Detection{"nuxt_static", "medium", "Nuxt estático detectado; confirma que el proyecto use generate."}
		}
		return Detection{"nuxt_ssr", "high", "Nuxt SSR/Nitro detectado."}
	}
	if dependency(manifest, "@sveltejs/kit") {
		config := projectConfig(path, "svelte.config.js", "svelte.config.ts")
		if strings.Contains(config, "adapter-static") {
			return Detection{"svelte_static", "high", "SvelteKit con adapter-static detectado."}
		}
		if strings.Contains(config, "adapter-node") {
			return Detection{"svelte_ssr", "high", "SvelteKit con adapter-node detectado."}
		}
		return Detection{"custom", "medium", "SvelteKit detectado; selecciona adapter-static o adapter-node en svelte.config."}
	}
	if dependency(manifest, "next") {
		return Detection{"nextjs", "high", "Se encontró Next.js; confirma que output: standalone esté habilitado."}
	}
	angular := exists(filepath.Join(path, "angular.json")) || dependency(manifest, "@angular/core")
	if angular && (dependency(manifest, "@angular/ssr") || dependency(manifest, "@nguniversal/express-engine") || scriptContains(manifest, "ssr")) {
		return Detection{"node_ssr", "medium", "Angular SSR detectado; revisa que el script start inicie el servidor SSR."}
	}
	if angular {
		return Detection{"angular_static", "high", "Angular estático detectado."}
	}
	if dependency(manifest, "vite") && (scriptContains(manifest, "--ssr") || scriptContains(manifest, "serve:ssr") || exists(filepath.Join(path, "src", "entry-server.ts")) || exists(filepath.Join(path, "src", "entry-server.tsx")) || exists(filepath.Join(path, "src", "entry-server.js")) || exists(filepath.Join(path, "src", "entry-server.jsx"))) {
		if exists(filepath.Join(path, "server.js")) {
			return Detection{"vite_ssr", "high", "Vite SSR con servidor Node propio detectado (server.js)."}
		}
		return Detection{"node_ssr", "medium", "Vite SSR detectado; revisa que npm run start inicie el servidor."}
	}
	if dependency(manifest, "vite") {
		return Detection{"static", "high", "Vite estático detectado."}
	}
	return Detection{"custom", "low", "Se encontró package.json, pero no una configuración de runtime concluyente."}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func readPackageManifest(path string) (packageManifest, bool) {
	file, err := os.Open(path)
	if err != nil {
		return packageManifest{}, false
	}
	defer file.Close()
	var manifest packageManifest
	return manifest, json.NewDecoder(file).Decode(&manifest) == nil
}

func dependency(manifest packageManifest, name string) bool {
	_, found := manifest.Dependencies[name]
	if found {
		return true
	}
	_, found = manifest.DevDependencies[name]
	return found
}

func scriptContains(manifest packageManifest, text string) bool {
	for _, script := range manifest.Scripts {
		if strings.Contains(strings.ToLower(script), strings.ToLower(text)) {
			return true
		}
	}
	return false
}

func projectConfig(path string, names ...string) string {
	for _, name := range names {
		contents, err := os.ReadFile(filepath.Join(path, name))
		if err == nil {
			return string(contents)
		}
	}
	return ""
}

// Validate checks if the project directory meets the minimum requirements for the selected runtime template.
// It returns a list of missing requirements/errors.
func Validate(path, kind string) []string {
	var missing []string
	switch kind {
	case "go":
		if !exists(filepath.Join(path, "go.mod")) {
			missing = append(missing, "Falta el archivo go.mod en el directorio del proyecto.")
		}
	case "rust":
		if !exists(filepath.Join(path, "Cargo.toml")) {
			missing = append(missing, "Falta el archivo Cargo.toml en el directorio del proyecto.")
		}
	case "java":
		if !exists(filepath.Join(path, "pom.xml")) && !exists(filepath.Join(path, "build.gradle")) && !exists(filepath.Join(path, "build.gradle.kts")) {
			missing = append(missing, "Falta el archivo pom.xml o build.gradle en el directorio del proyecto.")
		}
	case "custom":
		if !exists(filepath.Join(path, "Dockerfile")) {
			missing = append(missing, "Falta el archivo Dockerfile en el directorio del proyecto.")
		}
	case "static", "angular_static", "node_ssr", "vite_ssr", "astro_static", "astro_ssr", "nuxt_static", "nuxt_ssr", "svelte_static", "svelte_ssr", "nextjs":
		if !exists(filepath.Join(path, "package.json")) {
			missing = append(missing, "Falta el archivo package.json para este runtime basado en Node.")
		}
	}
	return missing
}
