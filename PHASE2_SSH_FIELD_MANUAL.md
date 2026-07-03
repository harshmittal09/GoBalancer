# Phase 2 Field Manual: SSH Key Transfer & sshd_config Editing

> This document is the step-by-step companion to `phase2_ssh_lockdown.sh`.
> It covers the **manual** operations you must perform from your **local machine** first.

---

## Step 1: Generate a Cryptographic Key Pair (Run on YOUR local machine)

Ed25519 is the recommended algorithm — it is faster and more secure than RSA.

```bash
# Generate a new Ed25519 key pair.
# -C adds a comment label so you know which key is which in authorized_keys.
ssh-keygen -t ed25519 -C "gobalancer-prod-key-$(date +%Y%m%d)" -f ~/.ssh/id_ed25519
```

You will be prompted for a **passphrase**. Always set one. This encrypts the
private key on disk so it is useless if your laptop is stolen.

This creates two files:
| File | Purpose |
|---|---|
| `~/.ssh/id_ed25519` | **PRIVATE key** — Never share, never copy to the server |
| `~/.ssh/id_ed25519.pub` | **PUBLIC key** — This is what goes on the server |

---

## Step 2: Transfer the Public Key to the Server

### Method A — Automated (Recommended)
`ssh-copy-id` is the safest, most reliable method. It automatically appends the
key without breaking any existing entries in `authorized_keys`.

```bash
# Replace <SERVER_IP> with the actual IP address of your bare-metal server.
ssh-copy-id -i ~/.ssh/id_ed25519.pub root@<SERVER_IP>
```
You will be prompted for the root password **this one last time**.

### Method B — Manual SSH Tunnel + Heredoc (If ssh-copy-id is unavailable)

This is the fully manual approach using a single SSH tunnel command.
It reads your public key locally and pipes it directly into the server's
`authorized_keys` file in one atomic operation.

```bash
# 1. Create the .ssh directory on the server with strict permissions,
#    then append your public key to authorized_keys.
cat ~/.ssh/id_ed25519.pub | ssh root@<SERVER_IP> \
    "mkdir -p ~/.ssh && \
     chmod 700 ~/.ssh && \
     cat >> ~/.ssh/authorized_keys && \
     chmod 600 ~/.ssh/authorized_keys"
```

### Method C — Verify the Transfer Worked
Before running the lockdown script, confirm key-based login works:

```bash
# -i specifies the private key to use.
# If this succeeds WITHOUT asking for a password, you are ready.
ssh -i ~/.ssh/id_ed25519 root@<SERVER_IP> "echo 'Key-based login confirmed!'"
```

---

## Step 3: Run the Lockdown Script on the Server

```bash
# Upload the script to the server
scp -i ~/.ssh/id_ed25519 phase2_ssh_lockdown.sh root@<SERVER_IP>:~/

# SSH into the server
ssh -i ~/.ssh/id_ed25519 root@<SERVER_IP>

# Run the lockdown (from inside the server session)
sudo bash ~/phase2_ssh_lockdown.sh
```

---

## Step 4: How to Edit sshd_config Manually Using Vim

> **Note:** The script above rewrites `sshd_config` automatically.
> This section is for manual edits, debugging, or future modifications.

### Open the file in Vim

```bash
sudo vim /etc/ssh/sshd_config
```

### Essential Vim Commands

| Action | Keystrokes |
|---|---|
| Enter **INSERT** mode to start typing | Press `i` |
| **Exit INSERT** mode and return to NORMAL mode | Press `Esc` |
| **Search** for a specific parameter | In NORMAL mode: `/PasswordAuthentication` then `Enter` |
| **Jump to next** search match | Press `n` |
| **Delete the current line** | In NORMAL mode: `dd` |
| **Undo** last change | In NORMAL mode: `u` |
| **Save and quit** | In NORMAL mode: `:wq` then `Enter` |
| **Quit WITHOUT saving** (emergency exit) | In NORMAL mode: `:q!` then `Enter` |
| **Go to a specific line number** (e.g. line 45) | In NORMAL mode: `:45` then `Enter` |

### Exact Parameters to Modify

Search for each parameter with `/ParameterName` and change the value.
If the line starts with `#`, remove the `#` first (that uncomments it).

```
# ❌ Disable ALL password-based login
PasswordAuthentication no

# ❌ Disable challenge-response (a secondary password mechanism)
ChallengeResponseAuthentication no
KbdInteractiveAuthentication no

# ❌ Disable PAM to prevent password auth fallback
UsePAM no

# ✅ Enable public key auth (the only allowed method)
PubkeyAuthentication yes

# ❌ Block direct root password login (allow key-based root only)
PermitRootLogin prohibit-password

# ❌ Prevent empty password logins
PermitEmptyPasswords no

# ⏱ Tight login window (30 seconds to authenticate)
LoginGraceTime 30

# 🔁 Limit retries before disconnect
MaxAuthTries 3
```

### After editing, ALWAYS validate before reloading

```bash
# This parses the config and reports any syntax errors.
# It will NOT restart the daemon — it is purely a dry run.
sudo sshd -t

# Only reload if the above command exits cleanly with no output.
sudo systemctl reload sshd
```

---

## Step 5: Verify the Lockdown Worked

Open a **new terminal tab** (keep your existing session alive as a safety net)
and confirm:

```bash
# 1. Key-based login should SUCCEED
ssh -i ~/.ssh/id_ed25519 root@<SERVER_IP>

# 2. Password login should be REJECTED immediately
ssh -o PasswordAuthentication=yes -o PubkeyAuthentication=no root@<SERVER_IP>
# Expected output: "Permission denied (publickey)."

# 3. Verify the effective daemon config (read the live values)
ssh -i ~/.ssh/id_ed25519 root@<SERVER_IP> "sudo sshd -T | grep -E 'passwordauthentication|pubkeyauthentication|permitrootlogin|maxauthtries'"
```
