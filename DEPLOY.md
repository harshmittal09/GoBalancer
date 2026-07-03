# GoBalancer — Production Deployment Runbook

This document provides the **exact, ordered** sequence of commands to build,
deploy, verify, and tear down the GoBalancer cluster on a bare-metal Docker
Swarm node.

> **Prerequisites**: `provision.sh` (Phase 1) and `phase2_ssh_lockdown.sh`
> (Phase 2) must have been executed successfully on the target server before
> any step below.

---

## Step 1: SSH into the Server

From your local machine:

```bash
ssh -i ~/.ssh/id_ed25519 root@<SERVER_IP>
```

All subsequent commands are run **on the server** inside this terminal session.

---

## Step 2: Clone the Repository

```bash
cd /opt
git clone https://github.com/harshmittal09/GoBalancer.git
cd GoBalancer
```

---

## Step 3: Generate TLS Certificates

For production, use certificates from Let's Encrypt or your internal CA.
For a first deployment or staging, generate self-signed certs:

```bash
bash gen_certs.sh
```

This creates:
- `certs/server.crt` — the public certificate (baked into the Docker image)
- `certs/server.key` — the private key (**never committed to git**)

> ⚠️ **Important**: The private key is baked into the image in this setup for
> simplicity. For a PCI/SOC2 environment, store it as a Docker Secret instead:
> ```bash
> docker secret create gobalancer_key ./certs/server.key
> ```
> Then reference it in the compose file under `secrets:`.

---

## Step 4: Verify the Swarm is Active

The Swarm was initialised by `provision.sh`. Confirm it is still running:

```bash
docker info --format '{{.Swarm.LocalNodeState}}'
# Expected output: active

docker node ls
# Expected: one manager node with STATUS=Ready, AVAILABILITY=Active
```

If Swarm is not active (e.g. after a server reboot):

```bash
docker swarm init --advertise-addr <SERVER_IP>
```

---

## Step 5: Build the Docker Images

Build both the proxy and backend images locally on the Swarm manager node.

```bash
# Build the proxy image
docker build \
  -t local/gobalancer-proxy:latest \
  -f Dockerfile \
  .

# Build the backend image
docker build \
  -t local/gobalancer-backend:latest \
  -f Dockerfile.backend \
  .

# Verify both images exist
docker image ls | grep gobalancer
```

> **For a multi-node Swarm**: Push images to a private registry so all worker
> nodes can pull them:
> ```bash
> # Tag and push
> docker tag local/gobalancer-proxy:latest  registry.example.com/gobalancer-proxy:latest
> docker tag local/gobalancer-backend:latest registry.example.com/gobalancer-backend:latest
> docker push registry.example.com/gobalancer-proxy:latest
> docker push registry.example.com/gobalancer-backend:latest
>
> # Update REGISTRY in your env before deploying
> export REGISTRY=registry.example.com
> ```

---

## Step 6: Create the Overlay Network (Pre-creation)

Docker Swarm creates the overlay network automatically from the compose file,
but pre-creating it gives you explicit control over its subnet and options:

```bash
docker network create \
  --driver overlay \
  --opt com.docker.network.driver.mtu=1450 \
  --subnet 10.10.0.0/24 \
  --attachable=false \
  lb-overlay

# Verify
docker network ls | grep lb-overlay
```

---

## Step 7: Deploy the Stack

This is the primary deployment command. It reads `docker-compose.prod.yml`
and creates all services, networks, and replicas atomically:

```bash
docker stack deploy \
  --compose-file docker-compose.prod.yml \
  --with-registry-auth \
  --resolve-image always \
  gobalancer
```

| Flag | Purpose |
|---|---|
| `--compose-file` | Specifies the production compose file |
| `--with-registry-auth` | Passes the manager's registry credentials to worker nodes |
| `--resolve-image always` | Forces Swarm to re-pull images instead of using stale cached versions |
| `gobalancer` | The stack name (all services will be prefixed with `gobalancer_`) |

---

## Step 8: Monitor Deployment Progress

Watch all tasks converge to their `Running` state in real time:

```bash
# List all services in the stack
docker service ls

# Watch the backend replicas converge (run until all 3/3 are Running)
watch -n 2 'docker service ps gobalancer_backend --no-trunc'

# Watch the proxy service
watch -n 2 'docker service ps gobalancer_tcp-proxy --no-trunc'
```

Expected final output of `docker service ls`:

```
ID             NAME                    MODE         REPLICAS   IMAGE                              PORTS
xxxxxxxxxxxx   gobalancer_backend      replicated   3/3        local/gobalancer-backend:latest
xxxxxxxxxxxx   gobalancer_tcp-proxy    replicated   1/1        local/gobalancer-proxy:latest      *:8080->8080/tcp, *:8443->8443/tcp
```

---

## Step 9: Smoke Test the Live Stack

```bash
# 1. Test plain TCP proxy
echo "hello-from-client" | nc <SERVER_IP> 8080
# Expected: "hello-from-client" echoed back immediately

# 2. Test TLS proxy (skip cert verification for self-signed)
echo "hello-over-tls" | openssl s_client -connect <SERVER_IP>:8443 -quiet 2>/dev/null
# Expected: "hello-over-tls" echoed back

# 3. Check the stats dashboard
curl -s http://<SERVER_IP>:9090 | head -20

# 4. Inspect the overlay network connectivity
docker network inspect lb-overlay
```

---

## Step 10: Verify Automated Self-Healing

The backend healthcheck will trigger automatic container replacement if a
replica fails. Test this:

```bash
# Find the container ID of one backend replica
CONTAINER_ID=$(docker ps --filter "name=gobalancer_backend" -q | head -1)
echo "Killing container: $CONTAINER_ID"

# Simulate a hard crash
docker kill "$CONTAINER_ID"

# Watch Swarm detect the failure and automatically schedule a replacement
watch -n 1 'docker service ps gobalancer_backend'

# Within ~30 seconds, Swarm will show:
#  task.1  Running  (new replica)
#  task.1  Failed   (old dead container)  ← shutdown=1 exit code
```

---

## Step 11: Perform a Zero-Downtime Rolling Update

When you push a new version of the code:

```bash
# 1. Rebuild the image with a new version tag
docker build -t local/gobalancer-backend:v2 -f Dockerfile.backend .

# 2. Update the service image
docker service update \
  --image local/gobalancer-backend:v2 \
  --update-parallelism 1 \
  --update-delay 10s \
  gobalancer_backend

# 3. Monitor the rolling update
docker service ps gobalancer_backend
# One replica will be stopped and replaced at a time.
# Active connections to the surviving replicas are never interrupted.
```

---

## Operational Reference

### View live logs

```bash
# All services
docker service logs gobalancer_tcp-proxy --follow --tail 50
docker service logs gobalancer_backend   --follow --tail 50
```

### Scale the backend fleet up or down

```bash
# Scale to 5 replicas
docker service scale gobalancer_backend=5

# Scale back to 3
docker service scale gobalancer_backend=3
```

### Tear down the entire stack

```bash
docker stack rm gobalancer

# Verify all containers are removed
docker ps -a | grep gobalancer
```

### Emergency rollback

```bash
docker service rollback gobalancer_tcp-proxy
docker service rollback gobalancer_backend
```
