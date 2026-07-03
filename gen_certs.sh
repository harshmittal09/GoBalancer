#!/usr/bin/env bash
# =============================================================================
# gen_certs.sh — Self-Signed TLS Certificate Generator for GoBalancer
#
# Generates a 2048-bit RSA private key and a self-signed X.509 certificate
# valid for 365 days using OpenSSL.
#
# Usage: bash gen_certs.sh
#
# Output:
#   certs/server.key  — RSA private key (keep this secure, never commit it)
#   certs/server.crt  — Self-signed certificate (public; safe to share)
#
# For production, replace these with certificates issued by a trusted CA
# (e.g. Let's Encrypt via certbot, or your internal PKI).
# =============================================================================

set -euo pipefail

CERT_DIR="./certs"
KEY_FILE="${CERT_DIR}/server.key"
CERT_FILE="${CERT_DIR}/server.crt"
DAYS_VALID=365

echo "→ Creating certs/ directory..."
mkdir -p "$CERT_DIR"

echo "→ Generating 4096-bit RSA private key..."
openssl genrsa -out "$KEY_FILE" 4096

echo "→ Generating self-signed certificate (valid for ${DAYS_VALID} days)..."
openssl req -new -x509 \
    -key "$KEY_FILE" \
    -out "$CERT_FILE" \
    -days "$DAYS_VALID" \
    -subj "/C=IN/ST=Delhi/L=NewDelhi/O=GoBalancer/OU=Infrastructure/CN=localhost" \
    -addext "subjectAltName=IP:127.0.0.1,DNS:localhost"

echo ""
echo "✅ Certificates generated successfully:"
echo "   Private Key  : ${KEY_FILE}"
echo "   Certificate  : ${CERT_FILE}"
echo ""
echo "Certificate details:"
openssl x509 -in "$CERT_FILE" -noout -subject -dates -fingerprint -sha256
echo ""
echo "⚠️  Remember: Add certs/server.key to .gitignore — never commit private keys!"
