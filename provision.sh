#!/usr/bin/env bash
# =============================================================================
# provision.sh — Bare-Metal Server Bootstrap for GoBalancer Cluster Node
#
# Target OS  : Ubuntu 22.04 LTS / Debian 12 (Bookworm)
# Run as     : root (or via sudo)
# Usage      : sudo bash provision.sh
#
# What this script does:
#   1. Validates the runtime environment
#   2. Updates the base OS and installs core utilities
#   3. Tunes the kernel for high-scale TCP networking
#   4. Installs Docker Engine CE (official upstream repo)
#   5. Initialises a Docker Swarm manager node
# =============================================================================

set -euo pipefail          # Exit on error, unbound var, or pipe failure
IFS=$'\n\t'

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()     { echo -e "${RED}[FATAL]${NC} $*" >&2; exit 1; }

# ── 0. Pre-flight Checks ──────────────────────────────────────────────────────
info "Running pre-flight checks..."

[[ $EUID -ne 0 ]] && die "This script must be run as root. Try: sudo bash provision.sh"

# Verify OS is Debian or Ubuntu
if ! grep -qiE "ubuntu|debian" /etc/os-release 2>/dev/null; then
    die "Unsupported OS. This script targets Ubuntu or Debian only."
fi

# Detect primary network interface IP (used later for Swarm advertise-addr)
PRIMARY_IP=$(ip route get 1.1.1.1 | awk '{print $7; exit}' 2>/dev/null || hostname -I | awk '{print $1}')
[[ -z "$PRIMARY_IP" ]] && die "Could not detect the primary IP address of this host."
info "Detected primary IP: ${PRIMARY_IP}"

# ── 1. OS Update & Core Utilities ────────────────────────────────────────────
info "Updating package index and upgrading existing packages..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -y -q
apt-get upgrade -y -q

info "Installing core utilities..."
apt-get install -y -q \
    git \
    curl \
    wget \
    ufw \
    iptables \
    net-tools \
    netcat-openbsd \
    htop \
    vim \
    ca-certificates \
    gnupg \
    lsb-release \
    apt-transport-https

success "Core utilities installed."

# ── 2. Kernel Tuning for High-Scale TCP Networking ───────────────────────────
info "Configuring OS limits for high-concurrency networking..."

# --- /etc/security/limits.conf ---
# Raise the open file descriptor limit to 65535 for all users.
# This is critical: each TCP connection consumes one file descriptor.
# Without this, the OS will refuse new connections beyond the default (~1024).
LIMITS_CONF="/etc/security/limits.conf"

# Idempotent: only append if not already present
if ! grep -q "# GoBalancer: High-Concurrency Tuning" "$LIMITS_CONF"; then
    cat >> "$LIMITS_CONF" << 'EOF'

# GoBalancer: High-Concurrency Tuning
# Applied by provision.sh
*         soft    nofile    65535
*         hard    nofile    65535
root      soft    nofile    65535
root      hard    nofile    65535
EOF
    success "Updated $LIMITS_CONF (open file descriptor limit → 65535)."
else
    warn "$LIMITS_CONF already tuned. Skipping."
fi

# Also apply to the current session immediately
ulimit -n 65535 || warn "Could not set ulimit for current session (may already be set)."

# --- /etc/sysctl.conf ---
# Kernel-level socket and networking optimisations.
SYSCTL_CONF="/etc/sysctl.conf"

if ! grep -q "# GoBalancer: Network Stack Optimisations" "$SYSCTL_CONF"; then
    cat >> "$SYSCTL_CONF" << 'EOF'

# GoBalancer: Network Stack Optimisations
# Applied by provision.sh

# --- Connection Queue & Backlog ---
# Size of the accept backlog queue for listening sockets.
# Default is 128; raising this prevents dropped SYN packets during bursts.
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535

# --- Socket Buffers ---
# Increase read/write socket buffer sizes to improve throughput
# for high-bandwidth or high-latency connections.
net.core.rmem_default = 262144
net.core.rmem_max     = 134217728
net.core.wmem_default = 262144
net.core.wmem_max     = 134217728
net.ipv4.tcp_rmem     = 4096 87380 134217728
net.ipv4.tcp_wmem     = 4096 65536 134217728

# --- Ephemeral Port Range ---
# Expand the local port range for outbound connections.
# Default is ~28,000 ports; this raises it to ~60,000, preventing
# port exhaustion under high connection-per-second workloads.
net.ipv4.ip_local_port_range = 1024 65535

# --- TIME_WAIT & Connection Recycling ---
# Allow rapid reuse of sockets in TIME_WAIT state.
# Critical for load balancers that cycle connections extremely fast.
net.ipv4.tcp_tw_reuse = 1

# Maximum number of sockets in TIME_WAIT state before the kernel
# starts aggressively recycling them.
net.ipv4.tcp_max_tw_buckets = 2000000

# --- Keepalive ---
# Detect and close dead connections faster.
net.ipv4.tcp_keepalive_time   = 120
net.ipv4.tcp_keepalive_intvl  = 30
net.ipv4.tcp_keepalive_probes = 5

# --- SYN Flood Protection ---
net.ipv4.tcp_syncookies = 1

# --- File Descriptor Limit (Kernel Level) ---
# Absolute maximum number of open file descriptors across all processes.
fs.file-max = 2097152

# --- Docker / Container Networking ---
# Required for Docker to forward packets between containers and the host.
net.ipv4.ip_forward = 1

