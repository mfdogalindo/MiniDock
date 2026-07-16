# Plan de mejoras y Hoja de Ruta — MiniDock

Última actualización: 2026-07-21. Este documento define la secuencia de trabajo oficial para alcanzar la primera versión de producción de MiniDock en un único servidor Mac mini.

## Decisiones Arquitectónicas Confirmadas

1. **Servidor Único**: Mac mini con Docker y Caddy proxy.
2. **Modo de Desbloqueo**: Únicamente **operación asistida (manual)** tras el arranque. Se ha eliminado por completo la configuración y código muerto de desbloqueo automático (`AutoUnlock`).
3. **Acceso al Panel**: Exclusivamente mediante red local (LAN) o VPN (WireGuard/Tailscale). Nunca expuesto directamente a Internet público.
4. **Scope Excluido de V1**: Apple Container, soporte multi-servidor, proveedores Git adicionales y rediseños visuales mayores se posponen para versiones posteriores.

---

## Fases de Ejecución

### Fase 0 — Estabilizar la base
- **Estado**: `validado`
- **Objetivo**: Consolidar repositorio, resolver inconsistencias de documentación y eliminar código muerto de auto-desbloqueo.
- **Entregables**: Repositorio limpio, documentación coherente en `README.md`, `docs/ESTADO.md` y `docs/PLAN_MEJORAS.md`, y controles de calidad verdes (`go test ./...`, `go vet ./...`, `docker compose config --quiet`).

### Fase 1 — Validar el camino real
- **Estado**: `validado`
- **Objetivo**: Garantizar el despliegue end-to-end en una Mac mini limpia con `scripts/e2e-compose.sh`.
- **Entregables**:
  - Script e2e completo pasando con resultado `passed`.
  - Verificaciones preflight en el panel antes del deploy: estado de Docker, estado de Caddy, red de workloads, espacio libre en disco, acceso al repositorio, validez de dominio y health endpoint.
- **Evidencia** (2026-07-22, OrbStack 29.4.0): `scripts/e2e-compose.sh` terminó con `MD-P0-04 Compose acceptance passed`; informe en `tmp/md-p0-04-e2e.json` y log en `tmp/md-p0-04-e2e.log`.

### Fase 2 — Releases recuperables
- **Estado**: `validado`
- **Objetivo**: Hacer que el despliegue, redeploy, cancelación y rollback sean idempotentes y atómicos.
- **Entregables**:
  - Persistir por release el estado deseado y observado (imagen, digest, configuración pública, ID contenedor).
  - Transición Caddy sin downtime destructivo (mantener contenedor anterior vivo hasta verificar el nuevo candidate).
  - Reconciliación automática al iniciar MiniDock tras reinicios o caídas de proceso.
  - Pruebas con fault injection.

### Fase 3 — Seguridad operativa
- **Estado**: `validado`
- **Objetivo**: Cerrar el modelo de seguridad y limitar el vector de ataque del daemon Docker.
- **Entregables**:
  - Panel restringido a loopback/LAN/VPN.
  - Proxy/broker de autorización delante del socket Docker (`ValidateDockerContainerSecurityArgs` rechazando privileged, host mounts, host network, etc.).
  - Prueba canario para verificar la no-fuga de secretos en logs, argumentos, webhooks y metadatos (`TestCanarySecretNoLeak`).
  - Rotación de contraseña maestra (`UpdateSecurityConfig`) sin pérdida de secretos.

### Fase 4 — Backup y observabilidad
- **Estado**: `validado`
- **Objetivo**: Garantizar backup cifrado y restauración verificable.
- **Entregables**:
  - Programación de backups cifrados con alertas en panel (`MD-P0-02`) cuando el sistema está bloqueado.
  - Panel (`/operations`) con telemetría de backups (último backup correcto, fallos, antigüedad, ensayo de restore).
  - Ensayo de restauración verificable (`PRAGMA quick_check;`) sobre volúmenes/directorios aislados.
  - Retención limpia de imágenes (`RetentionCandidates`) preservando la versión activa y las necesarias para rollback.

