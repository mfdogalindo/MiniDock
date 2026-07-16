# Arquitectura de runtime y releases

## Decisión de diseño

MiniDock persiste un contrato de release neutral de proveedor. Esto reduce el
acoplamiento del modelo de datos, pero la implementación operativa actual sí
depende de Docker y Caddy: no debe confundirse la abstracción del código con
paridad funcional entre runtimes.

Un adaptador de runtime recibe un artefacto inmutable, una configuración de
ejecución no secreta y el contrato de red/health. Debe poder construir o
resolver el artefacto, iniciar un candidato, inspeccionarlo, detenerlo y
eliminar sus artefactos. No debe filtrar secretos hacia el registro de release
ni hacia sus eventos.

La interfaz de proxy es independiente en el código. En la práctica,
`CaddyProxyAdapter.Apply` reemplaza una ruta identificada de MiniDock mediante
la Admin API JSON privada de Caddy. Docker no usa etiquetas `caddy.*`: así un
evento de contenedor no puede competir con la conmutación. Apple Container no
cuenta aún con un adaptador de proxy equivalente.

En código, `RuntimeAdapter` delimita las operaciones de build, arranque,
control, logs, estado y retención. `DockerAdapter` y `AppleContainerAdapter`
son implementaciones registrables; un runtime nuevo se incorpora como otro
adaptador, sin ampliar el modelo persistido de release.

## Contrato persistido de release

Cada release registra, cuando esté disponible:

- referencia solicitada y revisión exacta de fuente;
- huella de fuente cuando no exista revisión Git;
- referencia y digest del artefacto;
- runtime que materializó el release;
- puerto interno, endpoint de health y manifiesto neutral versionado;
- inicio, fin y, ante error, etapa, código y detalle normalizados.

El manifiesto no contiene secretos. La configuración no secreta queda
identificada por una huella, no por valores que puedan acabar en logs o
metadatos de infraestructura.

## Adaptadores actuales y transición

Docker es la única implementación candidata a soporte operativo. Su
conmutación Blue-Green valida un candidato, reemplaza atómicamente la ruta de
Caddy y conserva el color anterior hasta que la sonda externa confirma el
nuevo. La recuperación tras una caída se controla en `MD-P0-03` de
[PLAN_MEJORAS.md](PLAN_MEJORAS.md).

Apple Container es experimental. Puede construir e iniciar un contenedor, pero
carece de health y ruta equivalentes, digest persistido, métricas y retención.
No debe interpretarse un job exitoso en ese runtime como disponibilidad por
dominio. Las rutas Caddy siguen siendo un detalle del adaptador Docker, no
parte del modelo persistido.
