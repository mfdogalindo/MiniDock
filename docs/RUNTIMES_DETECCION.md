# Detección de runtimes y recetas de despliegue

Este documento define cómo MiniDock debe reconocer proyectos y proponer una
receta de despliegue. La detección es una ayuda: nunca ejecuta código del
proyecto, no instala dependencias y no sustituye la confirmación de quien lo
opera.

## Principios

- La inspección solo lee manifiestos y archivos convencionales, como
  `package.json`, `angular.json`, `go.mod`, `Cargo.toml`, `pom.xml`,
  `pyproject.toml`, `requirements.txt`, `manage.py` y `wsgi.py`.
- Cada resultado incluye tipo sugerido, evidencia, confianza y datos que aún
  deben confirmarse.
- La propuesta siempre se puede editar antes de crear la aplicación.
- Un `Dockerfile` existente tiene prioridad: MiniDock propone
  **Personalizado** y no lo reemplaza.
- Si no hay evidencia suficiente, MiniDock propone **Personalizado** en lugar
  de adivinar comandos de producción.
- Una carpeta local sin Git es un contexto de build válido. MiniDock no la
  clona, no hace `fetch` y no la modifica; cada despliegue usa su contenido
  actual.

## Datos que debe poder editar una receta

Las plantillas integradas cubren los casos comunes. Para ampliar soporte sin
proliferar Dockerfiles rígidos, una receta configurable debe conservar:

| Campo | Uso |
|---|---|
| Modo | Estático, servidor Node SSR, WSGI o ASGI. |
| Gestor de paquetes | npm, pnpm, Yarn, Bun, pip, Poetry, Maven, Gradle, Cargo o Go modules. |
| Comando de build | Construye el artefacto de producción. |
| Comando de inicio | Inicia el proceso que escucha el puerto configurado. |
| Directorio de salida | Directorio estático o artefacto generado. |
| Puerto y host | Valores inyectados por MiniDock; el servidor debe escuchar una interfaz accesible al contenedor. |
| Health check | Ruta y código de éxito esperados. |
| CPU y memoria | Límites predeterminados, ajustables por aplicación. |

Los secretos siguen separados de esta receta: build y runtime usan el sistema
cifrado existente y no se deben copiar a comandos ni a imágenes.

## Soporte actual

| Caso | Evidencia | Propuesta actual | Confirmación necesaria |
|---|---|---|---|
| Dockerfile propio | `Dockerfile` | Personalizado | El Dockerfile y health check son responsabilidad del proyecto. |
| Go | `go.mod` | API Go | Binario único y endpoint de health. |
| Rust | `Cargo.toml` | API Rust | Binario único y endpoint de health. |
| Java | Maven o Gradle | Java | JAR ejecutable y endpoint de health. |
| Next.js | Dependencia `next` | Next.js standalone | `output: "standalone"`. |
| Vite estático | Dependencia `vite` | Estática | `npm run build` debe producir `dist`. |
| Angular estático | `angular.json` o `@angular/core` | Angular estático | Build debe generar un `index.html` bajo `dist`. |
| Vite SSR con `server.js` | Vite, entrada SSR y `server.js` | Vite SSR (servidor Node) | MiniDock ejecuta `node server.js` con `NODE_ENV=production` y `PORT`; el build debe generar `dist/client` y `dist/server`. |
| Vite SSR genérico | Vite y script/entrada SSR sin `server.js` | Node SSR | `npm run start` debe arrancar el servidor productivo. |
| Angular SSR | Angular con `@angular/ssr`, Universal o script SSR | Node SSR | Verificar el script de inicio y health check. |
| Astro estático | Dependencia `astro` sin salida servidor | Astro estático | `npm run build` debe generar `dist`. |
| Astro SSR | `output: 'server'` y adaptador Node | Astro SSR | El adaptador `@astrojs/node` debe estar instalado. |
| Nuxt estático | Script `nuxt generate` o `ssr: false` | Nuxt estático | El proyecto debe generar `.output/public`. |
| Nuxt SSR | Dependencia `nuxt` | Nuxt SSR (Nitro) | Confirmar que el preset Node sea compatible. |
| SvelteKit estático | `adapter-static` | SvelteKit estático | El adaptador debe producir `build`. |
| SvelteKit SSR | `adapter-node` | SvelteKit SSR (Node) | El adaptador debe producir el servidor Node. |
| Carpeta sin Git | Directorio local permitido sin `.git` | Según manifiestos | No requiere rama; se construye directamente desde el directorio. |

