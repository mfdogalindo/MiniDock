# Documentación de MiniDock

La documentación distingue hechos del producto, guías operativas y trabajo
pendiente. No uses una guía o un comentario del código como prueba de que una
capacidad funciona en el host objetivo.

## Estado y trabajo pendiente

| Documento | Propósito |
|---|---|
| [ESTADO.md](ESTADO.md) | Fuente de verdad del estado observado, límites y evidencia disponible. |
| [PLAN_MEJORAS.md](PLAN_MEJORAS.md) | Roadmap vigente y handoff ejecutable para el siguiente agente. |

`PLAN_MEJORAS.md` reemplaza los planes históricos por paquetes de trabajo con
dependencias, pruebas y criterios de aceptación. El agente que implemente un
paquete debe actualizar su estado y adjuntar la evidencia en ese mismo
documento; no debe crear otra bitácora o roadmap paralelo.

## Arquitectura y capacidades

| Documento | Propósito |
|---|---|
| [ARQUITECTURA_RUNTIME.md](ARQUITECTURA_RUNTIME.md) | Contrato neutral de release y límites de los adaptadores. |
| [RUNTIMES.md](RUNTIMES.md) | Selección y restricciones de Docker y Apple Container. |
| [RUNTIMES_DETECCION.md](RUNTIMES_DETECCION.md) | Detección de proyectos, plantillas y requisitos. |
| [PROXY.md](PROXY.md) | Red, dominios, Caddy y TLS. |
| [MULTISERVIDOR.md](MULTISERVIDOR.md) | Agente gRPC saliente, mTLS y límites del vertical slice multi-servidor. |
| [SEGURIDAD.md](SEGURIDAD.md) | Modelo de amenazas, límites de confianza y controles operativos. |

## Operación

| Documento | Propósito |
|---|---|
| [OPERACION.md](OPERACION.md) | Preparación del host y comprobaciones funcionales. |
| [RUNBOOKS.md](RUNBOOKS.md) | Diagnóstico y recuperación ante incidentes conocidos. |

## Regla de mantenimiento

Una capacidad solo se marca **validada** si existe evidencia reproducible en
el host o entorno exigido por su criterio. Las pruebas unitarias permiten
marcar código como **implementado**, no como operable. Al cerrar trabajo:

1. actualiza `ESTADO.md` si cambió la realidad del producto;
2. actualiza el paquete correspondiente de `PLAN_MEJORAS.md` con comando,
   resultado y ubicación de la evidencia;
3. corrige la guía afectada en el mismo cambio;
4. elimina cualquier documento temporal cuyo conocimiento ya haya quedado
   incorporado aquí.
