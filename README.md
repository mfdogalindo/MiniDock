# MiniDock

> An open-source deployment control plane by [Kyberix](https://kyberix.co/en).

MiniDock es un servidor local de despliegues para una Mac mini. El daemon, la
API y el panel administrativo SSR están construidos en Go.

## Fase 0

La primera base incluye:

- Panel administrativo SSR sin JavaScript de cliente.
- SQLite para configuración y secretos cifrados.
- Asistente de primera ejecución para establecer la contraseña maestra.
- Derivación PBKDF2-SHA-256 con una sal aleatoria y 600 000 iteraciones.
- Cifrado autenticado AES-256-GCM para secretos.
- Bloqueo al reiniciar: la clave derivada vive solo en memoria.
- Docker Compose con Caddy como proxy inverso.

## Ejecutar localmente

```sh
cp .env.example .env
go mod tidy
./dev.sh
```

Abre `http://127.0.0.1:8080`. El primer acceso muestra el asistente de
configuración. La base de datos se crea en `./data/minidock.db` y está excluida
de Git. `dev.sh` ejecuta las pruebas y usa un observador para reconstruir y
reiniciar el servidor al modificar archivos Go o plantillas HTML. El observador está
implementado en Go y conserva la última instancia válida si un cambio no
compila. Recarga el navegador para ver el resultado SSR actualizado.

Para ejecutar una instancia sin observación de cambios:

```sh
go run ./cmd/minidock
```

## Ejecutar con Docker

```sh
docker compose up --build
```

Para un dominio público, establece `MINIDOCK_DOMAIN` en `.env` antes de iniciar
Compose. Caddy solicitará el certificado TLS cuando el dominio resuelva a esta
máquina y los puertos 80 y 443 sean accesibles.

Docker Desktop u OrbStack debe estar iniciado antes de ejecutar Compose.

## Progreso y secretos

El panel indica la fase activa y `docs/PROGRESO.md` conserva el control de
aceptación completo. La Fase 2 está lista para validación: cada aplicación
puede tener configuración pública y secretos separados por `production` o
`staging`, y por destino `build` o `runtime`. Los valores secretos se cifran,
no se muestran después de guardarlos, se pueden rotar o eliminar, y sus
operaciones quedan auditadas.

El despliegue manual usa la configuración y los secretos de `production`:
los valores de runtime llegan al contenedor como variables de entorno; los de
build se pasan a Docker BuildKit con `--secret`, no con `--build-arg`. Consulta
el checklist de aceptación en `docs/OPERACION.md` antes de marcar la fase como
completada.

## CI/CD (Fase 3)

MiniDock acepta pushes de GitHub en
`/webhooks/github/<ID_DE_LA_APLICACION>`. Define
`MINIDOCK_GITHUB_WEBHOOK_SECRET` con el mismo secreto configurado en GitHub;
solo los eventos `push` firmados para la rama registrada en la aplicación se
encolan. La cola persistente evita despliegues simultáneos de una misma
aplicación. Define opcionalmente `MINIDOCK_NOTIFICATION_WEBHOOK` para recibir
el resultado como JSON en Slack, Discord u otro webhook compatible.

El detalle de la aplicación permite encolar un despliegue manual o hacer
rollback a la última imagen exitosa. Consulta el checklist completo en
`docs/OPERACION.md` antes de marcar la Fase 3 como completada.

## Orígenes locales y repositorios privados

MiniDock no depende de GitHub: por defecto explora la carpeta de usuario `~`
(la carpeta equivalente en macOS, Linux o Windows). El explorador
permite crear subcarpetas desde el panel. También puedes configurar
`MINIDOCK_LOCAL_REPOSITORIES_PATH` y registrar la ruta como
`file:///ruta/del/repositorio`. Solo se aceptan rutas bajo ese directorio. En
Docker Compose monta el directorio del host con
`MINIDOCK_LOCAL_REPOSITORIES_PATH_HOST=/ruta/del/host`; se expone a MiniDock
como `/repos`, por lo que el formulario usa por ejemplo
`file:///repos/mi-servicio`. El montaje admite crear carpetas desde el panel;
configura una ruta dedicada si no quieres conceder escritura sobre todo tu
directorio de usuario.

Al elegir una carpeta Git, el asistente consulta sus ramas y tags para sugerir
la referencia a desplegar; también se puede escribir un ref manualmente. Una
carpeta con código que no tenga Git se puede elegir como **Usar código**: se
usa directamente como contexto de build, sin clonar, hacer fetch ni modificar
sus archivos. En ese caso no se requiere una rama.

Al seleccionar una carpeta local, MiniDock lee únicamente manifiestos como
`package.json`, `angular.json`, `go.mod`, `Cargo.toml` o `pom.xml`; no ejecuta
ningún script durante la detección. Propone Vite, Angular (estático o SSR),
Next.js, Go, Rust o Java. Las aplicaciones SSR de Vite y Angular usan la
plantilla **Node SSR (start)** y requieren comprobar que `npm run start` inicie
el servidor y responda en el puerto configurado. La sugerencia es editable;
usa **Personalizado** cuando el proyecto necesite un Dockerfile propio.

Para GitHub privado configura una GitHub App con permiso **Contents: Read**,
instálala solo en los repositorios necesarios y monta su clave privada de solo
lectura. Define `MINIDOCK_GITHUB_APP_ID` y
`MINIDOCK_GITHUB_APP_PRIVATE_KEY_PATH`; al registrar una aplicación indica el
ID de instalación. MiniDock crea tokens de instalación efímeros para Git y no
los guarda ni los escribe en el log. La referencia registrada puede ser una
rama, un tag o un ref.

## Plantillas de runtime (Fase 4)

Al registrar una aplicación, elige Estática, Next.js, Go, Rust o Java. MiniDock
genera durante el despliegue un Dockerfile temporal con una imagen multi-stage,
health check y límites de CPU/memoria; no añade infraestructura al repositorio.
Las plantillas esperan que el servicio escuche en el puerto registrado (8080
por defecto) y responda `GET /healthz`. Next.js debe habilitar
`output: "standalone"`. Las aplicaciones Java detectan Gradle o Maven. El tipo
Personalizado conserva el comportamiento anterior y requiere un `Dockerfile`.

## Operación y observabilidad (Fase 5)

El historial de cada aplicación conserva el estado de cada trabajo y un enlace
al log capturado durante ese despliegue. Es distinto de los logs actuales del
contenedor: el primero permanece asociado a su build/rollback incluso si luego
se reinicia o sustituye la aplicación. Los logs de despliegue requieren una
sesión desbloqueada y MiniDock solo los sirve desde `MINIDOCK_LOG_PATH`.

Para aplicaciones Docker, el detalle también muestra la última versión exitosa
y una instantánea actual de estado, health check, CPU y memoria. Apple
Container sigue mostrando historial y controles, pero sus métricas aún no se
integran en el panel.

El enlace **Operación y observabilidad** concentra alertas de despliegue,
disponibilidad, disco y certificados TLS, además de una limpieza manual de
logs/registros antiguos e imágenes Docker no retenidas. Los umbrales y la
retención se configuran en `.env.example`; revisa el checklist antes de activar
la limpieza en el host.
