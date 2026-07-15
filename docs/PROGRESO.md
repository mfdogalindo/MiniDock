# Progreso de MiniDock

Este archivo es la fuente de verdad para el avance funcional. Una fase solo se
marca como completada cuando todos sus puntos estén verificados mediante
pruebas y una comprobación manual.

## Control actual

- **Fase activa: 5 — Operación y observabilidad (implementada; pendiente de validación).**
- Siguiente entrega: aceptación operativa completa en el host.
- El panel también muestra la fase activa para que el estado no dependa solo
  de consultar esta documentación.

| Fase | Estado | Alcance actual | Siguiente entrega |
|---|---|---|---|
| 0. Base de infraestructura | Lista para validación | Servicio Go, SQLite, Docker Compose/Caddy, verificación del host, estructura de datos y backups documentados. | Ejecutar el checklist en la Mac mini y registrar la restauración de prueba. |
| 1. MVP de MiniDock | Implementada | Registro de aplicaciones, despliegue manual Docker, historial, logs y controles de contenedor. | Validar un primer repositorio real con Dockerfile. |
| 2. Secretos y configuración | Lista para validación | Configuración pública y secretos cifrados por aplicación, entorno (`production`/`staging`) y destino (`build`/`runtime`); rotación, eliminación y auditoría de creación, cambio y uso. Docker inyecta secretos de build con BuildKit y de ejecución como variables del contenedor. | Ejecutar el checklist de aceptación de Fase 2 y registrar el resultado; después, iniciar Fase 3 (CI/CD). |
| 3. CI/CD | Lista para validación | Webhook de GitHub firmado, regla por rama registrada, cola persistente por aplicación, notificación webhook y rollback a la última imagen exitosa. | Configurar GitHub y verificar un push y un rollback. |
| 4. Plantillas de runtime | Lista para validación | Plantillas sin Dockerfile en el repositorio para estáticas, Next.js, Go, Rust y Java; health checks y límites de CPU/memoria predeterminados. | Desplegar una aplicación real de cada familia y confirmar `/healthz`. |
| 5. Operación y observabilidad | Lista para validación | Panel consolidado, logs filtrados, métricas Docker, alertas de despliegue/disco/servicio/certificado y retención manual conservadora. | Ejecutar el checklist de aceptación y registrar el resultado. |
| 6. Flujo guiado y automatización | En progreso | Asistente, pipeline visible y automatizaciones configurables implementados; pendiente la validación en host y ampliar la operación contextual. | Validar ambos flujos y las reglas de automatización con un despliegue real. |

## Plan de trabajo: experiencia de lanzamiento y automatización

El objetivo de esta fase es que el flujo normal no requiera conocer los
detalles internos de Docker, Caddy ni de la cola de despliegues. Se trabajará
en incrementos pequeños y verificables:

1. **Asistente de creación de aplicación — implementado; pendiente de validación.** Divide el registro
   en tres pasos: origen Git, runtime y exposición. Cada paso valida sus datos
   antes de continuar, conserva los valores escritos y termina con una
   revisión antes de crear la aplicación.
2. **Pipeline visible por aplicación — implementado; pendiente de validación.**
   El detalle muestra la entrega más reciente como `origen → build → deploy →
   health check`, su estado persistido y un enlace al log. Un fallo permite
   abrir el log o volver a encolar el pipeline completo. La duración por etapa
   queda pendiente porque el ejecutor aún persiste un único intervalo por
   despliegue, no tiempos de etapas individuales.
3. **Automatizaciones configurables — implementado; pendiente de validación.**
   Cada aplicación puede elegir despliegue por push en su rama autorizada o
   confirmación manual de producción (son excluyentes), además de rollback
   automático ante un health check fallido. Operación permite programar la
   limpieza global diaria, semanal o mantenerla manual.
4. **Operación contextual — en progreso.** Los fallos del pipeline enlazan al
   log y ofrecen reintento; se ampliarán las acciones rápidas y mensajes a los
   demás estados operativos.
5. **Orígenes de código — implementado; pendiente de validación.** Además de
   URLs Git, MiniDock admite checkouts locales bajo un directorio permitido y
   GitHub App para repositorios privados. La referencia puede ser una rama,
   un tag o un ref que Git pueda resolver.
6. **Explorador de repositorios locales — implementado; pendiente de
   validación.** El asistente permite navegar las carpetas autorizadas y
   seleccionar un checkout Git sin escribir rutas a mano. El backend solo
   devuelve directorios bajo la raíz configurada, valida enlaces simbólicos y
   permite crear carpetas nuevas dentro de esa raíz; las carpetas ocultas se
   excluyen de la navegación. Las carpetas Git exponen ramas/tags; las
   carpetas de código sin Git se despliegan como contexto local directo.
7. **Detección de runtime — implementado; pendiente de validación.** El
   asistente lee manifiestos sin ejecutar código y propone Vite estático/SSR,
   Angular estático/SSR, Astro, Nuxt, SvelteKit, Next.js, Go, Rust, Java o
   configuración personalizada. La propuesta siempre puede cambiarse antes de
   crear la aplicación.