Las referencias Git pueden ser ramas, tags o refs explícitos. Al elegir una
carpeta Git local, el explorador ofrece sus ramas y tags; una referencia escrita
manualmente sigue siendo válida si Git puede resolverla.

## Casos recomendados para la siguiente ampliación

### Astro

Astro puede ser estático o SSR. Su modo predeterminado es estático y genera
`dist`; para SSR necesita un adaptador correspondiente al runtime. Con el
adaptador Node, el artefacto se inicia con
`node ./dist/server/entry.mjs` y debe recibir `HOST=0.0.0.0` y el puerto
configurado. La detección debe leer `astro.config.*`, buscar `output: 'server'`
o un adaptador Node y distinguirlo del caso estático.

Fuente: [receta oficial Docker de Astro](https://docs.astro.build/en/recipes/docker/)
y [renderizado bajo demanda de Astro](https://docs.astro.build/en/guides/on-demand-rendering/).

### Nuxt

Nuxt puede generar estáticos o un servidor Node mediante Nitro. Para el preset
`node-server`, el punto de entrada es `.output/server/index.mjs`; acepta
`PORT`/`NITRO_PORT` y `HOST`/`NITRO_HOST`. La detección debe leer
`nuxt.config.*`, `package.json` y los scripts para diferenciar `nuxt generate`
de `nuxt build`. La receta SSR debe usar `NITRO_PRESET=node-server` cuando no
esté fijado por el proyecto.

Fuente: [despliegue oficial de Nuxt](https://nuxt.com/docs/3.x/getting-started/deployment).

### SvelteKit

SvelteKit requiere un adaptador que determina su modo de salida. Con
`@sveltejs/adapter-static` se propone un servidor estático; con
`@sveltejs/adapter-node`, un servidor Node autónomo. La detección debe leer
`svelte.config.*` y `package.json`, y no asumir SSR solo por encontrar
`@sveltejs/kit`.

Fuente: [adaptadores oficiales de SvelteKit](https://svelte.dev/packages).

### Python: Django, FastAPI y Flask

Python necesita más confirmación que los casos Node porque el módulo de
arranque puede variar entre proyectos.

- **Django**: detectar `manage.py` y un archivo `wsgi.py` o `asgi.py`; proponer
  WSGI con Gunicorn o ASGI con Uvicorn, solicitando el módulo, por ejemplo
  `proyecto.wsgi:application`. También debe definirse la estrategia de
  `collectstatic` y la ruta de health check.
- **FastAPI**: detectar `fastapi` en `pyproject.toml` o `requirements.txt`;
  solicitar el módulo ASGI, por ejemplo `app.main:app`, y ejecutar Uvicorn o
  Gunicorn con worker Uvicorn.
- **Flask**: detectar `flask`; solicitar el objeto WSGI, por ejemplo
  `app:app`, y ejecutar Gunicorn en producción.

La documentación de Django identifica WSGI como su plataforma principal y el
objeto `application` como contrato de arranque; MiniDock no debe ejecutar
`runserver` en producción.

Fuente: [despliegue WSGI de Django](https://docs.djangoproject.com/en/4.2/howto/deployment/wsgi/).

## Orden de implementación recomendado

1. Añadir la receta editable por aplicación: gestor, build, inicio, salida y
   health check.
2. Añadir detección de gestores de paquetes y mostrar las evidencias en el
   asistente antes de aplicar una propuesta.
3. Añadir Python WSGI/ASGI con una pantalla de confirmación explícita para el
   módulo de aplicación y los archivos estáticos.
4. Mantener **Personalizado** como salida segura para monorepos, proyectos con
   artefactos fuera de los convencionales o configuraciones no concluyentes.

## Criterios de aceptación

Cada runtime nuevo debe verificarse con un proyecto real y una prueba que
compruebe, como mínimo:

1. La detección correcta a partir de manifiestos, sin ejecutar scripts.
2. Que la receta generada use el artefacto y comando de producción correctos.
3. Que `PORT` y `HOST` sean respetados por el proceso.
4. Que el health check responda según la ruta configurada.
5. Que el repositorio y, en particular, las carpetas locales sin Git, no sean
   modificados por MiniDock.
