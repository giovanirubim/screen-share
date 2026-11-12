# screen-share

A single executable that streams your screen to anyone with a browser.

## Use

1. Run `screen-share.exe`. It opens `http://localhost:8080/broadcast` in your browser.
2. Click **Start sharing** and pick a screen, window, or tab.
3. Send viewers the link marked **permanent** (e.g. `http://[2804:...]:8080/`).

Flags: `-port 8080`, `-udp-port 8080`, `-max-viewers 0`, `-no-browser`.

## Network requirements

- Open **TCP 8080** (web page) and **UDP 8080** (video):
  - Windows Firewall: allow the prompt on first run. Allow both Private and Public networks.
  - Your router: many ISP routers block incoming IPv6 by default. Allow inbound
    TCP+UDP 8080 to this PC's IPv6 address, or turn off the router's IPv6 firewall.
- Viewers need IPv6 on their side. Most mobile data plans have it.
  Check at https://test-ipv6.com.
- Each viewer gets the full stream from your PC, so upload speed ÷ bitrate ≈ max viewers.

## How it works

Your browser captures the screen (`getDisplayMedia`) and sends it via WebRTC
to the server (Go + [pion](https://github.com/pion/webrtc)). The server forwards
the same VP8/Opus packets to each viewer over one UDP port. Only localhost can broadcast.

## Build

With Go installed:

```
go build -ldflags "-s -w" -o screen-share.exe .
```

Without Go (PowerShell, from the repo folder; Go is downloaded into `.go\` and nothing is installed):

```powershell
$v = (Invoke-RestMethod 'https://go.dev/dl/?mode=json')[0].version
Invoke-WebRequest "https://go.dev/dl/$v.windows-amd64.zip" -OutFile go.zip
tar -xf go.zip; Rename-Item go .go; Remove-Item go.zip
.\.go\bin\go.exe build -ldflags "-s -w" -o screen-share.exe .
```

Run tests with `go test ./...`.