La aceptación del primer incremento consiste en crear una aplicación usando
teclado o ratón, avanzar y retroceder entre pasos sin perder datos, comprobar
los valores en la revisión y confirmar que el registro resultante conserva el
mismo contrato del formulario anterior.

La aceptación de automatizaciones consiste en desactivar el despliegue por
push y confirmar que GitHub responde sin encolar trabajo; después activar el
despliegue por push y confirmar que solo la rama registrada crea un trabajo.
Activa la confirmación de producción y verifica que un despliegue manual exige
escribir el nombre de la aplicación. Por último, con dos imágenes exitosas,
activa rollback automático y provoca un health check fallido: el historial
debe registrar el fallo y una acción `auto_rollback` hacia la última imagen
exitosa. Selecciona limpieza diaria o semanal en Operación y comprueba que la
política se conserva después de reiniciar MiniDock.

La Fase 1 soporta Docker y Apple Container; consulta `docs/RUNTIMES.md` para
selección, requisitos y diferencias de proxy.

La matriz de detección, recetas editables y siguientes runtimes se documenta
en `docs/RUNTIMES_DETECCION.md`.

## Cierre de Fase 4

Las plantillas se generan de forma temporal durante el despliegue, por lo que
no modifican el repositorio clonado ni reemplazan un `Dockerfile` existente.
Las aplicaciones de tipo `custom` continúan requiriendo un Dockerfile propio.
Todas las plantillas usan el puerto registrado (8080 por defecto), esperan un
endpoint `GET /healthz` y, al usar Docker, aplican los límites que corresponden
a su familia. La aceptación manual pendiente consiste en desplegar una
aplicación real de cada plantilla y comprobar el health check y los límites con
`docker inspect`.

## Primer incremento de Fase 5

Cada fila del historial de una aplicación muestra el estado persistido del
trabajo (`queued`, `running`, `successful` o `failed`) y ofrece **Ver log**.
Ese enlace sirve el archivo capturado por el worker para ese despliegue; no se
confunde con "logs actuales del contenedor", que cambian después de un
rollback o reinicio. El acceso requiere una sesión desbloqueada y queda
limitado al directorio configurado en `MINIDOCK_LOG_PATH`.

La aceptación manual pendiente consiste en encolar un despliegue que termine
con éxito y otro que falle, abrir ambos logs desde el historial y comprobar
que no es posible abrir un log de otra aplicación cambiando el identificador
en la URL.

## Segundo incremento de Fase 5

El detalle de una aplicación muestra la última imagen que terminó con éxito y
una instantánea del contenedor Docker activo: estado, health check, imagen,
inicio, CPU y memoria. La consulta usa `docker stats --no-stream`, por lo que
no deja procesos de monitorización abiertos. Si Docker no está disponible, el
contenedor no existe o se usa Apple Container, el panel lo indica y conserva el
historial; Apple Container todavía no tiene una integración equivalente de
métricas.

La aceptación manual pendiente consiste en desplegar una aplicación Docker,
comparar los valores del panel con `docker inspect` y `docker stats --no-stream
minidock-<nombre>`, y comprobar que el panel sigue respondiendo si el
contenedor se detiene.

## Cierre de Fase 5

El panel **Operación y observabilidad** consolida última actividad, versión,
estado, health check y uso de CPU/memoria de cada contenedor Docker. También
calcula el uso de disco del host, revisa certificados TLS de dominios públicos
y muestra alertas para fallo de despliegue, servicio no saludable, poco espacio
o certificados cercanos a vencer. Los logs se mantienen asociados a cada
aplicación y despliegue.

La limpieza es manual y autenticada desde el panel. Por defecto elimina logs y
registros finalizados de más de 30 días y conserva tres imágenes exitosas más
recientes por aplicación. Docker nunca recibe una eliminación forzada, de modo
que protegerá cualquier imagen todavía usada por un contenedor. Los valores se
ajustan con las variables `MINIDOCK_*` de `.env.example`. MiniDock no genera
artefactos de build separados; la política cubre los logs y las imágenes que sí
persisten. Apple Container conserva controles e historial, pero sus métricas y
limpieza de imágenes siguen sin una API integrada equivalente.

## Cierre de Fase 2

La implementación y las pruebas automatizadas están terminadas. Falta una
aceptación manual con Docker en el host: crear una configuración pública y un
secreto de build, desplegar un Dockerfile que use
`RUN --mount=type=secret,id=NOMBRE`, y confirmar que el secreto no aparece ni
en la imagen ni en los logs. El procedimiento se documenta en
`docs/OPERACION.md`.

## Cierre pendiente de aceptación

La Fase 0 requiere una validación en la Mac mini porque SSH, DNS y el destino
de backups son recursos del host y no se deben modificar automáticamente. La
Fase 1 requiere un despliegue de aceptación con un repositorio real. Los dos
procedimientos están en `docs/OPERACION.md`; hasta entonces su estado no se
marca como "Completada".
