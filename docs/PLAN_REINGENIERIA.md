# Plan de reingeniería hacia un MiniDock operable

Fecha de diagnóstico: 2026-07-15.

## Dictamen

MiniDock ya demuestra el flujo básico con `DemoTest`: obtiene el código,
construye una imagen, arranca un contenedor Docker, espera su health check y
guarda el log y la imagen resultante. Aún no debe presentarse como una
plataforma de despliegue plenamente operable. El riesgo no está en añadir más
plantillas: está en consolidar los contratos de release, recuperación, proxy,
seguridad y experiencia de operación.

La meta es que un operador pueda responder de forma fiable a estas preguntas:

1. ¿Qué commit exacto está publicado, con qué configuración y desde cuándo?
2. ¿Qué ocurre si MiniDock, Docker o el host se reinician a mitad de un job?
3. ¿Un release fallido preserva inequívocamente el servicio anterior?
4. ¿El dominio enruta al release saludable y cómo se demuestra?
5. ¿Un despliegue automatizado con secretos puede funcionar de forma segura
   sin una persona conectada?

## Hallazgos observables

| Área | Estado actual | Brecha a cerrar |
|---|---|---|
| Unidad desplegable | El ejecutor usa `docker build` y `docker run` directamente. | El roadmap habla de Compose por aplicación; falta un manifiesto declarativo, versionado y reproducible por release. |
| Identidad del artefacto | La imagen usa una etiqueta basada en hora. | No se registra SHA de commit, árbol de fuentes, digest OCI, Dockerfile efectivo ni configuración no secreta usada. |
| Sustitución | Se elimina el contenedor actual antes de iniciar el nuevo. | Un error al crear la sustitución puede dejar indisponible la aplicación; el rollback es un trabajo posterior, no una conmutación atómica. |
| Cola | SQLite conserva `queued/running`; un bucle único procesa trabajos. | No hay lease, heartbeat, cancelación, reanudación/reconciliación tras caída, prioridad ni paralelismo controlado entre aplicaciones. |
| Secretos y CI/CD | La clave maestra vive solo en memoria; con secretos y panel bloqueado el job falla. | El despliegue automático no es realmente desatendido. Hace falta una política explícita de custodia de claves. |
| Proxy y dominios | Caddy Docker Proxy se configura mediante etiquetas. | Falta comprobación de que Caddy recibió la ruta, validación estricta de dominio y estado de TLS/ruta sin bloquear la página. |
| Contrato de runtime | Las plantillas y detectores cubren varios frameworks. | Los endpoints de health y comandos de producción no son uniformes; el roadmap afirma `/healthz`, pero varias plantillas verifican `/`. |
| Persistencia/recuperación | SQLite, logs e imágenes se retienen. | Faltan backups atómicos probados, inventario de artefactos, migraciones versionadas y recuperación de estado del runtime al arrancar. |
| Seguridad de superficie | Sesión HttpOnly/SameSite, CSP y webhook firmado. | No hay CSRF explícito, cookie `Secure` condicionada a HTTPS, rate limiting, roles/auditoría de acciones críticas ni política clara para el socket Docker. |
| UI/UX | Asistente, detalle, pipeline y logs en vivo ya existen. | El dashboard no resume salud/acción; los estados son optimistas y faltan progreso real por etapa, prevención de errores y recuperación guiada. |

## Principios de diseño que deben fijarse antes de implementar

1. **Release inmutable.** Un release es `aplicación + commit SHA + digest de
   imagen + manifiesto + configuración versionada + timestamps`. La etiqueta
   humana no es su identidad.
2. **Estado deseado y estado observado separados.** SQLite expresa el release
   deseado; un reconciliador consulta Docker/Caddy y marca las diferencias.
3. **No interrumpir por defecto.** El release previo sigue atendiendo hasta que
   el candidato esté sano y el proxy haya conmutado. Si no es posible, la UI lo
   declara como modo con downtime.