EOF
    success "Updated $SYSCTL_CONF with network stack optimisations."
else
    warn "$SYSCTL_CONF already tuned. Skipping."
fi

# Apply sysctl changes immediately without a reboot
sysctl -p "$SYSCTL_CONF" > /dev/null 2>&1
success "Kernel parameters applied immediately via sysctl -p."

# ── 3. Firewall (UFW) Configuration ──────────────────────────────────────────
info "Configuring UFW firewall rules..."

ufw --force reset          # Start from a known clean state

# Allow SSH first to avoid locking ourselves out
ufw allow 22/tcp comment 'SSH Access'

# Ports for the GoBalancer cluster
ufw allow 80/tcp   comment 'HTTP'
ufw allow 443/tcp  comment 'HTTPS'
ufw allow 8080/tcp comment 'GoBalancer Proxy (plain)'
ufw allow 8443/tcp comment 'GoBalancer Proxy (TLS)'
ufw allow 9090/tcp comment 'GoBalancer Stats Dashboard'

# Docker Swarm inter-node communication ports
ufw allow 2376/tcp  comment 'Docker daemon TLS'
ufw allow 2377/tcp  comment 'Docker Swarm cluster management'
ufw allow 7946/tcp  comment 'Docker Swarm node discovery (TCP)'
ufw allow 7946/udp  comment 'Docker Swarm node discovery (UDP)'
ufw allow 4789/udp  comment 'Docker Swarm overlay network (VXLAN)'

# Default policy: deny all inbound not explicitly allowed
ufw default deny incoming
ufw default allow outgoing

ufw --force enable
success "UFW firewall enabled and configured."

# ── 4. Docker Engine CE Installation ─────────────────────────────────────────
info "Installing Docker Engine CE from official upstream repository..."

# Remove any old/unofficial Docker packages to avoid conflicts
apt-get remove -y -q \
    docker docker-engine docker.io containerd runc 2>/dev/null || true

# Add Docker's official GPG key
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg

# Add Docker's official apt repository
echo \
    "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
    https://download.docker.com/linux/ubuntu \
    $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    | tee /etc/apt/sources.list.d/docker.list > /dev/null

apt-get update -y -q
apt-get install -y -q \
    docker-ce \
    docker-ce-cli \
    containerd.io \
    docker-buildx-plugin \
    docker-compose-plugin

# Enable and start Docker service
systemctl enable docker
systemctl start docker

success "Docker Engine CE installed successfully."

# Verify Docker is running
DOCKER_VERSION=$(docker --version 2>/dev/null || echo "UNKNOWN")
info "Docker version: ${DOCKER_VERSION}"

# ── 5. Docker Daemon Tuning ───────────────────────────────────────────────────
info "Tuning Docker daemon configuration for production workloads..."

DOCKER_DAEMON_CONF="/etc/docker/daemon.json"
mkdir -p /etc/docker

cat > "$DOCKER_DAEMON_CONF" << 'EOF'
{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "100m",
    "max-file": "5"
  },
  "default-ulimits": {
    "nofile": {
      "Name":  "nofile",
      "Hard":  65535,
      "Soft":  65535
    }
  },
  "storage-driver": "overlay2",
  "live-restore": true,
  "userland-proxy": false,
  "ipv6": false,
  "metrics-addr": "127.0.0.1:9323",
  "experimental": true
}
EOF

systemctl restart docker
success "Docker daemon reconfigured and restarted."

# ── 6. Docker Swarm Initialisation ───────────────────────────────────────────
info "Initialising Docker Swarm manager node..."

# Check if Swarm is already active to make the script idempotent
SWARM_STATE=$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo "inactive")

if [[ "$SWARM_STATE" == "active" ]]; then
    warn "Docker Swarm is already initialised on this node. Skipping swarm init."
else
    docker swarm init --advertise-addr "$PRIMARY_IP"
    success "Docker Swarm initialised. This node is now the Swarm Manager."
fi

# Retrieve and display the join tokens for worker nodes
info "--- Swarm Node Join Tokens (KEEP THESE SECURE) ---"
echo ""
echo -e "${YELLOW}  Worker  Join Token:${NC}"
docker swarm join-token worker  -q
echo ""
echo -e "${YELLOW}  Manager Join Token:${NC}"
docker swarm join-token manager -q
echo ""

# ── 7. Final System Summary ───────────────────────────────────────────────────
echo ""
echo -e "${GREEN}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  ✅  GoBalancer — Bare-Metal Provisioning Complete!${NC}"
echo -e "${GREEN}══════════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "  ${CYAN}Primary IP          :${NC} ${PRIMARY_IP}"
echo -e "  ${CYAN}OS File Descriptors :${NC} $(ulimit -n)"
echo -e "  ${CYAN}Docker Version      :${NC} ${DOCKER_VERSION}"
echo -e "  ${CYAN}Swarm Status        :${NC} $(docker info --format '{{.Swarm.LocalNodeState}}')"
echo -e "  ${CYAN}UFW Status          :${NC} $(ufw status | head -1)"
echo ""
echo -e "  ${YELLOW}Next Step: Phase 2 — Administrative Lockdown & SSH Hardening${NC}"
echo -e "  ${YELLOW}Run: sudo bash phase2_ssh_lockdown.sh${NC}"
echo ""
echo -e "${GREEN}══════════════════════════════════════════════════════════════${NC}"
