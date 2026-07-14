# MiniDock

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

## Límites actuales

Esta entrega no monta el socket Docker ni ejecuta despliegues todavía. Eso se
añadirá cuando definamos el modelo de aplicaciones y la política de permisos
del ejecutor.
