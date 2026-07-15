// Package runtime contains the opinionated container templates used by MiniDock.
package runtime

import (
	"fmt"
	"strings"
)

// Template describes the operational defaults coupled to a generated Dockerfile.
// All built-in services listen on PORT (8080 by default) and expose /healthz.
type Template struct {
	Type       string
	Name       string
	Port       int
	CPUs       string
	Memory     string
	Dockerfile string
}

// For returns the built-in template for an application type. Custom applications
// deliberately return false and must provide their own Dockerfile.
func For(kind string) (Template, bool) {
	switch kind {
	case "static":
		return Template{kind, "Estática (Node + Caddy)", 8080, "0.50", "128m", staticDockerfile}, true
	case "angular_static":
		return Template{kind, "Angular estático", 8080, "0.50", "192m", angularStaticDockerfile}, true
	case "node_ssr":
		return Template{kind, "Node SSR (start)", 8080, "0.75", "512m", nodeSSRDockerfile}, true
	case "vite_ssr":
		return Template{kind, "Vite SSR (servidor Node)", 8080, "0.75", "512m", viteSSRDockerfile}, true
	case "astro_static":
		return Template{kind, "Astro estático", 8080, "0.50", "128m", staticDockerfile}, true
	case "astro_ssr":
		return Template{kind, "Astro SSR (Node)", 8080, "0.75", "384m", astroSSRDockerfile}, true
	case "nuxt_static":
		return Template{kind, "Nuxt estático", 8080, "0.50", "192m", nuxtStaticDockerfile}, true
	case "nuxt_ssr":
		return Template{kind, "Nuxt SSR (Nitro)", 8080, "0.75", "512m", nuxtSSRDockerfile}, true
	case "svelte_static":
		return Template{kind, "SvelteKit estático", 8080, "0.50", "128m", svelteStaticDockerfile}, true
	case "svelte_ssr":
		return Template{kind, "SvelteKit SSR (Node)", 8080, "0.75", "384m", svelteSSRDockerfile}, true
	case "nextjs":
		return Template{kind, "Next.js standalone", 8080, "0.75", "512m", nextDockerfile}, true
	case "go":
		return Template{kind, "API Go", 8080, "0.50", "256m", goDockerfile}, true
	case "rust":
		return Template{kind, "API Rust", 8080, "0.50", "256m", rustDockerfile}, true
	case "java":
		return Template{kind, "API Java", 8080, "1.00", "768m", javaDockerfile}, true
	default:
		return Template{}, false
	}
}

func Types() []string {
	return []string{"static", "angular_static", "node_ssr", "vite_ssr", "astro_static", "astro_ssr", "nuxt_static", "nuxt_ssr", "svelte_static", "svelte_ssr", "nextjs", "go", "rust", "java", "custom"}
}

func IsSupported(kind string) bool {
	for _, candidate := range Types() {
		if kind == candidate {
			return true
		}
	}
	return false
}

// Dockerfile returns a generated Dockerfile with the caller-selected port.
func Dockerfile(kind string, port int) (string, bool) {
	template, ok := For(kind)
	if !ok {
		return "", false
	}
	if port < 1 || port > 65535 {
		port = template.Port
	}
	return strings.ReplaceAll(template.Dockerfile, "{{PORT}}", fmt.Sprint(port)), true
}

const staticDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM caddy:2.10-alpine
ARG PORT={{PORT}}
COPY --from=build /app/dist /srv
RUN printf ':%s { root * /srv; try_files {path} /index.html; file_server }\n' "$PORT" > /etc/caddy/Caddyfile
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:{{PORT}}/ || exit 1
`

const nextDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 HOSTNAME=0.0.0.0 PORT={{PORT}}
COPY --from=build /app/public ./public
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD node -e "require('http').get('http://127.0.0.1:'+process.env.PORT+'/healthz',r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"
CMD ["node", "server.js"]
`

const angularStaticDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build && index=$(find dist -type f -name index.html -print -quit) && test -n "$index" && mkdir /out && cp -R "$(dirname "$index")"/. /out/

