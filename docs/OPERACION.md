# Operación del host (Fase 0)

MiniDock debe ejecutarse con un usuario operativo dedicado, sin acceso SSH por
contraseña. El panel se mantiene solo en red local/VPN hasta que se elija un
método de acceso público.

## Validación de preparación

En la Mac mini, ejecuta:

```sh
chmod +x scripts/validate-host.sh scripts/backup.sh
MINIDOCK_DATA_PATH="$HOME/minidock" scripts/validate-host.sh
```

El script verifica Git, Docker, Docker Compose, el daemon y una ejecución real
de contenedor; además crea `apps`, `logs` y `backups` con permisos `0700`.

Antes de exponer el servidor, confirma manualmente:

- SSH usa llaves y `PasswordAuthentication no`; el usuario operativo no es
  administrador salvo el acceso imprescindible a Docker.
- Cada dominio apunta a la IP pública, o se resuelve por DNS local/VPN. Caddy
  solo debe solicitar TLS público cuando los puertos 80 y 443 estén publicados.
- `MINIDOCK_DOMAIN` está definido y `docker compose up --build` devuelve
  respuesta en `/healthz`.

## Backups y recuperación

Monta un volumen externo o remoto y ejecuta diariamente:

```sh
MINIDOCK_DATA_PATH="$HOME/minidock" MINIDOCK_BACKUP_PATH="/Volumes/backups/minidock" scripts/backup.sh
```

El backup incluye la base SQLite, repositorios, logs y configuración persistida.
Conserva el archivo `.sha256` y prueba trimestralmente una restauración en otro
directorio antes de sustituir el servicio.

> El socket Docker equivale a privilegios de administrador. El Compose de
> MiniDock lo monta únicamente para permitir despliegues; no publiques el panel
> sin una capa de red autenticada (VPN, Tailscale o equivalente).

## Orígenes de código locales y GitHub privado

Para repositorios locales, MiniDock usa por defecto
la carpeta de usuario `~` y permite crear subcarpetas desde el explorador.
También puedes configurar una ruta distinta. Crea un directorio dedicado que contenga solo los
checkouts que MiniDock puede leer. Configura su ruta con
`MINIDOCK_LOCAL_REPOSITORIES_PATH`; en Docker Compose define además
`MINIDOCK_LOCAL_REPOSITORIES_PATH_HOST` para montarlo como
`/repos`. Registra el origen con formato `file:///repos/nombre`. MiniDock
resuelve enlaces simbólicos y rechaza cualquier ruta que salga del directorio
permitido.

Para repositorios privados de GitHub, crea una GitHub App con permiso
**Contents: Read**, instálala únicamente en los repositorios necesarios y
monta su clave privada de solo lectura en el contenedor. Configura
`MINIDOCK_GITHUB_APP_ID` y `MINIDOCK_GITHUB_APP_PRIVATE_KEY_PATH`, e indica
el ID de instalación al registrar la aplicación. MiniDock intercambia la
clave por un token de instalación temporal para cada clon/fetch; no persiste
ese token. Una referencia puede ser una rama, un tag o un ref Git resoluble.
Si usas Compose, la ruta debe existir dentro del contenedor; añade un override
como el siguiente y usa esa ruta en `MINIDOCK_GITHUB_APP_PRIVATE_KEY_PATH`:

```yaml
services:
  minidock:
    volumes:
      - /ruta/segura/github-app.pem:/run/secrets/github-app.pem:ro
```

## Aceptación de secretos y configuración (Fase 2)

Con una aplicación de prueba y Docker seleccionado como runtime:

1. Abre **Gestionar secretos** y elige `production / build`.
2. Añade una configuración pública, por ejemplo `PUBLIC_LABEL=acceptance`, y
   un secreto, por ejemplo `BUILD_TOKEN=<valor temporal>`.
3. El Dockerfile de prueba debe declarar la sintaxis BuildKit y consumir el
   secreto sin copiarlo a una capa:

   ```Dockerfile
   # syntax=docker/dockerfile:1
   FROM alpine:3.21
   ARG PUBLIC_LABEL
   RUN --mount=type=secret,id=BUILD_TOKEN test "$PUBLIC_LABEL" = acceptance && test -s /run/secrets/BUILD_TOKEN
   CMD ["sleep", "infinity"]
   ```

4. Despliega y confirma que el build termina correctamente; revisa sus logs e
   inspecciona la imagen para confirmar que nunca contienen `BUILD_TOKEN`.
   Añade un secreto de `runtime` si la aplicación necesita recibirlo al iniciar.
5. Registra el resultado de la comprobación aquí y cambia Fase 2 a
   **Completada** en `docs/PROGRESO.md`.

Apple Container puede recibir secretos de ejecución, pero los secretos de
build requieren Docker BuildKit; MiniDock rechaza ese despliegue si se elige
Apple Container para no degradar la protección del secreto.

## Aceptación de CI/CD (Fase 3)

1. Define `MINIDOCK_GITHUB_WEBHOOK_SECRET` con un valor aleatorio y reinicia
   MiniDock. Opcionalmente, define `MINIDOCK_NOTIFICATION_WEBHOOK` con el URL
   de un webhook de Slack, Discord u otro receptor compatible con JSON.
   Desbloquea el panel después de cada reinicio: la clave maestra permanece
   únicamente en memoria y los trabajos necesitan acceder a secretos de build
   o runtime de forma segura.
