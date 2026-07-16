# Operación y aceptación del host

> Esta guía define comprobaciones; no certifica que ya hayan pasado. Consulta
> [ESTADO.md](ESTADO.md) y registra evidencia en el paquete correspondiente de
> [PLAN_MEJORAS.md](PLAN_MEJORAS.md).

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
- Ejecuta `scripts/prepare-runtime-network.sh` antes de `docker compose up --build`.
  La red de workloads debe ser `bridge`, `internal=true` y usar la subred
  configurada. MiniDock no debe tener montado `/var/run/docker.sock`; solo
  `docker-socket-proxy` puede montarlo de solo lectura.
- En Linux Docker Engine, después de iniciar Caddy ejecuta
  `sudo scripts/harden-runtime-firewall.sh install` y confirma con `check`.
  La regla permite únicamente Caddy → workload y deniega salida, movimiento
  lateral, metadata y acceso al plano de control. Docker Desktop/OrbStack
  requieren una implementación equivalente dentro de la VM.
- `MINIDOCK_DOMAIN` está definido y `docker compose up --build` devuelve
  respuesta en `/healthz`. Después de configurar y desbloquear el panel,
  `curl --fail http://127.0.0.1:8080/runtimez` desde la red Docker debe
  responder `{"status":"ready"}`. Si falla por runtime, confirma que
  `DOCKER_HOST` apunta al proxy TCP privado y revisa sus ACL, sin volver a
  montar el socket en MiniDock.
- El proxy de socket reduce la superficie del daemon, pero una ACL por familia
  de endpoint no puede validar el cuerpo de `containers/create` o `build`.
  Antes de aceptar una exposición pública, añade un plugin de autorización o
  broker que rechace privilegios, montajes host, red host, dispositivos,
  capacidades y nombres/redes fuera del prefijo de MiniDock.

Para repetir la aceptación completa desde una copia limpia del repositorio:

```sh
MINIDOCK_E2E_REPORT=/tmp/minidock-md-p0-01.json scripts/e2e-compose.sh
```

Debe producir un JSON con `"result":"passed"`; el archivo `.log` adyacente
contiene los logs de Compose y no debe subirse si incluye datos operativos.

## Backups y recuperación

Monta un volumen externo o remoto y ejecuta diariamente:

```sh
MINIDOCK_DATA_PATH="$HOME/minidock" MINIDOCK_BACKUP_PATH="/Volumes/backups/minidock" scripts/backup.sh
```

El script crea una copia completa cifrada `minidock-*.mdbk`, más su suma
`.sha256` y un manifiesto lateral `.kms.json`. El manifiesto contiene solo salt
y verificador del KMS —no la contraseña— y es necesario para recuperar en otro
host. Conserva los tres archivos juntos; el `.mdbk` incluye SQLite (como
`database.mdbk`), repositorios, logs y configuración persistida, sin dejar la
base en claro en el volumen de respaldo.

Para un simulacro completo en otro volumen o host, con MiniDock detenido:

```sh
scripts/restore-backup.sh /Volumes/backups/minidock/minidock-AAAAMMDDTHHMMSSZ.mdbk /Volumes/restore/minidock
```

El script autentica y descomprime primero hacia un directorio temporal, exige
que el destino no exista y restaura SQLite con permisos `0600`. Confirma después
que el panel inicia, ejecuta migraciones y puede desbloquearse desde la UI antes
de dirigir tráfico al host restaurado.

El backup de base administrado por MiniDock usa el formato autenticado y
versionado `.mdbk`; requiere desbloquear el proveedor de claves desde la UI y
nunca crea un fallback en texto plano. Para una recuperación, con MiniDock
detenido y el manifiesto `.kms.json` correspondiente disponible:

Para enviar el backup de base programado directamente a S3 o MinIO, configura
`MINIDOCK_BACKUP_PROVIDER=s3` (o `minio`), el bucket y las credenciales S3.
MiniDock genera la instantánea SQLite en memoria, la cifra y la transmite al
objeto remoto sin crear el anterior `temp-*.db` en el VPS. El respaldo completo
de `scripts/backup.sh` sigue siendo una operación local independiente.

```sh
read -rs MINIDOCK_KMS_PASSWORD; printf '%s' "$MINIDOCK_KMS_PASSWORD" | minidock verify --kms-config /ruta/backup.mdbk.kms.json --file /ruta/backup.mdbk --password-stdin; unset MINIDOCK_KMS_PASSWORD
read -rs MINIDOCK_KMS_PASSWORD; printf '%s' "$MINIDOCK_KMS_PASSWORD" | minidock restore --kms-config /ruta/backup.mdbk.kms.json --file /ruta/backup.mdbk --destination /volumen-nuevo/minidock.db --password-stdin; unset MINIDOCK_KMS_PASSWORD
```

`restore` exige que el destino no exista, comprueba autenticidad e integridad
SQLite en un directorio temporal antes de escribirlo y conserva permisos `0600`.
No pongas la contraseña en el historial: usa una entrada interactiva o un
gestor de secretos que alimente la entrada estándar.

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

## Aceptación de secretos y configuración

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
   En Docker, MiniDock materializa los valores runtime en un archivo temporal
   `0600` y usa `--env-file`; lo borra al terminar de invocar Docker para que
   ni la línea de comandos ni el log del deploy contengan los valores.
5. Registra la evidencia en `MD-P1-02` de
   [PLAN_MEJORAS.md](PLAN_MEJORAS.md) y actualiza [ESTADO.md](ESTADO.md) si
   cambia el estado validado.

