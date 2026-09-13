# LocalChat

A small two-person chat for devices on the same Wi-Fi network. Go serves the website and coordinates the two peers.

The app prefers a direct WebRTC data channel when both browser origins allow it. Browsers block WebRTC on ordinary, non-secure LAN origins such as `http://192.168.x.x`, so the app automatically falls back to an ephemeral relay through the local Go process in that common setup. The UI always shows **Direct WebRTC** or **Local Go relay** so it never misrepresents the connection.

## Run on Windows

Open PowerShell in this folder and run:

```powershell
go run .
```

If `go` is not on PATH:

```powershell
& 'C:\Program Files\Go\bin\go.exe' run .
```

On the host computer, open:

```text
http://localhost:8080
```

On the second device, while connected to the same Wi-Fi, open the host computer's IPv4 address. For example:

```text
http://192.168.100.4:8080
```

The exact IPv4 address can be found with:

```powershell
(Get-NetIPConfiguration | Where-Object { $_.IPv4DefaultGateway -ne $null }).IPv4Address.IPAddress
```

If Windows asks whether to allow network access, allow Go on **Private networks**. Do not allow it on public networks. You can choose another port with `go run . -addr :9090`.

## Use

1. Person one enters a name and selects **Create a room**.
2. Share the six-character code or invite link.
3. Person two opens the site on the same Wi-Fi, enters a name and the code, then selects **Join**.
4. When the connection is ready, both people can chat. The footer shows whether the transport is direct WebRTC or the local Go relay.

Rooms are limited to two peers, kept only in memory, and expire after 12 hours. Nothing is written to a database or disk. In relay mode, recent messages remain in the server's in-memory event buffer for up to five minutes (and at most 256 signaling/message events) so they can be delivered.

## Test and build

```powershell
go test ./...
go build -o localchat.exe .
```

## Network notes

- This is local-network software, not an internet-hosted service.
- Some guest Wi-Fi networks enable client isolation, which prevents devices from reaching one another.
- The room code is an invitation, not strong authentication. Only share it with the intended person.
- No public STUN or TURN service is configured. WebRTC setup and fallback message delivery remain on the local Go server.
