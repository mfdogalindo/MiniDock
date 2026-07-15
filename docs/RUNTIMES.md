# Runtimes de contenedores

MiniDock detecta los dos runtimes al intentar un despliegue:

- `docker`: requiere que `docker info` responda correctamente.
- `apple`: requiere que el binario `container` y su servicio (`container system
  start`) estén disponibles.

Configura la preferencia global con `MINIDOCK_RUNTIME`:

```sh
# elige Apple Container si está disponible; de lo contrario Docker
MINIDOCK_RUNTIME=auto

# exige una implementación concreta y falla si no está lista
MINIDOCK_RUNTIME=apple
MINIDOCK_RUNTIME=docker
```

Apple Container requiere Apple silicon y macOS 26. Sus imágenes son OCI y el
CLI admite construir desde Dockerfile/Containerfile, ejecutar, iniciar,
detener y consultar logs. MiniDock publica por `127.0.0.1:<puerto interno>` en
ese runtime. Ejecuta MiniDock directamente en macOS para usarlo; una instancia
de MiniDock dentro de Docker no puede controlar el servicio macOS de Apple.

Docker sigue siendo la opción que integra el proxy dinámico Caddy mediante el
socket Docker. Para Apple Container configura Caddy para dirigir cada dominio
al puerto local publicado hasta que se incorpore una integración de proxy
específica para su red.

Los secretos de build de MiniDock usan `docker build --secret` (BuildKit). Si
una aplicación los configura, debe desplegarse con `docker`; Apple Container
mantiene soporte para secretos de ejecución, pero rechaza secretos de build.
