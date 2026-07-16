# Modelo de amenazas y controles

MiniDock administra builds y contenedores; el aislamiento de la interfaz web
no convierte al runtime en un límite de seguridad. El panel debe permanecer en
loopback o VPN mientras no exista RBAC/OIDC operativo y revisado.

| Activo o frontera | Amenaza | Control actual | Límite pendiente |
|---|---|---|---|
| Docker daemon | Panel o workload obtiene privilegios de host | MiniDock no monta el socket; solo usa el proxy TCP interno con ACL y una red workload `internal` | La ACL de familias no inspecciona `containers/create`; producción Linux necesita un authorization plugin/broker y firewall `DOCKER-USER`. |
| Red de workloads | SSRF, metadata o movimiento lateral | Bridge aislado; Caddy es el único iniciador permitido y el firewall bloquea rangos privados/metadata | Docker Desktop/OrbStack necesitan la regla equivalente en su VM. |
| GitHub webhook | Push falsificado, replay o ráfaga | HMAC SHA-256, entrega deduplicada por huella, rama/repositorio/SHA validados y límite persistente por aplicación | Rotar el secreto fuera de Git y vigilar respuestas 429/5xx. |
| Secretos de build/runtime | Fuga por código, CLI, log, informe o notificación | Cifrado AES-GCM, secretos de build con BuildKit y runtime Docker mediante `env-file` temporal `0600`; manifiestos públicos sin valores y logs bajo sesión desbloqueada | Falta prueba canario contra un daemon real y soporte equivalente si Apple Container deja de ser experimental. |
| Panel administrativo | Fuerza bruta o robo de sesión | Cookies Secure/HttpOnly, CSRF y bloqueo persistente de cinco minutos tras cinco fallos por origen directo con huella almacenada | No confía en `X-Forwarded-For`; no exponerlo a Internet hasta disponer de RBAC/OIDC revisado. |
| Backup | Copia en claro o restauración no autenticada | Formato `.mdbk` versionado/autenticado, sin fallback en claro y restauración verificada | Probar periódicamente restauración desde S3/MinIO en un host distinto. |
| Agentes remotos | Nodo suplantado o control remoto arbitrario | mTLS, WireGuard requerido, huella ligada al nodo; sin shell, deploy ni secretos | Faltan enrolamiento aprobado, rotación y revocación antes de otorgar órdenes. |
| Cloudflare Tunnels & Secretos | Fuga de tokens, modificación de un contenedor ajeno o exposición accidental del origen | AES-256-GCM en SQLite, token fuera de argv/logs/`.env`, túnel remoto con 404 final y sidecar etiquetado sin puertos, capacidades ni raíz escribible; límites de CPU/memoria/PID/log y eliminación restringida al mismo propietario/proyecto | Docker conserva el entorno del contenedor en sus metadatos; cualquier administrador del daemon ya tiene privilegios equivalentes. Limitar y rotar el API Token en Cloudflare. |

## Reglas operativas

- Nunca publiques Caddy Admin API (puerto 2019), Docker socket proxy ni el
  listener gRPC fuera de sus redes privadas.
- No añadas etiquetas `caddy.*` a releases gestionados: la única autoridad de
  rutas es MiniDock mediante la API privada de Caddy.
- Configura `MINIDOCK_GITHUB_WEBHOOK_SECRET` mediante un gestor de secretos y
  rota el valor cuando cambie un operador o se sospeche exposición.
- Antes de activar despliegue por push, ejecuta la puerta de aceptación y
  conserva el informe sin secretos.