4. **Jobs finitos, recuperables e idempotentes.** Cada etapa deja checkpoint,
   timeout y causa estructurada; reiniciar MiniDock no deja trabajos eternos.
5. **Un contrato explícito por runtime.** Puerto, comando, health endpoint,
   archivos de salida y variables requeridas se validan antes del build.
6. **Seguridad por capacidad mínima.** El socket Docker se trata como acceso
   administrativo; los secretos nunca se muestran ni se incluyen en logs,
   etiquetas, eventos ni manifiestos persistidos.
7. **El operador primero.** Toda acción destructiva muestra alcance, impacto y
   recuperación; todo error apunta a su causa y siguiente acción.

## Plan priorizado

### Hito 0 — Contrato operativo y aceptación reproducible (bloqueador)

Objetivo: convertir la demo en un caso de aceptación automatizable y decidir
el modelo de unidad desplegable.

- **Decisión (2026-07-15):** el contrato de release será neutral de proveedor.
  MiniDock define el estado deseado, el artefacto y la ruta; adaptadores de
  runtime materializan ese contrato en Docker, Apple Container u otra
  tecnología futura. Compose puede ser una implementación auxiliar de un
  adaptador Docker, nunca la unidad desplegable ni el formato persistido de un
  release. El proxy se expone mediante un adaptador independiente del runtime.
  No mantener ambos modelos implícitos.
- Definir un `Release` persistente con commit SHA, ref solicitada, digest OCI,
  imagen, runtime, puerto, health endpoint, inicio/fin y causa normalizada.
- Definir los códigos de error de las etapas `source`, `build`, `start`,
  `route`, `health` y `rollback`; no deducir la etapa a partir de texto de log.
- Crear una matriz de aceptación en CI: build de MiniDock, Compose válido,
  deploy de `DemoTest`, health interno, ruta Caddy, redeploy, rollback y fallo
  deliberado.

Criterio de salida: una ejecución produce un informe de release reproducible
sin inspección manual de SQLite o Docker.

### Hito 1 — Motor de releases seguro y recuperable (P0)

Objetivo: evitar indisponibilidad y estado ambiguo durante un despliegue.

- Sustituir el job genérico por una máquina de estados persistida, con intento,
  lease, heartbeat, deadline, cancelación y reanudación después de reiniciar.
- Añadir límites: timeout de clon/fetch/build/start/health, tamaño máximo de
  log y cuota de workspace; exponer cancelación segura en la UI.
- Construir el candidato con nombre temporal; validar health y conectar al
  proxy antes de retirar el release anterior. Conservar el anterior hasta que
  el nuevo sea `ready`.
- Implementar reconciliación de arranque: detectar jobs `running` abandonados,
  contenedores huérfanos, release activo real y divergencias de red/proxy.
- Permitir concurrencia limitada entre aplicaciones, manteniendo exclusión por
  aplicación y recursos globales configurables.
- Persistir y verificar digest de imagen; bloquear rollback a imágenes ausentes
  y mostrar una acción clara para reconstruirlas.

Criterio de salida: matar MiniDock durante cualquier etapa no deja dos
contenedores atendiendo el mismo dominio ni deja la aplicación sin un estado
explicable o recuperable.

### Hito 2 — Proxy, red y dominio verificables (P0)

Objetivo: que “successful” signifique accesible por el dominio configurado.

- Validar nombres de dominio, puerto y host contra una política explícita;
  rechazar valores que puedan alterar etiquetas/configuración de Caddy.
- Introducir un adaptador de proxy: aplicar ruta, comprobar que Caddy la
  reconoce y ejecutar una sonda HTTP con `Host`/TLS correctos antes de marcar
  el release exitoso.
- Separar el dominio administrativo de dominios de aplicaciones y documentar
  red, DNS local, TLS público y certificados de desarrollo.
- Ejecutar comprobaciones de certificados y métricas en un recolector de fondo
  con caché; las páginas nunca deben abrir conexiones TLS externas por fila.
