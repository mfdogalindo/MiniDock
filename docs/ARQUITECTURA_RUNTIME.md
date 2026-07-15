# Arquitectura de runtime y releases

## Decisión

MiniDock no está acoplado a Docker ni a Compose. Un release es un contrato
persistente neutral de proveedor; Docker Engine, Apple Container y los
proveedores futuros son adaptadores que lo ejecutan.

Un adaptador de runtime recibe un artefacto inmutable, una configuración de
ejecución no secreta y el contrato de red/health. Debe poder construir o
resolver el artefacto, iniciar un candidato, inspeccionarlo, detenerlo y
eliminar sus artefactos. No debe filtrar secretos hacia el registro de release
ni hacia sus eventos.

El adaptador de proxy es independiente: publica una ruta para un candidato,
comprueba que la reconoce y la retira. Esto permite que Apple Container u otro
runtime use el mismo contrato aunque no soporte etiquetas de Caddy Docker
Proxy.

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

El adaptador Docker actual sigue siendo la implementación operativa inicial.
Apple Container se mantiene como runtime seleccionable, pero no se marcará un
release como enrutado hasta que exista su adaptador de proxy. Las etiquetas de
Caddy son un detalle del adaptador Docker, no parte del modelo de datos.