FROM caddy:2.10-alpine
ARG PORT={{PORT}}
COPY --from=build /out /srv
RUN printf ':%s { root * /srv; try_files {path} /index.html; file_server }\n' "$PORT" > /etc/caddy/Caddyfile
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:{{PORT}}/ || exit 1
`

const nodeSSRDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production HOSTNAME=0.0.0.0 PORT={{PORT}}
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev
COPY --from=build /app/dist ./dist
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD node -e "require('http').get('http://127.0.0.1:'+process.env.PORT+'/',r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"
CMD ["sh", "-c", "npm run start -- --host 0.0.0.0 --port $PORT"]
`

// viteSSRDockerfile targets Vite's custom SSR-server layout. The server is
// run directly because preview scripts often use development-only wrappers.
const viteSSRDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production HOSTNAME=0.0.0.0 PORT={{PORT}}
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev
COPY --from=build /app/dist ./dist
COPY --from=build /app/server.js ./server.js
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD node -e "require('http').get('http://127.0.0.1:'+process.env.PORT+'/',r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"
CMD ["node", "server.js"]
`

const astroSSRDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT={{PORT}}
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev
COPY --from=build /app/dist ./dist
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD node -e "require('http').get('http://127.0.0.1:'+process.env.PORT+'/',r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"
CMD ["node", "./dist/server/entry.mjs"]
`

const nuxtStaticDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run generate

FROM caddy:2.10-alpine
ARG PORT={{PORT}}
COPY --from=build /app/.output/public /srv
RUN printf ':%s { root * /srv; try_files {path} /200.html; file_server }\n' "$PORT" > /etc/caddy/Caddyfile
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:{{PORT}}/ || exit 1
`

const nuxtSSRDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
ENV NITRO_PRESET=node-server
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production NITRO_HOST=0.0.0.0 NITRO_PORT={{PORT}}
COPY --from=build /app/.output ./.output
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD node -e "require('http').get('http://127.0.0.1:'+process.env.NITRO_PORT+'/',r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"
CMD ["node", ".output/server/index.mjs"]
`

const svelteStaticDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM caddy:2.10-alpine
ARG PORT={{PORT}}
COPY --from=build /app/build /srv
RUN printf ':%s { root * /srv; try_files {path} /index.html; file_server }\n' "$PORT" > /etc/caddy/Caddyfile
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:{{PORT}}/ || exit 1
`

const svelteSSRDockerfile = `FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT={{PORT}}
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev
COPY --from=build /app/build ./build
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD node -e "require('http').get('http://127.0.0.1:'+process.env.PORT+'/',r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"
CMD ["node", "build"]
`

const goDockerfile = `FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/app .

FROM busybox:1.37-musl
COPY --from=build /out/app /app
ENV PORT={{PORT}}
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/bin/busybox", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:{{PORT}}/healthz"]
USER 65532:65532
ENTRYPOINT ["/app"]
`

const rustDockerfile = `FROM rust:1.88-alpine AS build
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY . .
RUN cargo build --release && find target/release -maxdepth 1 -type f -perm -111 -exec cp {} /out-app \; -quit

FROM busybox:1.37-musl
COPY --from=build /out-app /app
ENV PORT={{PORT}}
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/bin/busybox", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:{{PORT}}/healthz"]
USER 65532:65532
ENTRYPOINT ["/app"]
`

const javaDockerfile = `FROM eclipse-temurin:21-jdk-alpine AS build
WORKDIR /src
COPY . .
RUN if [ -f gradlew ]; then chmod +x gradlew && ./gradlew --no-daemon build -x test; elif [ -f mvnw ]; then chmod +x mvnw && ./mvnw -DskipTests package; elif [ -f pom.xml ]; then mvn -DskipTests package; else gradle --no-daemon build -x test; fi && find . \( -path '*/target/*.jar' -o -path '*/build/libs/*.jar' \) | grep -v -E '(sources|javadoc|plain)\.jar$' | head -n 1 | xargs -r -I{} cp '{}' /app.jar

FROM eclipse-temurin:21-jre-alpine
RUN apk add --no-cache busybox-extras
COPY --from=build /app.jar /app.jar
ENV SERVER_PORT={{PORT}} PORT={{PORT}}
EXPOSE {{PORT}}
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:{{PORT}}/healthz || exit 1
USER 10001:10001
ENTRYPOINT ["java", "-jar", "/app.jar"]
`
