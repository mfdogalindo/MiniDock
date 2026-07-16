# Estado actual del producto — MiniDock v1.0 Roadmap

Última actualización: 2026-07-21. Estado consolidado tras la Fase 0.

## Alcance Objetivo V1

Desplegar y operar aplicaciones Docker de forma confiable en un único servidor Mac mini.

### Incluido
- Único servidor Mac mini.
- Docker como único runtime soportado.
- Caddy como proxy inverso administrativo y de aplicaciones.
- Panel accesible únicamente mediante red local (LAN) o VPN.
- Despliegue, redeploy, cancelación y rollback atómico.
- Secretos cifrados (KMS interno con desbloqueo manual asistido).
- Backup y restauración cifrados verificables.
- Recuperación automática y reconciliación tras reinicios o fallos.

### Pospuesto (Fuera del Alcance V1)
- Apple Container runtime.
- Agentes y despliegues multi-servidor.
- Exposición pública del panel administrativo.
- Nuevos frameworks o proveedores Git adicionales.
- Rediseño visual amplio.

---

## Estado por Fase de la Hoja de Ruta

| Fase | Título | Estado | Resultado |
|---|---|---|---|
| 0 | Estabilizar la base | **Completado** | Repositorio limpio, eliminación de código muerto (`AutoUnlock`), modelo de desbloqueo asistido unificado y CI verde (`go test`, `go vet`, `docker compose config`). |
| 1 | Validar el camino real | **Completado** | Preflight de host y `scripts/e2e-compose.sh` validados en OrbStack 29.4.0: deploy, redeploy, fallo controlado, cancelación y rollback pasaron. |
| 2 | Releases recuperables | **Completado** | Conmutación Caddy sin downtime destructivo, estado persistido por release, rollback idempotente, reconciliación al inicio y fault injection cubierto. |
| 3 | Seguridad operativa | **Completado** | Panel en loopback/VPN, broker de seguridad ante socket Docker, prueba canario de no-fuga de secretos y rotación de contraseña KMS. |
| 4 | Backup y observabilidad | **Completado** | Backups cifrados con alertas por bloqueo (`MD-P0-02`), telemetría en `/operations` (último éxito, error, antigüedad, ensayo de restore) y retención segura. |
| 5 | UX y pruebas de sistema | **Completado** | Confirmaciones explícitas en UI (rollback, stop, cleanup), `go test -race ./...` 100% pasando sin condiciones de carrera, errores estructurados y accesibilidad por teclado. |
| 6 | Primera release | **Completado** | Release SemVer v1.0.0, versión expuesta en `/healthz` y `/readyz`, etiquetas OCI en `Dockerfile` y documentación de actualización lista. |

---

## Estado de Calidad y Controles

- `go test ./...`: **PASANDO**
- `go vet ./...`: **PASANDO**
- `docker compose config --quiet`: **PASANDO**

## Experiencia v1.1 en desarrollo

El primer incremento del flujo «elige código → despliega» está implementado en
la interfaz y en el servidor: MiniDock deriva los valores operativos del caso
común, permite crear y encolar el primer release en una acción y abre
directamente su pipeline. La publicación conserva `nombre.localhost` como
valor seguro por defecto y Cloudflare Tunnel como mecanismo público opcional.

La creación en una acción se considera **implementada y validada E2E** en el
host de desarrollo. La evidencia reproducible está en
`tmp/md-p0-04-e2e.json`. El preflight global accionable también está
implementado y cubierto por pruebas: una instalación incompleta puede guardar
la aplicación, pero no encola un release hasta que Docker, Caddy, la red y el
disco estén listos. Su validación E2E con servicios detenidos sigue pendiente;
el alcance restante está en la Fase 7 de [PLAN_MEJORAS.md](PLAN_MEJORAS.md).
