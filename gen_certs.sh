#!/bin/bash
mkdir -p certs
cd certs

# Generate CA Private Key
openssl genrsa -out ca.key 2048


#Genrate CA Public Root Certificate
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 -out ca.crt -subj "/CN=GoBalancer Local CA"


#Generate Server's Private Key
openssl genrsa -out server.key 2048


#Generate Server's CSR
openssl req -new -key server.key -out server.csr -subj "/CN=localhost"


# Extension file to define Subject Alternative Name
cat > server.ext << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
subjectAltName = @alt_names
[alt_names]
DNS.1 = localhost
IP.1 = 127.0.0.1
EOF


#Sign the Server Cert
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days 365 -sha256 -extfile server.ext

