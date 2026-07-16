# Agentes multi-servidor (vertical slice)

Esta capacidad está en una fase inicial y no habilita todavía despliegues
remotos. Su único propósito es comprobar una identidad de agente, una conexión
saliente persistente y el inventario de nodos antes de conceder cualquier
autoridad sobre runtimes o secretos.

## Modelo de red

Instala WireGuard entre el host del control plane y cada nodo. Publica
`MINIDOCK_AGENT_ADDRESS` solamente en la IP WireGuard; nunca en Internet ni en
la interfaz de administración. El agente marca la conexión saliente hacia esa
dirección. WireGuard restringe el transporte y mTLS identifica al proceso:
ambas capas son obligatorias.

El control plane requiere TLS 1.3, un certificado de servidor y una CA de
clientes. Cada agente necesita un certificado cliente distinto, más la CA que
firma el servidor. La huella SHA-256 del primer certificado visto queda ligada
al `node-id`; usar otra clave con ese ID se rechaza. La renovación/revocación
administrada se implementará antes de autorizar despliegues remotos.

## Activación

En el control plane, define las tres variables TLS y una dirección WireGuard:

```sh
MINIDOCK_AGENT_ADDRESS=10.88.0.1:9443
MINIDOCK_AGENT_TLS_CERTIFICATE_PATH=/etc/minidock/control.crt
MINIDOCK_AGENT_TLS_PRIVATE_KEY_PATH=/etc/minidock/control.key
MINIDOCK_AGENT_TLS_CLIENT_CA_PATH=/etc/minidock/agent-ca.crt
```

En cada nodo, ejecuta el binario `minidock-agent` con archivos de certificado
de permisos restrictivos:

```sh
minidock-agent \
  --control 10.88.0.1:9443 --server-name control.minidock.internal \
  --node-id edge-01 --node-name edge-01 \
  --tls-certificate /etc/minidock/agent.crt \
  --tls-private-key /etc/minidock/agent.key \
  --tls-ca /etc/minidock/control-ca.crt
```

El panel y `GET /api/nodes` muestran nombre, versión, capacidades y última
señal. Esta API requiere una sesión desbloqueada. No incluye la huella del
certificado.

## Límites de seguridad actuales

- El protocolo no contiene shell, ejecución arbitraria, deploy, rollback ni
  transferencia de secretos.
- El agente no abre un listener y MiniDock no realiza SSH ni conexiones
  entrantes hacia nodos.
- Una CA capaz de emitir certificados cliente controla el enrolamiento actual;
  por tanto debe guardarse fuera de los nodos. El siguiente paquete incorpora
  tokens de enrolamiento de un solo uso, aprobación y rotación/revocación.