Apple Container sigue siendo experimental. Puede recibir secretos de ejecución,
pero no ofrece todavía el contrato de archivo temporal de Docker; los secretos
de build requieren Docker BuildKit y MiniDock rechaza ese despliegue si se
elige Apple Container para no degradar la protección del secreto.

## Puerta de aceptación reproducible

Antes de aprobar una versión en la Mac mini real, con Docker/OrbStack iniciado,
los puertos 80 y 443 libres y `jq` instalado, ejecuta:

```sh
MINIDOCK_E2E_REPORT=/tmp/minidock-acceptance.json scripts/e2e-compose.sh
```

El resultado debe ser `0`; el JSON debe tener `result: "passed"` y cinco
releases: dos `deploy` exitosos, uno fallido deliberadamente, uno `cancelled`
y un `rollback` exitoso. Cada release exitoso debe contener `source_revision`,
`source_fingerprint`, `artifact_digest`, `configuration_digest`, runtime,
manifiesto y log. Conserva ese JSON y el `.log` adyacente como evidencia de la
aceptación. El script no acepta únicamente `/healthz`: además verifica la ruta
del dominio del fixture a través de Caddy tras cada transición.

No apruebes la versión si el informe falta, si el rollback no vuelve a servir
la primera versión del fixture, o si el informe contiene un digest/huella vacío.
El script limpia Compose y restaura los archivos del fixture incluso al fallar.

## Aceptación de CI/CD

1. Define `MINIDOCK_GITHUB_WEBHOOK_SECRET` con un valor aleatorio y reinicia
   MiniDock. Opcionalmente, define `MINIDOCK_NOTIFICATION_WEBHOOK` con el URL
   de un webhook de Slack, Discord u otro receptor compatible con JSON.
   Desbloquea el panel después de cada reinicio: la clave maestra permanece
   únicamente en memoria y los trabajos necesitan acceder a secretos de build
   o runtime de forma segura.
   El límite predeterminado admite 30 pushes válidos por aplicación y minuto;
   ajusta `MINIDOCK_GITHUB_WEBHOOK_RATE_LIMIT` y
   `MINIDOCK_GITHUB_WEBHOOK_RATE_WINDOW` solo si la cadencia esperada lo exige.
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

## Conmutación y recuperación de releases

Hasta que el propietario exija conmutación sin corte, MiniDock opera con
**downtime breve y acotado**: valida un contenedor candidato, retira el activo
y crea el contenedor de producción. El periodo empieza al retirar el activo y
termina cuando el health y la ruta del proxy vuelven a verificarse; sus etapas
quedan en el historial del deploy. No se debe anunciar disponibilidad continua
durante esta modalidad.

Cada release guarda un manifiesto no secreto que incluye la configuración
pública efectiva y, para Docker, el ID de contenedor observado después de una
conmutación saludable. El informe descargable de release es la evidencia a
conservar durante un incidente. No inspecciones variables de entorno del
contenedor para recuperar configuración: pueden contener secretos.

Durante el cambio, el contenedor activo se renombra a
`minidock-<app>-previous` y se detiene; solo se elimina cuando el nuevo
contenedor pasa health y la verificación de Caddy. Si MiniDock muere entre
ambos pasos, el arranque siguiente elimina candidatos y hace una de estas dos
acciones: restaura y renombra el contenedor anterior si falta el primario, o
elimina el anterior detenido si el primario ya existe. Esta restauración usa
el contenedor preservado, por lo que no reconstruye su entorno efectivo.

El rollback manual/automático exige imagen y digest registrados y usa la
configuración pública registrada en el release objetivo; conserva únicamente
los secretos actualmente desbloqueados. Si la imagen, el manifiesto o health
de restauración no son válidos, el job queda como `failed` con un código
normalizado (`artifact_missing`, `artifact_digest_mismatch` o
`rollback_restore_failed`), nunca solo como texto de log.

## Aceptación de plantillas de runtime

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
5. Registra la evidencia en `MD-P0-04` de
   [PLAN_MEJORAS.md](PLAN_MEJORAS.md) y actualiza [ESTADO.md](ESTADO.md) solo
   cuando las cinco comprobaciones hayan pasado.

## Aceptación de historial y logs

1. En una aplicación de prueba, encola un despliegue válido y espera a que el
   historial indique `successful`. Encola después uno que falle de forma
   controlada y confirma el estado `failed`.
2. Abre **Ver log** en cada fila. Debe mostrarse el archivo de ese trabajo,
   incluido el error del segundo caso; el enlace de logs actuales del
   contenedor puede ser distinto, pues refleja solo la instancia activa.
3. Con una sesión autorizada, cambia en la URL el ID de aplicación por el de
   otra aplicación. MiniDock debe responder `404`, aun si el ID de despliegue
   existe. Sin sesión, debe redirigir a `/unlock`.
4. Anota el resultado en `MD-P0-04` de
   [PLAN_MEJORAS.md](PLAN_MEJORAS.md) antes de ampliar el alcance de operación
   y actualiza [ESTADO.md](ESTADO.md).

## Aceptación de estado y recursos

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

## Aceptación de operación y observabilidad

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
5. Registra la evidencia en `MD-P1-03` de
   [PLAN_MEJORAS.md](PLAN_MEJORAS.md) y actualiza [ESTADO.md](ESTADO.md)
   cuando todos los puntos aplicables hayan pasado.
