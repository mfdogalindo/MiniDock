# Roadmap: MiniDock

## Objetivo

Convertir esta Mac mini en un servidor local para desplegar y administrar
aplicaciones web de bajo tráfico. MiniDock centralizará despliegues CI/CD,
configuración, secretos, dominios y observabilidad básica.

## Alcance inicial

- Frontends: Angular, Next.js, Vite y aplicaciones estáticas.
- Backends: Rust, Go y Java.
- Despliegues en una sola máquina, sin requisitos de escalamiento horizontal.
- Integración con repositorios Git.
- Despliegue mediante contenedores Docker.
- HTTPS, dominios/subdominios y proxy inverso.
- Gestión segura de secretos por aplicación y entorno.

## Principios técnicos

- Priorizar simplicidad operativa y recuperación fácil.
- Aislar cada aplicación en un proyecto Docker Compose.
- Mantener configuraciones declarativas y versionables.
- No almacenar secretos en Git ni mostrarlos después de crearlos.
- Usar una base de datos local para el estado de MiniDock.
- Diseñar el sistema para automatización, pero conservar una interfaz manual
  útil.

## Fase 0: Base de infraestructura

- Instalar y validar Docker, Docker Compose y Git en la Mac mini.
- Configurar un proxy inverso: Caddy o Traefik.
- Definir estructura de almacenamiento para código, volúmenes, backups y logs.
- Configurar acceso SSH seguro y usuarios operativos.
- Definir estrategia de DNS local o público.
- Establecer backups de datos persistentes y configuración.

**Resultado:** un host preparado para servir aplicaciones Docker con HTTPS.

## Fase 1: MVP de MiniDock

- Crear una aplicación de administración local.
- Registrar una aplicación con:
  - Nombre, repositorio, rama y directorio de trabajo.
  - Tipo de aplicación.
  - Comando de build.
  - Comando de ejecución.
  - Puerto interno y dominio.
- Ejecutar un despliegue manual:
  1. Clonar o actualizar el repositorio.
  2. Construir imagen Docker.
  3. Crear o actualizar el servicio.
  4. Validar health check.
  5. Registrar resultado y logs.
- Mostrar historial de despliegues y estado actual.
- Permitir iniciar, detener, reiniciar y consultar logs.

**Resultado:** despliegues manuales confiables desde una interfaz local.

## Fase 2: Secretos y configuración

- Separar variables públicas, configuración y secretos.
- Cifrar secretos en reposo con una clave maestra fuera del repositorio.
- Inyectar secretos únicamente durante build o ejecución según corresponda.
- Gestionar secretos por aplicación y entorno: `production`, `staging`.
- Auditar creación, modificación y uso de secretos.
- Implementar rotación y eliminación segura.

**Resultado:** configuración operativa sin exponer credenciales.

## Fase 3: CI/CD

- Crear un endpoint webhook para GitHub, GitLab u otros proveedores.
- Validar firmas de webhook.
- Configurar reglas de despliegue por rama.
- Encolar despliegues para evitar ejecuciones simultáneas de la misma
  aplicación.
- Notificar éxito o error mediante correo, webhook o Discord/Slack.
- Añadir rollback hacia la última imagen exitosa.

**Resultado:** cada push autorizado puede desplegar automáticamente.

## Fase 4: Plantillas de runtime

- Plantilla para aplicaciones estáticas: build Node + Caddy/Nginx.
- Plantilla para Next.js: standalone output + Node.
- Plantilla para APIs Go: build multi-stage y binario mínimo.
- Plantilla para APIs Rust: build multi-stage y binario mínimo.
- Plantilla para Java: Gradle/Maven + JRE ligero.
- Health checks y límites de CPU/memoria por plantilla.

**Resultado:** crear aplicaciones nuevas sin escribir infraestructura
repetitiva.

## Fase 5: Operación y observabilidad

- Panel con estado, versión desplegada, uso de recursos y última actividad.
- Logs agregados y filtrables por aplicación/despliegue.
- Métricas básicas: CPU, memoria, disco, disponibilidad y duración de builds.
- Alertas por fallo de despliegue, falta de espacio, servicio caído o
  certificado próximo a vencer.
- Política de retención de imágenes, logs y artefactos.

**Resultado:** el servidor puede operarse sin inspección manual constante.

## Arquitectura inicial propuesta

- Aplicación: Go con `net/http` y plantillas `html/template` para SSR.
- Daemon y backend: un único binario Go que sirve el panel y ejecutará los jobs.
- Persistencia: SQLite.
- Ejecutor: servicio interno que controla Docker mediante la API o CLI.
- Proxy inverso: Caddy, por su gestión automática de HTTPS.
- Cola inicial: tabla de jobs en SQLite; migrar solo si aparece necesidad real.
- Empaquetado: Docker Compose para MiniDock y cada aplicación administrada.

## Riesgos a resolver temprano

- Seguridad del acceso a Docker, ya que equivale a privilegios elevados en el
  host.
- Protección de la clave maestra de secretos.
- Exposición pública de la interfaz administrativa.
- Backups verificables de bases de datos y volúmenes.
- Compatibilidad de builds con arquitectura Apple Silicon.
- Espacio en disco por imágenes, capas de build y logs.

## Próximas decisiones

1. Definir si MiniDock será solo para uso local/VPN o accesible desde Internet.
2. Elegir proveedor Git inicial: GitHub, GitLab o ambos.
3. Definir el modelo de contraseña maestra y los límites de acceso al panel.
4. Definir método de acceso: Tailscale, Cloudflare Tunnel, VPN o exposición
   directa.
5. Establecer el primer caso real de despliegue para validar el MVP.