2. En GitHub, añade un webhook de tipo `application/json` para
   `https://<panel>/webhooks/github/<ID_DE_LA_APLICACION>`, usa el mismo
   secreto y selecciona únicamente el evento **Push**.
3. Haz push a la rama registrada de la aplicación. GitHub debe recibir `202`,
   el panel debe mostrar el trabajo como `queued` y luego como `successful` o
   `failed`. Los pushes de otras ramas se ignoran.
4. Durante un despliegue, envía otro push: solo habrá un trabajo activo para
   esa aplicación. El segundo webhook recibe `202` porque el trabajo ya está
   protegido contra duplicados.
5. Tras dos despliegues exitosos, pulsa **Rollback a última imagen exitosa**;
   comprueba que inicia la imagen exitosa anterior (no la que está activa) y
   que el resultado aparece con acción `rollback`.

## Aceptación de plantillas de runtime (Fase 4)

Para cada tipo integrado (Estática, Next.js, Go, Rust y Java), registra una
aplicación sin `Dockerfile` en su repositorio y despliega con Docker. La
aplicación debe escuchar el puerto registrado y responder `200` en `/healthz`.

1. Comprueba que el repositorio clonado sigue sin `Dockerfile`: MiniDock crea
   un archivo temporal y no debe versionar ni modificar archivos de la app.
2. Confirma el health check con `docker inspect --format '{{.State.Health.Status}}' minidock-<nombre>`; debe ser `healthy`.
3. Confirma los límites con `docker inspect --format '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}' minidock-<nombre>`.
   Los valores por familia son: estática `0.50 CPU / 128 MiB`, Next.js `0.75 /
   512 MiB`, Go y Rust `0.50 / 256 MiB`, y Java `1.00 / 768 MiB`.
4. Para Next.js, establece `output: "standalone"` en su configuración. Para
   Java, proporciona `gradlew`, `mvnw`, `pom.xml` o una instalación Gradle
   compatible. Go y Rust deben construir un único binario ejecutable desde la
   raíz del proyecto.
5. Registra el resultado y cambia Fase 4 a **Completada** en
   `docs/PROGRESO.md` solo cuando las cinco comprobaciones hayan pasado.

## Aceptación de historial y logs (primer incremento de Fase 5)

1. En una aplicación de prueba, encola un despliegue válido y espera a que el
   historial indique `successful`. Encola después uno que falle de forma
   controlada y confirma el estado `failed`.
2. Abre **Ver log** en cada fila. Debe mostrarse el archivo de ese trabajo,
   incluido el error del segundo caso; el enlace de logs actuales del
   contenedor puede ser distinto, pues refleja solo la instancia activa.
3. Con una sesión autorizada, cambia en la URL el ID de aplicación por el de
   otra aplicación. MiniDock debe responder `404`, aun si el ID de despliegue
   existe. Sin sesión, debe redirigir a `/unlock`.
4. Anota el resultado en `docs/PROGRESO.md` antes de ampliar la Fase 5 con
   versión desplegada y métricas de recursos.

## Aceptación de estado y recursos (segundo incremento de Fase 5)

1. Con Docker y una aplicación desplegada, abre el detalle de la aplicación.
   **Estado actual** debe mostrar `running`, el health check, la imagen, la
   hora de inicio, CPU y memoria; **Última versión exitosa** debe coincidir con
   la imagen del último trabajo exitoso.
2. Compara la información con:

   ```sh
   docker inspect --format '{{.State.Status}} {{.Config.Image}}' minidock-<nombre>
   docker stats --no-stream minidock-<nombre>
   ```

3. Detén el contenedor y vuelve a cargar la página. Debe seguir mostrando el
   historial y una indicación de que la instantánea actual no está disponible,
   sin devolver un error del servidor.
4. Con Apple Container, confirma que el panel comunica que las métricas no
   están integradas todavía y que los controles e historial siguen operativos.

## Aceptación final de operación y observabilidad (Fase 5)

1. Abre **Operación y observabilidad** desde el panel. Comprueba uso de disco,
   última actividad, estado, health, imagen, CPU y memoria de una aplicación
   Docker. Los valores deben coincidir con `docker inspect` y `docker stats`.
2. Provoca un fallo de despliegue controlado y detén una aplicación. El panel
   debe mostrar las alertas correspondientes, además de conservar los logs
   filtrados por aplicación y despliegue. Si se configura
   `MINIDOCK_NOTIFICATION_WEBHOOK`, confirma también la notificación de fallo.
3. Para un dominio público con TLS, verifica que no hay alerta cuando faltan
   más días que `MINIDOCK_CERTIFICATE_ALERT_DAYS`; comprueba después la alerta
   al usar un certificado cercano a su expiración. Dominios locales e IPs se
   excluyen deliberadamente de esta comprobación.
4. Antes de usar **Ejecutar limpieza ahora**, toma un backup. Configura valores
   de prueba bajos para `MINIDOCK_RETENTION_DAYS` y
   `MINIDOCK_RETAINED_IMAGES`, crea logs antiguos y confirma que se eliminan
   solo después de pulsar el botón. Confirma que la imagen de un contenedor en
   uso y las imágenes retenidas siguen disponibles.
5. Registra el resultado y cambia Fase 5 a **Completada** en
   `docs/PROGRESO.md` cuando todos los puntos aplicables hayan pasado.