- Añadir prueba de integración Compose+Caddy que verifique panel y aplicación
  desde la red pública simulada.

Criterio de salida: el estado de la UI distingue “contenedor sano” de
“dominio enrutado y disponible”.

### Hito 3 — Fuentes, artefactos y runtimes con contrato (P0)

Objetivo: builds repetibles y diagnósticos previos al consumo de recursos.

- Resolver una rama/tag a SHA antes del build y mostrarlo; para fuentes locales
  almacenar una huella del árbol o declarar que no hay reproducibilidad Git.
- Añadir `.dockerignore` gestionado o advertencia de contexto excesivo; medir
  tamaño de contexto y espacio antes de construir.
- Convertir cada plantilla en un contrato testeado: comando de producción,
  output esperado, usuario, puerto, health endpoint y límites. Unificar
  `/healthz` o permitir configurarlo por aplicación con validación previa.
- Separar detección (sugerencia) de validación (requisitos reales); la UI debe
  listar exactamente qué falta para el runtime seleccionado.
- Soportar Dockerfile propio con build args/secret mounts declarados y un
  linter de contrato, sin ejecutar comandos arbitrarios del formulario.

Criterio de salida: una aplicación incompatible falla antes del build con una
causa accionable, y cada release puede reconstruirse desde sus metadatos.

### Hito 4 — Seguridad y continuidad de secretos (P0)

Objetivo: hacer explícito y seguro el modelo de operación humana y CI/CD.

- Decidir el modo de claves:
  - **Operación asistida:** jobs con secretos permanecen bloqueados hasta que
    un operador desbloquee MiniDock.
  - **Operación desatendida:** clave de datos envuelta por un proveedor local
    del host (Keychain/secret manager), con rotación, auditoría y controles de
    arranque.
- Mostrar esa condición antes de permitir activar deploy por push; nunca
  aceptar un webhook que inevitablemente fallará por clave bloqueada.
- Añadir tokens CSRF a formularios mutables, cookies `Secure` detrás de HTTPS,
  rotación/invalidez de sesiones, límites de intentos y registro de acciones.
- Revisar el modelo de amenaza del socket Docker y el montaje de repositorios;
  usar directorio dedicado de solo lectura si no es necesario crear carpetas.
- Implementar backup SQLite consistente (`VACUUM INTO` o backup API), prueba
  de restauración programada y cifrado/retención del destino de backup.

Criterio de salida: se puede explicar qué persona o componente puede desplegar
con secretos, en qué condiciones y cómo se recupera el sistema.

### Hito 5 — Observabilidad y operación cotidiana (P1)

Objetivo: diagnosticar sin abrir una terminal.

- Registrar eventos estructurados por release y etapa; conservar logs de build
  y runtime con límites, búsqueda y descarga controlada.
- Añadir duración por etapa, cola/espera, uso de disco de imágenes/workspaces,
  salud de proxy y última sonda exitosa.
- Reemplazar alertas repetidas por reglas deduplicadas con severidad, inicio,
  resolución y enlace al release afectado.
- Añadir vista de recuperación: reintentar desde etapa segura, rollback,
  cancelar, inspeccionar diferencias y limpiar artefactos con vista previa.
- Definir SLO local mínimo: disponibilidad de ruta, límite de duración de
  build y presupuesto de disco; mostrar cuándo se incumple.

Criterio de salida: un operador identifica causa, impacto y acción siguiente
en menos de dos interacciones para los fallos comunes.

### Hito 6 — UI/UX de calidad de producto (P1)

Objetivo: una interfaz calmada, clara y accesible que reduzca errores de
operación.

#### Lineamientos obligatorios

- **Jerarquía de información:** dashboard = salud y acciones inmediatas;
  detalle = release actual, historial y recuperación; operación = estado del
  host. No duplicar datos sin aportar una decisión.