### Fase 5 — UX y pruebas de sistema
- **Estado**: `validado`
- **Objetivo**: Modularizar componentes gigantes (`server.go`, `store.go`, `executor.go`) y añadir pruebas integrales.
- **Entregables**:
  - Confirmación interactiva antes de acciones destructivas (Rollback, Detener contenedor, Limpieza).
  - Detector de carreras validado al 100% (`go test -race ./...`).
  - Mensajes de error estructurados (causa, impacto, acción recomendada) y accesibilidad por teclado.

### Fase 6 — Primera release (v1.0)
- **Estado**: `validado`
- **Objetivo**: Publicar release oficial versionada, reproducible y actualizable.
- **Entregables**:
  - Tag SemVer `v1.0.0` y constante de versión en `/healthz` y `/readyz`.
  - Imágenes Docker reproducibles con metadatos OCI (`org.opencontainers.image.version="1.0.0"`).
  - Documentación de instalación limpia, actualización y restauración completa de MiniDock v1.0.0.

### Fase 7 — Experiencia «local primero, pública cuando quieras» (v1.1)
- **Estado**: `en progreso`
- **Objetivo**: Reducir el recorrido normal a «elige código → confirma detección → despliega → abre URL», sin convertir el panel en un servicio público ni ocultar fallos operativos.

#### Diagnóstico

MiniDock ya posee las piezas difíciles —builds reproducibles, releases Blue-Green, rollback, secretos, preflight, Caddy y Cloudflare Tunnel—, pero el recorrido anterior pedía al usuario decidir nombre, rama, directorio, motor, puerto, health check, comandos y dominio antes de crear una aplicación. Esas decisiones pertenecen al sistema en el caso común. Además, «crear» devolvía al inventario y obligaba a entrar de nuevo al detalle para iniciar el primer deploy.

La experiencia objetivo no copia el modelo de confianza de un SaaS. El panel continúa en loopback/LAN/VPN; solo las rutas de aplicaciones atraviesan un túnel saliente autenticado. El socket Docker continúa detrás del broker y los secretos permanecen cifrados. «Fácil» significa valores derivados, divulgación progresiva y acciones reversibles, no permisos más amplios.

#### Recorrido objetivo

1. **Preparar host una sola vez**: un chequeo visual valida Docker, Caddy, red, almacenamiento y, opcionalmente, Cloudflare.
2. **Elegir origen**: carpeta local o repositorio Git. MiniDock propone nombre y referencia sin ejecutar código.
3. **Confirmar detección**: se muestra el framework encontrado; directorio, motor, puerto y health check quedan en opciones avanzadas.
4. **Crear y desplegar**: una acción explícita y protegida por sesión/CSRF registra y encola el pipeline existente.
5. **Observar el release**: el detalle abre directamente con etapas y log en vivo; un fallo indica causa y siguiente acción.
6. **Publicar**: por defecto se usa `nombre.localhost`. Con dominio propio y Cloudflare API configurado, MiniDock administra ingress y DNS; el panel nunca cruza el túnel público.

#### Paquetes de trabajo

- **7.1 Creación en una acción — implementado y validado E2E**
  - Valores seguros derivados también en servidor: nombre desde el repositorio, rama `main` para Git remoto, directorio `.`, runtime `auto`, plantilla estática por defecto, puerto/health check de la plantilla y dominio `nombre.localhost`.
  - Opciones de infraestructura agrupadas como avanzadas y dos intenciones inequívocas: «Solo crear» y «Crear y desplegar».
  - Redirección directa al detalle, con mensajes accesibles de éxito/error.
  - Evidencia automatizada: `TestCreateAndDeployAppliesSafeDefaults`, `TestApplicationNameFromRepository` y suite `internal/app`.
