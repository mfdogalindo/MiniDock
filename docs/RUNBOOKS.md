# Runbooks de operación de MiniDock

Estos procedimientos ayudan a diagnosticar incidentes conocidos. El flujo de
backup y restore cifrado está validado en un simulacro automatizado; consulta
la evidencia de `MD-P0-02` en [PLAN_MEJORAS.md](PLAN_MEJORAS.md).

---

## 1. Incidente de Docker o Caddy

### Síntomas
* MiniDock muestra errores al intentar usar el runtime a través de
  `docker-socket-proxy`.
* Los logs de build fallan inmediatamente en la etapa `build` o `start`.
* Los dominios configurados de las aplicaciones devuelven un error HTTP 502/503 o no resuelven, mientras que los contenedores están corriendo.

### Diagnóstico
1. **Verificar socket Docker:** Comprueba que el daemon de Docker esté activo y responda en el host:
   ```bash
   docker info
   docker ps
   ```
2. **Revisar el único montaje permitido:** MiniDock no debe tener acceso al
   socket. Comprueba que únicamente `docker-socket-proxy` lo monte y que el
   endpoint TCP privado responda desde MiniDock:
   ```bash
   docker compose exec minidock docker info
   docker inspect $(docker compose ps -q minidock) --format '{{range .Mounts}}{{println .Source}}{{end}}'
   ```
3. **Revisar Caddy y la ruta controlada:** Caddy no observa etiquetas Docker.
   MiniDock reemplaza la ruta mediante su Admin API privada y verifica la
   aplicación a través del proxy:
   ```bash
   docker compose logs caddy
   docker compose exec minidock wget -qO- http://127.0.0.1:8080/runtimez
   ```
4. **Verificar la red de MiniDock:** Confirma que la red puente compartida existe:
   ```bash
   docker network ls | grep minidock
   ```

### Mitigación
* **Si Docker no responde:** Reinicia el servicio en el host:
   ```bash
   # En Linux/systemd
   sudo systemctl restart docker
   
   # En macOS (si usas OrbStack o Docker Desktop)
   orbstack stop && orbstack start
   ```
* **Si la red `minidock` no existe:** MiniDock la crea automáticamente, pero puedes forzar su creación manual:
   ```bash
   docker network create minidock
   ```
* **Si Caddy no enruta:** No habilites etiquetas ni expongas el puerto 2019.
  Reiniciar Caddy restaura el `Caddyfile` base; MiniDock reconcilia las rutas
  registradas al iniciar. Comprueba primero `/runtimez` y el informe de release:
   ```bash
   docker compose restart caddy
   ```

---

## 2. Disco Lleno (Disk Full)

### Síntomas
* Las compilaciones fallan con errores del tipo `no space left on device`.
* La base de datos SQLite de MiniDock se bloquea o arroja errores de escritura.
* El panel de **Operación y observabilidad** muestra una alerta crítica por poco espacio disponible (<2GB).

### Diagnóstico
1. **Comprobar espacio del sistema de archivos:**
   ```bash
   df -h
   ```
2. **Identificar consumo de Docker:**
   ```bash
   docker system df
   ```
3. **Identificar directorios de MiniDock pesados (workspaces o logs):**
   ```bash
   du -sh ~/minidock/*
   ```

### Mitigación
1. **Limpieza automática desde la UI:**
   * Ve a **Operación y observabilidad** -> **Ejecutar limpieza ahora** para purgar logs antiguos y retener solo las últimas 3 imágenes exitosas de cada aplicación.
2. **Limpieza profunda de Docker:**
   * Elimina imágenes, contenedores detenidos y cachés de compilación no utilizados:
     ```bash
     docker system prune -a --volumes -f
     ```
     > [!WARNING]
     > Esto eliminará todas las imágenes de Docker que no estén siendo utilizadas actualmente por un contenedor en ejecución. MiniDock mantendrá los contenedores en marcha, pero reconstruirá las imágenes en el próximo despliegue.
3. **Truncado de logs del host:**
   * Si los archivos de logs de despliegues pasados ocupan mucho espacio, puedes purgar manualmente logs de más de 30 días:
     ```bash
     find ~/minidock/logs -name "*.log" -mtime +30 -delete
     ```

---

## 3. Problema de Certificados (TLS/HTTPS)

### Síntomas
* El navegador muestra advertencias de certificado caducado o no seguro en los dominios de las aplicaciones.
* MiniDock emite una alerta en la UI indicando que los certificados del dominio están por expirar o no son válidos.

### Diagnóstico
1. **Comprobar resolución DNS:** Asegúrate de que el dominio resuelve a la IP pública correcta del host:
   ```bash
   dig +short app.ejemplo.com
   ```
