# Live test: FerryTest WSL distro

End-to-end test procedure for ferry against a real Linux receiver. The test
spins up a throwaway WSL2 distro called `FerryTest` on a Windows host, runs
ferry there, and uploads from a Mac client through an SSH tunnel.

This document captures what actually worked on the run from `2026-05-08`.
The "warts" section at the bottom records what didn't work, so the next person
can skip the dead ends.

## Layout

- **Mac (client)**: runs `ferry upload` against a tunneled localhost.
- **Windows host**: runs WSL2 with default OpenSSH (PowerShell shell).
  Referred to below as `<ssh-host>`. The procedure assumes the host already
  has WSL2 enabled (so at least one existing distro is fine - `FerryTest`
  is created and destroyed in isolation and won't disturb others).
- **FerryTest WSL distro**: throwaway. Created, used, destroyed.

In all command snippets below, substitute:

- `<ssh-host>` - your SSH alias for the Windows host (e.g. `<windows-host>`).
- `<TOKEN>` - the bearer token printed during step 5.
- `<UPLOAD_ID>` - the upload id printed by `ferry upload` on completion.

## Prerequisites

- SSH access to the Windows host (`ssh <ssh-host>`).
- The host has WSL2 with ~1 GiB free disk.
- A current ferry binary set on the Mac. Build with both:
  ```sh
  ./scripts/build.sh    # produces dist/ferry for the local platform (Mac client)
  ./scripts/cross.sh    # produces dist/ferry-linux-amd64 (FerryTest receiver)
  ```

## Procedure

### 1. Pre-flight check

```sh
ssh <ssh-host> 'wsl -l -v'
```

Confirm `FerryTest` is **not** listed. The output is UTF-16; expect garbled
spacing. Just eyeball it.

### 2. Download an Ubuntu rootfs onto the host

The Ubuntu cloud-images team renames their tarballs every release. The
canonical name for jammy at the time of this run was
`ubuntu-jammy-wsl-amd64-ubuntu22.04lts.rootfs.tar.gz` (NOT
`ubuntu-jammy-wsl-amd64-wsl.rootfs.tar.gz` - that 404s).

```sh
ssh <ssh-host> 'New-Item -ItemType Directory C:\Temp -Force | Out-Null; New-Item -ItemType Directory C:\WSL -Force | Out-Null; $ProgressPreference="SilentlyContinue"; Invoke-WebRequest -Uri "https://cloud-images.ubuntu.com/wsl/jammy/current/ubuntu-jammy-wsl-amd64-ubuntu22.04lts.rootfs.tar.gz" -OutFile C:\Temp\ferry-ubuntu.tar.gz; Get-Item C:\Temp\ferry-ubuntu.tar.gz | Select Name,Length'
```

Note on `Invoke-WebRequest`: this run used Windows PowerShell 5.1, where
`-UseBasicParsing` was sometimes needed to avoid IE-engine initialization
errors. PowerShell 7+ ignores or rejects that flag (it was removed because
basic parsing became the default), so the snippet above drops it. If you
hit IE-engine errors on a 5.1-only host, add `-UseBasicParsing` back in.

Expect `~341 MB`. If it's smaller, the download was truncated - delete and
re-run. (See "Warts" below for why this happens.)

### 3. Import as `FerryTest`

```sh
ssh <ssh-host> 'wsl --import FerryTest C:\WSL\FerryTest C:\Temp\ferry-ubuntu.tar.gz --version 2; wsl -l -v'
```

Should report `The operation completed successfully` and `FerryTest` should
appear `Stopped`.

### 4. Install ferry binary into FerryTest

The interop bind-mount (`/mnt/c`) is enabled by default for new distros, so
the simplest path is: scp the binary to `C:\Temp` on the Windows host, then
copy it into FerryTest from `/mnt/c/Temp`.

```sh
scp dist/ferry-linux-amd64 <ssh-host>:C:/Temp/ferry-linux-amd64
ssh <ssh-host> 'wsl -d FerryTest -u root -- bash -c "cp /mnt/c/Temp/ferry-linux-amd64 /usr/local/bin/ferry && chmod +x /usr/local/bin/ferry && /usr/local/bin/ferry --help"'
```

### 5. Configure ferry on FerryTest

Inline `bash -c "..."` over SSH-PowerShell-WSL gets eaten by quoting (the
shell on the Windows host is PowerShell, which re-parses everything before
WSL hands it to bash). Use the **base64 script-upload pattern** for any
non-trivial command:

1. Author the bash script locally.
2. Base64-encode it on the Mac (`base64 < script.sh | tr -d '\n'`).
3. Decode + write it as a real file on the Windows host using
   `[System.IO.File]::WriteAllBytes(path, [System.Convert]::FromBase64String(b64))`,
   which sidesteps every layer of quoting.
4. Execute the script via `wsl -d FerryTest -u root -- bash /mnt/c/Temp/script.sh`.

```sh
cat > /tmp/ferrytest-install.sh <<'BASH'
#!/usr/bin/env bash
set -euxo pipefail
mkdir -p /etc/ferry /var/lib/ferry/data
TOKEN=$(openssl rand -hex 32)
TOKEN_HASH=$(printf '%s' "$TOKEN" | sha256sum | awk '{print $1}')
cat > /etc/ferry/config.json <<JSON
{
  "listen_addr": "0.0.0.0:7421",
  "data_dir": "/var/lib/ferry/data",
  "tokens_path": "/etc/ferry/tokens.json",
  "completed_retention_seconds": 86400,
  "incomplete_retention_seconds": 604800,
  "max_patch_bytes": 67108864,
  "disk_safety_margin_bytes": 1073741824
}
JSON
cat > /etc/ferry/tokens.json <<JSON
{ "tokens": { "$TOKEN_HASH": { "namespaces": ["livetest"] } } }
JSON
echo -n "$TOKEN" > /root/ferry-token.txt
echo "TOKEN_FILE=/root/ferry-token.txt"
BASH
B64=$(base64 < /tmp/ferrytest-install.sh | tr -d '\n')
ssh <ssh-host> "[System.IO.File]::WriteAllBytes('C:\\Temp\\ferrytest-install.sh', [System.Convert]::FromBase64String('$B64'))"
ssh <ssh-host> 'wsl -d FerryTest -u root -- bash /mnt/c/Temp/ferrytest-install.sh'
```

Read the token back with:

```sh
ssh <ssh-host> 'wsl -d FerryTest -u root -- cat /root/ferry-token.txt'
```

### 6. Start ferry under a keepalive SSH session

WSL kills processes the moment its session daemon thinks the distro is idle.
`nohup` does NOT survive across separate `wsl --exec` calls; you need a
session that stays open. Solution: a sleep-loop wrapper that the SSH process
keeps alive.

```sh
cat > /tmp/ferrytest-keepalive.sh <<'BASH'
#!/usr/bin/env bash
set -e
pkill -f '/usr/local/bin/ferry listen' || true
sleep 1
/usr/local/bin/ferry listen --config /etc/ferry/config.json >>/var/log/ferry.log 2>&1 &
echo "ferry pid=$!"
exec sleep 86400
BASH
B64=$(base64 < /tmp/ferrytest-keepalive.sh | tr -d '\n')
ssh <ssh-host> "[System.IO.File]::WriteAllBytes('C:\\Temp\\ferrytest-keepalive.sh', [System.Convert]::FromBase64String('$B64'))"

# Background SSH session that holds the distro running. Don't close until teardown.
ssh <ssh-host> 'wsl -d FerryTest -u root -- bash /mnt/c/Temp/ferrytest-keepalive.sh' &
KEEPALIVE_PID=$!
```

### 7. Open an SSH tunnel from Mac

WSL2 mirrors localhost by default - port 7421 inside FerryTest is also reachable
on `127.0.0.1:7421` of the Windows host. So a single SSH `-L` is enough:

```sh
ssh -N -L 17421:127.0.0.1:7421 <windows-host> &
TUNNEL_PID=$!
sleep 3
curl -sS http://127.0.0.1:17421/health   # {"ok":true,"version":"0.0.1"}
```

Targeting the distro's WSL2 internal address (`hostname -I` inside FerryTest)
directly from the Windows host does NOT work - Windows can't route into the
distro that way without a `netsh portproxy` rule. Use the host's loopback;
WSL2 plumbs it for you.

### 8. Generate a test file on the Mac

```sh
dd if=/dev/urandom of=/tmp/ferry-livetest-50.bin bs=1m count=50
shasum -a 256 /tmp/ferry-livetest-50.bin > /tmp/ferry-livetest-50.bin.sha256
```

50 MiB is the recommended size for a sanity run because the SSH-tunnel
throughput is the bottleneck (~500 KiB/s in this run). A 200 MiB attempt
hit a single-PATCH read-timeout at ~85% in this setup; the 50 MiB run
completes cleanly and exercises the same code path. If you have a faster
direct path to the host (Tailscale, host firewall rule), bump the count
back up to 200.

### 9. Upload + resume test

The CLI requires positional file argument **after** flags:

```sh
TOKEN=$(ssh <ssh-host> 'wsl -d FerryTest -u root -- cat /root/ferry-token.txt')
./dist/ferry upload \
    --to http://127.0.0.1:17421 \
    --as livetest-50.bin \
    --namespace livetest \
    --token "$TOKEN" \
    --idempotency-key livetest-50-v1 \
    /tmp/ferry-livetest-50.bin
```

To exercise resume, kill the process mid-flight (`Ctrl-C` or `kill -INT
<pid>`), then re-run the same command. Same `--idempotency-key` plus the
on-disk `.partial` lets the server return the existing upload's URL on POST,
the client HEADs to learn the offset, and the upload picks up where it left
off.

Real run from `2026-05-08` (50 MiB file):

- Run 1: killed at 30s, reached `40%` (20 MiB delivered to client; server held
  ~22 MiB after read-ahead).
- Run 2 (resume): started PATCH at 64% (32 MiB), completed at `34.9s`. Total
  wall-clock: ~65s (vs. ~95s for an uninterrupted upload at this throughput).

### 10. Verify

```sh
# Status from Mac
./dist/ferry status \
    --to http://127.0.0.1:17421 \
    --upload <id-from-upload-output> \
    --namespace livetest --token "$TOKEN" --json
# {"offset":52428800,"percent":100,"size":52428800,"state":"complete", ...}

# Checksum on FerryTest
ssh <ssh-host> 'wsl -d FerryTest -u root -- bash -c "sha256sum /var/lib/ferry/data/livetest/<id>"'

# Mac side
cat /tmp/ferry-livetest-50.bin.sha256
```

Hashes from the run:

```
8def5b39d124e5e583d2fd934b666efdcfc39b363da1f60662499855bfebfe25  /tmp/ferry-livetest-50.bin
8def5b39d124e5e583d2fd934b666efdcfc39b363da1f60662499855bfebfe25  /var/lib/ferry/data/livetest/01KR31WC44Z94MP18MB8VG660G
```

Match.

### 11. Teardown

```sh
# 1. Kill the SSH tunnel and keepalive
kill "$TUNNEL_PID" "$KEEPALIVE_PID" 2>/dev/null
pkill -f "ssh -N -L 17421" 2>/dev/null
pkill -f "wsl -d FerryTest" 2>/dev/null

# 2. Stop and unregister the distro
ssh <ssh-host> 'wsl --terminate FerryTest; wsl --unregister FerryTest'

# 3. Clean up host files
ssh <ssh-host> 'Remove-Item C:\WSL\FerryTest -Recurse -Force -ErrorAction SilentlyContinue; Remove-Item C:\Temp\ferry-ubuntu.tar.gz -Force -ErrorAction SilentlyContinue; Remove-Item C:\Temp\ferrytest-*.sh,C:\Temp\check-*.sh,C:\Temp\ferry-linux-amd64 -Force -ErrorAction SilentlyContinue'

# 4. Confirm
ssh <ssh-host> 'wsl -l -v'    # FerryTest should be gone
```

```sh
# Mac
rm -f /tmp/ferry-livetest*.bin /tmp/ferry-livetest*.bin.sha256 /tmp/ferry-up*.log /tmp/ferrytest-*.sh /tmp/check-*.sh
```

## Warts (real failures from this run)

- **Wrong tarball URL**: `ubuntu-jammy-wsl-amd64-wsl.rootfs.tar.gz` 404s. The
  current name is `ubuntu-jammy-wsl-amd64-ubuntu22.04lts.rootfs.tar.gz`. List
  the directory first if unsure.

- **Truncated download surviving**: a backgrounded `Invoke-WebRequest` left a
  stale 255 MB partial when interrupted, but `wsl --import` happily started
  reading it. The import failed mid-way with `Truncated tar archive`. Always
  verify the file size matches the upstream `Content-Length`. Fix: kill the
  hung PowerShell PID and re-download from scratch (passing
  `$ProgressPreference="SilentlyContinue"` makes the foreground download
  finish faster).

- **No outbound network from FerryTest**: on this run the host had other
  long-running distros with extensive Docker bridge networking, and the new
  distro could ICMP `1.1.1.1` but TCP/HTTPS to github.com (and 1.1.1.1)
  timed out. Likely a NAT or firewall interaction with the existing
  bridges. Workaround: don't `git clone` from inside FerryTest. Build the
  binary on the Mac, scp to the host, copy in via `/mnt/c/Temp`.

- **`nohup` doesn't persist across `wsl -d --` calls**: each `wsl --exec`
  call gets its own session, and when SSH closes the session, WSL's vmcompute
  reaper SIGKILLs everything. The fix is a keepalive SSH session running
  `exec sleep 86400` so the WSL session stays alive for the whole test.

- **Reaching FerryTest from the Windows host**: curling the distro's WSL2
  internal IP from the Windows host **times out**. Use
  `curl http://127.0.0.1:7421/` instead - WSL2's localhost forwarding
  handles the routing.

- **CLI argument order**: `ferry upload` requires the positional `<file>`
  arg AFTER all flags. `ferry upload <file> --to ...` errors with
  "expected exactly one file argument" because the flag parser swallows the
  filename.

- **SSH tunnel throughput was the bottleneck**: ~500 KiB/s through
  Mac→SSH→Windows→WSL-loopback→ferry. The 200 MiB upload timed out at 85%
  during a single PATCH; the 50 MiB run completed without incident. For
  future deeper-throughput tests, a direct path (Tailscale, port-forward
  rule on the host's firewall) would beat the SSH tunnel.