- **7.2 Preflight accionable antes de crear — implementado; pendiente validación E2E**
  - El dashboard y el asistente muestran Docker, Caddy, red interna y disco antes del primer release, con reintento desde la misma pantalla.
  - Cada fallo incluye causa, impacto y una acción concreta. «Solo crear» continúa disponible y «Crear y desplegar» se deshabilita hasta resolver el host.
  - El servidor repite el preflight al recibir un deploy nuevo o manual: si el estado cambió, conserva la aplicación, no crea un trabajo condenado a fallar y redirige al detalle con la corrección necesaria.
  - Evidencia automatizada: `TestCreateAndDeployPreservesApplicationWhenHostIsNotReady`, cobertura del formulario sin JavaScript y suite `internal/app`.
  - Aceptación restante: validar en Compose/OrbStack el ciclo detener Docker o Caddy → registrar → corregir → reintentar → desplegar.
- **7.3 Publicación pública integrada — pendiente**
  - Proponer subdominios únicamente entre zonas accesibles al API Token, sin exponer el token al navegador.
  - Hacer idempotente la sincronización ingress + DNS al crear/cambiar dominio y mostrar por separado `release saludable`, `ruta aplicada`, `DNS propagado` y `TLS listo`.
  - Si Cloudflare falla, preservar el release local y ofrecer reintento; nunca revertir un contenedor saludable por un fallo DNS externo.
- **7.4 Importación y automatización — pendiente**
  - Añadir conexión guiada de GitHub App, selector de repositorio/rama y webhook con secreto generado/rotado por MiniDock.
  - Mantener confirmación de producción por defecto; habilitar deploy-on-push solo mediante decisión explícita incompatible con esa confirmación.
- **7.5 Preview y ciclo de vida — pendiente**
  - Entornos preview aislados por rama/PR con caducidad, límites y dominios no colisionables.
  - Cuotas de disco/CPU/memoria, limpieza previsualizable y eliminación recuperable de una aplicación.
- **7.6 Instalación y actualización — pendiente**
  - Comando bootstrap idempotente con diagnóstico de Docker/OrbStack, creación de redes, `.env` mínimo y URL final.
  - Actualización con backup previo, migración comprobada, health check y rollback de MiniDock.

#### Invariantes de seguridad de la fase

- El panel administrativo no se publica mediante Caddy/Cloudflare y mantiene sesión, CSRF, rate limit y desbloqueo manual.
- Ningún detector ejecuta scripts del repositorio; el código no confiable solo se ejecuta durante el build aislado.
- No se aceptan contenedores privilegiados, red host, montajes host arbitrarios, capabilities adicionales ni `no-new-privileges=false`.
- Tokens y secretos no aparecen en HTML, URL, argumentos, logs, webhooks ni metadatos de release.
- Un fallo de proveedor externo no destruye el último release saludable ni impide el acceso local.
- Automatizaciones destructivas conservan preview, confirmación, auditoría y rollback cuando aplique.

#### Métricas de aceptación v1.1

- Proyecto compatible local: máximo tres decisiones visibles y una acción para obtener el primer deployment en cola.
- Cero campos de infraestructura obligatorios en el camino común.
- Tiempo hasta feedback visible menor a dos segundos, sin esperar al build para mostrar el pipeline.
- Todas las operaciones largas son observables, cancelables o reintentables de forma segura.
- Pruebas unitarias, `-race`, Compose config y E2E real verdes; la fase no se marca validada solo por pruebas unitarias.

---

## Tabla de Control de Fases

| Fase | Descripción | Estado |
|---|---|---|
| 0 | Estabilizar la base | `validado` |
| 1 | Validar el camino real | `validado` (OrbStack 29.4.0; informe MD-P0-04) |
| 2 | Releases recuperables | `validado` |
| 3 | Seguridad operativa | `validado` |
| 4 | Backup y observabilidad | `validado` |
| 5 | UX y pruebas de sistema | `validado` |
| 6 | Primera release (v1.0) | `validado` |
| 7 | Experiencia local primero y publicación guiada (v1.1) | `en progreso` (7.1 implementado y validado E2E) |
