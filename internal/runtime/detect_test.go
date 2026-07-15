package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFrameworksWithoutExecutingProjectCode(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"vite", map[string]string{"package.json": `{"devDependencies":{"vite":"latest"}}`}, "static"},
		{"vite ssr", map[string]string{"package.json": `{"devDependencies":{"vite":"latest"},"scripts":{"build":"vite build --ssr"}}`}, "node_ssr"},
		{"vite ssr custom server", map[string]string{"package.json": `{"devDependencies":{"vite":"latest"},"scripts":{"build":"vite build --ssr"}}`, "server.js": ""}, "vite_ssr"},
		{"angular", map[string]string{"angular.json": `{}`, "package.json": `{"dependencies":{"@angular/core":"latest"}}`}, "angular_static"},
		{"angular ssr", map[string]string{"angular.json": `{}`, "package.json": `{"dependencies":{"@angular/core":"latest","@angular/ssr":"latest"}}`}, "node_ssr"},
		{"astro static", map[string]string{"package.json": `{"dependencies":{"astro":"latest"}}`}, "astro_static"},
		{"astro ssr", map[string]string{"package.json": `{"dependencies":{"astro":"latest","@astrojs/node":"latest"}}`, "astro.config.mjs": `export default { output: 'server' }`}, "astro_ssr"},
		{"nuxt static", map[string]string{"package.json": `{"dependencies":{"nuxt":"latest"},"scripts":{"generate":"nuxt generate"}}`}, "nuxt_static"},
		{"nuxt ssr", map[string]string{"package.json": `{"dependencies":{"nuxt":"latest"}}`}, "nuxt_ssr"},
		{"svelte static", map[string]string{"package.json": `{"devDependencies":{"@sveltejs/kit":"latest"}}`, "svelte.config.js": `import adapter from '@sveltejs/adapter-static'`}, "svelte_static"},
		{"svelte ssr", map[string]string{"package.json": `{"devDependencies":{"@sveltejs/kit":"latest"}}`, "svelte.config.js": `import adapter from '@sveltejs/adapter-node'`}, "svelte_ssr"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir()
			for name, contents := range test.files {
				if err := os.WriteFile(filepath.Join(path, name), []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := Detect(path).Type; got != test.want {
				t.Fatalf("Detect() = %q, want %q", got, test.want)
			}
		})
	}
}
