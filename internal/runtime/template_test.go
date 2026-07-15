package runtime

import (
	"strings"
	"testing"
)

func TestBuiltInTemplatesHaveOperationalDefaults(t *testing.T) {
	for _, kind := range []string{"static", "angular_static", "node_ssr", "vite_ssr", "astro_static", "astro_ssr", "nuxt_static", "nuxt_ssr", "svelte_static", "svelte_ssr", "nextjs", "go", "rust", "java"} {
		template, ok := For(kind)
		if !ok || template.CPUs == "" || template.Memory == "" {
			t.Fatalf("%s has no resource limits", kind)
		}
		dockerfile, ok := Dockerfile(kind, 9090)
		if !ok || !strings.Contains(dockerfile, "HEALTHCHECK") || !strings.Contains(dockerfile, "9090") {
			t.Fatalf("%s does not render health check and port: %s", kind, dockerfile)
		}
	}
}

func TestCustomIsNotAGeneratedTemplate(t *testing.T) {
	if _, ok := Dockerfile("custom", 8080); ok {
		t.Fatal("custom must require a Dockerfile")
	}
}