- **Estados explícitos:** usar siempre `pendiente`, `en ejecución`, `listo`,
  `degradado`, `fallido`, `cancelado` y `desconocido`; color nunca es el único
  indicador.
- **Progreso honesto:** mostrar etapa activa, duración, salida reciente y
  siguiente transición conocida. No marcar “build procesado” si la etapa real
  es desconocida.
- **Acciones seguras:** desplegar, rollback, detener y limpiar deben indicar
  release objetivo, impacto y recuperación. Las acciones irreversibles exigen
  confirmación contextual, no solo un botón rojo.
- **Prevención antes que error:** el asistente valida acceso a fuente, rama,
  runtime, dominio, puerto, secreto requerido y health endpoint antes de crear
  o desplegar.
- **Lenguaje operativo:** mensajes breves con causa, impacto y acción: “Caddy
  no reconoce `api.example.com`; revisar DNS o la conexión a la red `minidock`”.
- **Accesibilidad:** HTML semántico, foco visible, orden de teclado, contraste
  AA, etiquetas/errores asociados al campo, regiones ARIA para cambios en
  vivo y logs que no roben el foco.
- **Respuesta y resiliencia:** SSR útil sin JavaScript; mejoras JS progresivas;
  skeletons o estados de carga para llamadas lentas; preservar entradas y URL
  tras errores o recargas.
- **Móvil y densidad:** tarjetas resumidas en móvil, tabla expandible en
  escritorio; datos críticos visibles sin desplazamiento horizontal.
- **Consistencia:** un único vocabulario para releases, aplicaciones, jobs y
  estados; fechas relativas con fecha absoluta accesible; zona horaria visible.

#### Entregables UX

1. Dashboard de salud: aplicaciones ordenadas por gravedad, release activo,
   última sonda y una acción recomendada.
2. Detalle de release: línea temporal real por etapa, metadatos de
   reproducibilidad, log navegable y acciones seguras.
3. Asistente de alta: validación progresiva, detección explicable y pantalla
   final de “preflight” antes del primer deploy.
4. Centro de recuperación: trabajos activos, cancelación, reintento y rollback
   con lenguaje de impacto.
5. Pruebas de accesibilidad automatizadas y cinco sesiones de prueba de tareas
   con operadores objetivo antes de dar por cerrado el hito.

### Hito 7 — Cobertura, entrega y mantenimiento (P1)

- Pruebas unitarias para cada transición y migración; integración con Docker
  real; e2e con Compose/Caddy; pruebas de regresión de UI y accesibilidad.
- Pipeline CI que construya la imagen, ejecute pruebas y publique artefactos
  con SBOM, escaneo de dependencias y digest firmado.
- Versionado de esquema y compatibilidad hacia atrás; migraciones probadas en
  copia de una base real.
- Runbooks concisos para incidente de Docker/Caddy, disco lleno, certificado,
  secreto bloqueado, job abandonado y restauración completa.

## Orden recomendado de ejecución

`Hito 0 → Hito 1 → Hito 2 → Hito 3 → Hito 4 → Hito 5 + Hito 6 → Hito 7`.

Los Hitos 1, 2 y 4 son bloqueadores de una operación real. Las nuevas
plantillas, proveedores Git adicionales o métricas más ricas no deben adelantar
esa secuencia.

## Decisiones que requiere el propietario

1. ¿Se mantiene Docker Engine directo o se adopta Compose declarativo por
   release? Esta decisión determina el modelo de reconciliación.
2. ¿MiniDock debe poder desplegar secretos sin estar desbloqueado? Si la
   respuesta es sí, ¿qué almacén de claves del host se autoriza?
3. ¿El panel estará solo en VPN/red local o se expondrá públicamente detrás de
   un proveedor de identidad? Define el modelo de autenticación y cookies.
4. ¿Qué disponibilidad se espera por aplicación: puede haber downtime breve o
   es obligatorio un despliegue con conmutación sin interrupción?