2. **Comprobar puertos abiertos:** Caddy requiere que los puertos `80` (para el reto HTTP-01 de Let's Encrypt) y `443` estén abiertos al exterior.
3. **Inspeccionar logs de TLS en Caddy:**
   ```bash
   docker compose logs caddy | grep -E "challenge|acme|cert"
   ```

### Mitigación
* **Si el puerto 80/443 está bloqueado:** Configura el firewall del host o las reglas de port forwarding del router para permitir el tráfico entrante.
* **Si es un dominio local (desarrollo):** Asegura el uso de certificados locales autofirmados o añade el certificado raíz de desarrollo al llavero del sistema de los clientes.
* **Forzar renovación en Caddy:** Si el DNS y los puertos son correctos pero Caddy se ha quedado atascado, reinícialo:
   ```bash
   docker compose restart caddy
   ```

---

## 4. Secreto Bloqueado (Master Key Locked)

### Síntomas
* Los webhooks de GitHub devuelven un error HTTP `503 Service Unavailable` al recibir pushes.
* GitHub recibe `429 Too Many Requests` con `Retry-After` durante una ráfaga de
  pushes correctamente firmados.
* El pipeline se detiene en seco porque no se pueden descifrar las variables secretas del repositorio o de la app.
* Logs del sistema registran el mensaje `[AUDIT] Master key is locked. Deployment aborted`.

### Diagnóstico
1. Visita el panel de administración. Si te redirige a `/unlock` o muestra un candado cerrado, la clave maestra en memoria se ha perdido (común después de un reinicio del host o del proceso de MiniDock).

### Mitigación
* **Operación asistida (Manual):**
  1. Accede a la URL `/unlock` en el navegador o panel local.
  2. Introduce la contraseña maestra del sistema para derivar la clave de descifrado KMS en memoria.
  3. Tras la verificación, MiniDock vuelve a operar normalmente.
* **Si GitHub recibe `429`:** espera el valor de `Retry-After` y reintenta. El
  límite se conserva en SQLite por aplicación y se aplica después de validar la
  firma HMAC; no intentes evitarlo cambiando `X-Forwarded-For`.

---

## 5. Job Abandonado o Colgado

### Síntomas
* Un despliegue se queda en estado `running` indefinidamente en la UI.
* Las peticiones de despliegues nuevos se quedan en `queued` porque la aplicación solo permite un trabajo activo simultáneo.

### Diagnóstico
1. Si el servidor de MiniDock sufrió un reinicio abrupto, el reconciliador interno al arrancar debería haber marcado el trabajo como `failed` (`start/worker_restarted`).
2. Si el proceso sigue vivo pero bloqueado (por ejemplo, un comando de compilación interactivo esperando input o un fetch de Git bloqueado), inspecciona el lease de la base de datos.

### Mitigación
1. **Cancelar desde la UI:**
   * Haz clic en **Cancelar entrega** en el detalle del despliegue colgado. Esto enviará una señal de cancelación al hilo de ejecución.
2. **Liberación manual (si el panel está atascado):**
   * Detén y arranca el contenedor o proceso de MiniDock. El worker de arranque liberará automáticamente todos los leases huérfanos.
3. **Matar procesos huérfanos de Docker:**
   * Busca contenedores temporales de compilación huérfanos y elimínalos:
     ```bash
     docker ps -a --filter "name=minidock-build-"
     docker rm -f <container_id>
     ```

---

## 6. Restauración completa

### Síntomas
* Pérdida total del host por fallo de hardware, corrupción de disco, o necesidad de migrar la instancia completa de MiniDock a un nuevo host.

### Requisitos previos
* Un `.mdbk` completo creado con `scripts/backup.sh`, su `.sha256` y su
  manifiesto lateral `.kms.json` (no secreto), además de la contraseña de KMS.

### Paso a paso de la restauración
1. **Preparar el nuevo host:**
   * Ejecuta `scripts/validate-host.sh` para comprobar que el nuevo host cuenta con Git, Docker y permisos de usuario correctos.
2. **Restaurar archivos de datos:**
   * Copia el `.mdbk`, `.sha256` y `.kms.json` al host de destino. Verifica la
     suma y restaura en un directorio nuevo:
     ```bash
     scripts/restore-backup.sh minidock-YYYYMMDDTHHMMSSZ.mdbk ~/minidock
     ```
3. **Verificar antes de reemplazar datos:**
   * Comprueba el `.sha256`, restaura en un directorio aislado y conserva una
     copia de los datos actuales. No continúes si el archivo no verifica o no
     contiene la base y configuración esperadas.
4. **Levantar el servicio:**
   * Vuelve a levantar la pila de MiniDock usando Docker Compose:
     ```bash
     docker compose up -d --build
     ```
5. **Desbloquear:**
   * Accede a la UI y desbloquea el panel para restaurar la operatividad completa de las automatizaciones y despliegues con secretos.

El simulacro automatizado cubre backup y restore completo en un segundo
directorio, autenticidad, integridad SQLite y permisos. En el host real repite
el procedimiento antes de aprobar un cambio de almacenamiento o versión.
